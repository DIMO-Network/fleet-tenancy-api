package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"os"
	"sort"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/google/subcommands"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

// backfillCmd seeds the tenancy service from the two systems it replaces.
//
// WHAT COMES FROM WHERE, and why it is not symmetric:
//
//   - Tenant rows and credentials come from kaufmann-oracle ONLY. Its tenants
//     are the operators: they hold the DIMO developer license, the signer
//     keypair and the Kore credentials. fleet-lite's Kaufmann row is the same
//     company with the same dimo_client_id, so its API key is the same secret —
//     copying it too would duplicate a credential and violate the unique index
//     on tenant_credentials.dimo_client_id.
//
//   - fleet-lite tenants with NO kaufmann counterpart migrate as SELF-SERVE
//     tenants: kind='customer', no parent, their own credentials, implicit
//     entitlements. They signed up through fleet-lite's own onboarding with
//     their own developer license, and the design has always carried them —
//     dropping them would lock out real fleets (52 vehicles across four tenants
//     as of 2026-08-05, one of them logged in that day).
//
//   - Users and memberships come from BOTH. fleet-lite's tenant_users are the
//     customer's own staff and have no counterpart in access_tenants; dropping
//     them would lock every fleet-lite user out on cutover.
//
//   - Invitations come from fleet-lite only; kaufmann has no invitation flow.
//
// Credentials are decrypted with each source's key and re-encrypted with this
// service's, because the three keys differ. GCM authenticates, so a decrypt that
// succeeds proves the key was right.
//
// Idempotent: everything upserts, so a re-run converges rather than duplicating.
type backfillCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	dryRun bool
}

func (*backfillCmd) Name() string { return "backfill" }
func (*backfillCmd) Synopsis() string {
	return "seed tenants, users and memberships from the source systems"
}
func (*backfillCmd) Usage() string {
	return `backfill [-dry-run]:
	Seeds this service from kaufmann-oracle and fleet-lite-app.

	Connection details come from the environment so secrets stay off the command
	line and out of shell history:

	  BACKFILL_KAUFMANN_DSN       postgres://... (kaufmann_oracle)
	  BACKFILL_KAUFMANN_ENC_KEY   its TENANT_SECRET_ENC_KEY
	  BACKFILL_FLEETLITE_DSN      postgres://...?search_path=fleets_lite

	fleet-lite puts its tables in a schema named after its database, which differs
	by environment (fleets_lite in prod, fleet_lite_app locally), so its DSN must
	carry search_path. kaufmann's schema name is fixed and is qualified inline.
	  BACKFILL_FLEETLITE_ENC_KEY  its TENANT_SECRET_ENC_KEY

	-dry-run reports what would be written, including any fleet-lite tenant with
	no counterpart here — whose members would be locked out on cutover. Nothing
	is written unless every credential in scope decrypts cleanly.
  `
}

func (p *backfillCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&p.dryRun, "dry-run", false, "report only; write nothing")
}

type srcTenant struct {
	id, name      string
	dimoClientID  sql.NullString
	dimoSecretEnc sql.NullString
	signerAddress sql.NullString
	signerKeyEnc  sql.NullString
}

func (p *backfillCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	kDSN, kKey := os.Getenv("BACKFILL_KAUFMANN_DSN"), os.Getenv("BACKFILL_KAUFMANN_ENC_KEY")
	fDSN, fKey := os.Getenv("BACKFILL_FLEETLITE_DSN"), os.Getenv("BACKFILL_FLEETLITE_ENC_KEY")
	if kDSN == "" || fDSN == "" {
		p.logger.Error().Msg("BACKFILL_KAUFMANN_DSN and BACKFILL_FLEETLITE_DSN are required")
		return subcommands.ExitUsageError
	}
	if p.settings.TenantSecretEncKey == "" {
		p.logger.Error().Msg("TENANT_SECRET_ENC_KEY is empty — refusing to write credentials under sha256(\"\")")
		return subcommands.ExitFailure
	}

	kdb, err := sql.Open("postgres", kDSN)
	if err != nil {
		p.logger.Err(err).Msg("open kaufmann")
		return subcommands.ExitFailure
	}
	defer func() { _ = kdb.Close() }()
	fdb, err := sql.Open("postgres", fDSN)
	if err != nil {
		p.logger.Err(err).Msg("open fleet-lite")
		return subcommands.ExitFailure
	}
	defer func() { _ = fdb.Close() }()

	store := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	store.WaitForDB(p.logger)
	target := store.DBS().Writer

	// ---- Pass 1: read and verify everything before writing anything ----

	tenants, err := readKaufmannTenants(ctx, kdb)
	if err != nil {
		p.logger.Err(err).Msg("read kaufmann tenants")
		return subcommands.ExitFailure
	}

	// Re-encrypt up front so a bad key aborts before any write.
	type reKeyed struct {
		t         srcTenant
		apiKeyEnc string
		signerEnc string
	}
	prepared := make([]reKeyed, 0, len(tenants))
	for _, t := range tenants {
		r := reKeyed{t: t}
		var cerr error
		if r.apiKeyEnc, cerr = recrypt(kKey, p.settings.TenantSecretEncKey, t.dimoSecretEnc); cerr != nil {
			p.logger.Error().Str("tenant", t.name).Err(cerr).
				Msg("could not decrypt DIMO secret with the kaufmann key — aborting, nothing written")
			return subcommands.ExitFailure
		}
		if r.signerEnc, cerr = recrypt(kKey, p.settings.TenantSecretEncKey, t.signerKeyEnc); cerr != nil {
			p.logger.Error().Str("tenant", t.name).Err(cerr).
				Msg("could not decrypt signer key — aborting, nothing written")
			return subcommands.ExitFailure
		}
		prepared = append(prepared, r)
	}

	// The check that matters: fleet-lite tenants with no counterpart. Their
	// members would silently lose access on cutover, so surface them loudly
	// rather than letting the backfill quietly skip.
	stranded, err := strandedFleetLiteTenants(ctx, fdb, tenants)
	if err != nil {
		p.logger.Err(err).Msg("check fleet-lite tenants")
		return subcommands.ExitFailure
	}
	// Prepare the self-serve tenants: same verify-before-write discipline, using
	// fleet-lite's encryption key.
	preparedSelfServe := make([]reKeyed, 0, len(stranded))
	for _, sv := range stranded {
		r := reKeyed{t: srcTenant{id: sv.id, name: sv.name,
			dimoClientID: sv.dimoClientID, dimoSecretEnc: sv.dimoSecretEnc}}
		var cerr error
		if r.apiKeyEnc, cerr = recrypt(fKey, p.settings.TenantSecretEncKey, sv.dimoSecretEnc); cerr != nil {
			p.logger.Error().Str("tenant", sv.name).Err(cerr).
				Msg("could not decrypt fleet-lite DIMO secret — aborting, nothing written")
			return subcommands.ExitFailure
		}
		preparedSelfServe = append(preparedSelfServe, r)
		p.logger.Info().Str("tenant_id", sv.id).Str("name", sv.name).
			Msg("fleet-lite tenant with no kaufmann counterpart — migrating as self-serve")
	}

	// tenant_credentials.dimo_client_id is uniquely indexed. Two tenants sharing
	// a client id would fail mid-write, so catch it here instead.
	seenClient := map[string]string{}
	for _, r := range append(append([]reKeyed{}, prepared...), preparedSelfServe...) {
		if !r.t.dimoClientID.Valid || r.t.dimoClientID.String == "" {
			continue
		}
		k := strings.ToLower(r.t.dimoClientID.String)
		if other, dup := seenClient[k]; dup {
			p.logger.Error().Str("tenant", r.t.name).Str("conflicts_with", other).
				Msg("two tenants share a DIMO client id — refusing, one credential cannot belong to two tenants")
			return subcommands.ExitFailure
		}
		seenClient[k] = r.t.name
	}

	p.logger.Info().
		Int("kaufmann_tenants", len(tenants)).
		Int("selfserve_fleetlite_tenants", len(preparedSelfServe)).
		Bool("dry_run", p.dryRun).
		Msg("verification complete")

	if p.dryRun {
		return subcommands.ExitSuccess
	}

	// ---- Pass 2: write, one transaction ----
	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		p.logger.Err(err).Msg("begin")
		return subcommands.ExitFailure
	}
	defer func() { _ = tx.Rollback() }()

	for _, r := range prepared {
		// kaufmann's tenants are the operators — they hold the developer license.
		// The uuid is reused verbatim so no foreign key in either source app has
		// to be re-keyed and Tenant-Id keeps working.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenants (id, name, kind, entitlement_mode, fleet_lite_enabled)
			VALUES ($1, $2, 'operator', 'implicit', TRUE)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, updated_at = NOW()`,
			r.t.id, r.t.name); err != nil {
			p.logger.Err(err).Str("tenant", r.t.name).Msg("upsert tenant")
			return subcommands.ExitFailure
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_credentials (tenant_id, dimo_client_id, dimo_api_key_enc, signer_address, signer_key_enc)
			VALUES ($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,''))
			ON CONFLICT (tenant_id) DO UPDATE SET
				dimo_client_id = EXCLUDED.dimo_client_id,
				dimo_api_key_enc = EXCLUDED.dimo_api_key_enc,
				signer_address = EXCLUDED.signer_address,
				signer_key_enc = EXCLUDED.signer_key_enc,
				updated_at = NOW()`,
			r.t.id, r.t.dimoClientID.String, r.apiKeyEnc, r.t.signerAddress.String, r.signerEnc); err != nil {
			p.logger.Err(err).Str("tenant", r.t.name).Msg("upsert credentials")
			return subcommands.ExitFailure
		}
	}

	for _, r := range preparedSelfServe {
		// No parent, own credentials, implicit entitlements: their fleet is
		// whatever their own developer license is privileged on, exactly as
		// fleet-lite works for them today.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenants (id, name, kind, entitlement_mode, fleet_lite_enabled)
			VALUES ($1, $2, 'customer', 'implicit', TRUE)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, updated_at = NOW()`,
			r.t.id, r.t.name); err != nil {
			p.logger.Err(err).Str("tenant", r.t.name).Msg("upsert self-serve tenant")
			return subcommands.ExitFailure
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_credentials (tenant_id, dimo_client_id, dimo_api_key_enc)
			VALUES ($1, NULLIF($2,''), NULLIF($3,''))
			ON CONFLICT (tenant_id) DO UPDATE SET
				dimo_client_id = EXCLUDED.dimo_client_id,
				dimo_api_key_enc = EXCLUDED.dimo_api_key_enc,
				updated_at = NOW()`,
			r.t.id, r.t.dimoClientID.String, r.apiKeyEnc); err != nil {
			p.logger.Err(err).Str("tenant", r.t.name).Msg("upsert self-serve credentials")
			return subcommands.ExitFailure
		}
	}

	// Read both sources, merge, then write once. A membership can exist in both
	// — the Kaufmann tenant does, now that the two systems share one uuid — and
	// whichever source wrote last would otherwise silently overwrite the other.
	// That demoted a real kaufmann admin to role=member with no capabilities.
	acc := map[memberKey]*memberAccess{}
	kRows, err := readKaufmannMembers(ctx, kdb, acc)
	if err != nil {
		p.logger.Err(err).Msg("read kaufmann members")
		return subcommands.ExitFailure
	}
	fRows, err := readFleetLiteMembers(ctx, fdb, acc)
	if err != nil {
		p.logger.Err(err).Msg("read fleet-lite members")
		return subcommands.ExitFailure
	}
	users, memberships, err := writeMembers(ctx, tx, acc)
	if err != nil {
		p.logger.Err(err).Msg("write memberships")
		return subcommands.ExitFailure
	}
	p.logger.Info().Int("kaufmann_rows", kRows).Int("fleetlite_rows", fRows).
		Int("merged_memberships", memberships).
		Int("overlapping", kRows+fRows-memberships).
		Msg("member sources merged")

	if err := tx.Commit(); err != nil {
		p.logger.Err(err).Msg("commit")
		return subcommands.ExitFailure
	}

	p.logger.Info().
		Int("operator_tenants", len(prepared)).
		Int("selfserve_tenants", len(preparedSelfServe)).
		Int("users", users).
		Int("memberships", memberships).
		Msg("backfill complete")
	return subcommands.ExitSuccess
}

// recrypt decrypts with the source key and re-encrypts with the target key.
// Empty stays empty. A decrypt failure is fatal to the caller by design: GCM
// authenticates, so failure means the key is wrong and guessing would corrupt.
func recrypt(srcKey, dstKey string, v sql.NullString) (string, error) {
	if !v.Valid || v.String == "" {
		return "", nil
	}
	plain, err := service.DecryptSecret(srcKey, v.String)
	if err != nil {
		return "", err
	}
	return service.EncryptSecret(dstKey, plain)
}

func readKaufmannTenants(ctx context.Context, kdb *sql.DB) ([]srcTenant, error) {
	rows, err := kdb.QueryContext(ctx, `
		SELECT id, name, dimo_client_id, dimo_secret_enc, signer_address, signer_key_enc
		  FROM kaufmann_oracle.tenants ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []srcTenant
	for rows.Next() {
		var t srcTenant
		if err := rows.Scan(&t.id, &t.name, &t.dimoClientID, &t.dimoSecretEnc,
			&t.signerAddress, &t.signerKeyEnc); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type fleetLiteTenant struct {
	id, name      string
	dimoClientID  sql.NullString
	dimoSecretEnc sql.NullString
}

// strandedFleetLiteTenants returns fleet-lite tenants whose uuid does not appear
// among the kaufmann tenants being migrated. After the uuid-unification
// migration the Kaufmann tenant matches; anything still listed here is a tenant
// nobody has accounted for.
func strandedFleetLiteTenants(ctx context.Context, fdb *sql.DB, kt []srcTenant) ([]fleetLiteTenant, error) {
	known := map[string]bool{}
	for _, t := range kt {
		known[t.id] = true
	}
	rows, err := fdb.QueryContext(ctx,
		`SELECT id, name, dimo_client_id, dimo_api_key_enc FROM tenants ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []fleetLiteTenant
	for rows.Next() {
		var t fleetLiteTenant
		if err := rows.Scan(&t.id, &t.name, &t.dimoClientID, &t.dimoSecretEnc); err != nil {
			return nil, err
		}
		if !known[t.id] {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func upsertUser(ctx context.Context, tx *sql.Tx, wallet string, email sql.NullString) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO users (wallet, email) VALUES ($1, NULLIF($2,''))
		ON CONFLICT (wallet) DO UPDATE SET
			email = COALESCE(EXCLUDED.email, users.email), updated_at = NOW()`,
		common.HexToAddress(wallet).Hex(), email.String)
	return err
}

// mapKaufmannCapabilities translates kaufmann's stored permissions into the
// shared model's vocabulary. Two of its strings do not survive the move:
//
//   - manage_admin_users is simply renamed manage_members. Copying it verbatim
//     does not merely look untidy — every authorization check reads
//     `permissions`, so an operator admin carrying the old string would be
//     refused member management on cutover.
//
//   - view_all_fleets is deliberately absent from the shared model. It encodes
//     the same fact as "no group restriction", which lives in scope_group_ids,
//     and two homes for one fact have no defined resolution when they disagree.
//     It is dropped here and expressed as scope_group_ids = NULL.
func mapKaufmannCapabilities(perms []string) []string {
	out := make([]string, 0, len(perms))
	seen := map[string]bool{}
	for _, p := range perms {
		switch p {
		case "view_all_fleets":
			continue // becomes scope_group_ids = NULL, not a capability
		case "manage_admin_users":
			p = models.CapManageMembers
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// memberKey identifies one membership. Wallets are checksummed before use so
// the two sources, which disagree on casing, land on the same key.
type memberKey struct {
	tenantID string
	wallet   string
}

// memberAccess is one membership merged across every source that mentions it.
//
// Merging happens here, in memory, rather than through ON CONFLICT in the
// database. Doing it in SQL would make the result depend on which source
// happened to be written last, and — worse for a tool that is meant to be
// re-runnable — a union performed in the upsert would accumulate across runs
// and could never shed a capability that had been removed at the source. Built
// this way, a re-run converges on whatever the sources currently say.
type memberAccess struct {
	role         string
	perms        map[string]bool
	unrestricted bool // any source saying "all fleets" wins
	groups       map[string]bool
	email        sql.NullString
	lastLogin    sql.NullTime
}

// roleRank orders the display labels so a merge keeps the most privileged one.
// role is only a label — permissions is what authorization reads — but showing
// someone as a "member" while they hold owner capabilities is a confusing lie.
func roleRank(r string) int {
	switch r {
	case "owner":
		return 3
	case "admin":
		return 2
	default:
		return 1
	}
}

// merge folds one source's view of a membership into the accumulated one.
//
// The rule is "no access is removed by migrating", which is the same principle
// that keeps stranded fleet-lite tenants rather than dropping them. Capabilities
// union; scope takes the more permissive side, where unrestricted beats any set
// of groups and two group sets combine.
func (a *memberAccess) merge(role string, perms []string, unrestricted bool, groups []string,
	email sql.NullString, lastLogin sql.NullTime) {
	if roleRank(role) > roleRank(a.role) {
		a.role = role
	}
	if a.perms == nil {
		a.perms = map[string]bool{}
	}
	for _, p := range perms {
		a.perms[p] = true
	}
	if unrestricted {
		a.unrestricted = true
	}
	if a.groups == nil {
		a.groups = map[string]bool{}
	}
	for _, g := range groups {
		a.groups[g] = true
	}
	if !a.email.Valid && email.Valid {
		a.email = email
	}
	if lastLogin.Valid && (!a.lastLogin.Valid || lastLogin.Time.After(a.lastLogin.Time)) {
		a.lastLogin = lastLogin
	}
}

// permissionsJSON renders the merged capabilities, sorted so a re-run produces
// byte-identical rows and diffs stay readable.
func (a *memberAccess) permissionsJSON() (string, error) {
	out := make([]string, 0, len(a.perms))
	for p := range a.perms {
		out = append(out, p)
	}
	sort.Strings(out)
	b, err := json.Marshal(out)
	return string(b), err
}

// scopeArg renders scope_group_ids: nil for unrestricted, otherwise the sorted
// union. Note that "no groups and not unrestricted" is an empty array, not nil —
// nil would silently mean the opposite.
func (a *memberAccess) scopeArg() interface{} {
	if a.unrestricted {
		return nil
	}
	out := make([]string, 0, len(a.groups))
	for g := range a.groups {
		out = append(out, g)
	}
	sort.Strings(out)
	return pq.Array(out)
}

// readKaufmannMembers reads access_tenants, mapping capabilities and resolving
// group scope.
//
// access_fleet_groups is keyed by wallet alone, so it is joined through
// fleet_groups to pin each row to the tenant being read. Group ids became
// tenant-scoped in R1, so they drop straight into scope_group_ids.
func readKaufmannMembers(ctx context.Context, kdb *sql.DB, acc map[memberKey]*memberAccess) (int, error) {
	rows, err := kdb.QueryContext(ctx, `
		SELECT at.tenant_id, at.wallet, at.permissions::text, at.is_admin, up.email,
		       COALESCE(
		         (SELECT array_agg(g.fleet_group_id ORDER BY g.fleet_group_id)
		            FROM kaufmann_oracle.access_fleet_groups g
		            JOIN kaufmann_oracle.fleet_groups fg ON fg.id = g.fleet_group_id
		           WHERE g.wallet = at.wallet AND fg.tenant_id = at.tenant_id),
		         ARRAY[]::text[]) AS scope_group_ids
		  FROM kaufmann_oracle.access_tenants at
		  LEFT JOIN kaufmann_oracle.user_profiles up ON up.wallet = at.wallet`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	n := 0
	for rows.Next() {
		var tenantID, wallet, permsRaw string
		var isAdmin bool
		var email sql.NullString
		var scope pq.StringArray
		if err := rows.Scan(&tenantID, &wallet, &permsRaw, &isAdmin, &email, &scope); err != nil {
			return n, err
		}

		var srcPerms []string
		if uerr := json.Unmarshal([]byte(permsRaw), &srcPerms); uerr != nil {
			srcPerms = nil
		}
		unrestricted := false
		for _, p := range srcPerms {
			if p == "view_all_fleets" {
				unrestricted = true
				break
			}
		}
		role := "member"
		if isAdmin {
			role = "admin"
		}

		k := memberKey{tenantID: tenantID, wallet: common.HexToAddress(wallet).Hex()}
		if acc[k] == nil {
			acc[k] = &memberAccess{}
		}
		acc[k].merge(role, mapKaufmannCapabilities(srcPerms), unrestricted, []string(scope), email, sql.NullTime{})
		n++
	}
	return n, rows.Err()
}

// readFleetLiteMembers reads tenant_users. fleet-lite's only owner-gated
// operations are member management and tenant settings, so an owner maps to
// exactly those two capabilities. A NULL allowed_group_ids means unrestricted,
// matching this schema.
func readFleetLiteMembers(ctx context.Context, fdb *sql.DB, acc map[memberKey]*memberAccess) (int, error) {
	rows, err := fdb.QueryContext(ctx, `
		SELECT tenant_id, wallet, role, email, allowed_group_ids, last_login_at
		  FROM tenant_users`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	n := 0
	for rows.Next() {
		var tenantID, wallet, role string
		var email sql.NullString
		var allowed pq.StringArray
		var lastLogin sql.NullTime
		if err := rows.Scan(&tenantID, &wallet, &role, &email, &allowed, &lastLogin); err != nil {
			return n, err
		}

		var perms []string
		if role == "owner" {
			perms = []string{models.CapManageMembers, models.CapManageSettings}
		}
		// lib/pq scans a NULL text[] as a nil slice; nil means unrestricted.
		unrestricted := allowed == nil

		k := memberKey{tenantID: tenantID, wallet: common.HexToAddress(wallet).Hex()}
		if acc[k] == nil {
			acc[k] = &memberAccess{}
		}
		acc[k].merge(role, perms, unrestricted, []string(allowed), email, lastLogin)
		n++
	}
	return n, rows.Err()
}

// writeMembers persists the merged set. Users are written first so the
// membership rows have something to reference.
func writeMembers(ctx context.Context, tx *sql.Tx, acc map[memberKey]*memberAccess) (users, members int, err error) {
	// Deterministic order keeps a re-run's logs and lock ordering stable.
	keys := make([]memberKey, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tenantID != keys[j].tenantID {
			return keys[i].tenantID < keys[j].tenantID
		}
		return keys[i].wallet < keys[j].wallet
	})

	seenWallet := map[string]bool{}
	for _, k := range keys {
		a := acc[k]
		if !seenWallet[k.wallet] {
			if err := upsertUser(ctx, tx, k.wallet, a.email); err != nil {
				return users, members, err
			}
			seenWallet[k.wallet] = true
			users++
		}

		permsJSON, jerr := a.permissionsJSON()
		if jerr != nil {
			return users, members, jerr
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memberships (tenant_id, wallet, role, permissions, scope_group_ids, last_login_at)
			VALUES ($1, $2, $3, $4::jsonb, $5::text[], $6)
			ON CONFLICT (tenant_id, wallet) DO UPDATE SET
				role = EXCLUDED.role,
				permissions = EXCLUDED.permissions,
				scope_group_ids = EXCLUDED.scope_group_ids,
				last_login_at = GREATEST(memberships.last_login_at, EXCLUDED.last_login_at),
				updated_at = NOW()`,
			k.tenantID, k.wallet, a.role, permsJSON, a.scopeArg(), nullableTime(a.lastLogin)); err != nil {
			return users, members, err
		}
		members++
	}
	return users, members, nil
}

func nullableTime(v sql.NullTime) interface{} {
	if !v.Valid {
		return nil
	}
	return v.Time
}
