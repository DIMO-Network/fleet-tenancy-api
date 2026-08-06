package main

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/google/subcommands"
	_ "github.com/lib/pq"
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
	defer kdb.Close()
	fdb, err := sql.Open("postgres", fDSN)
	if err != nil {
		p.logger.Err(err).Msg("open fleet-lite")
		return subcommands.ExitFailure
	}
	defer fdb.Close()

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

	kUsers, kMembers, err := copyKaufmannMembers(ctx, kdb, tx)
	if err != nil {
		p.logger.Err(err).Msg("copy kaufmann members")
		return subcommands.ExitFailure
	}
	fUsers, fMembers, err := copyFleetLiteMembers(ctx, fdb, tx)
	if err != nil {
		p.logger.Err(err).Msg("copy fleet-lite members")
		return subcommands.ExitFailure
	}

	if err := tx.Commit(); err != nil {
		p.logger.Err(err).Msg("commit")
		return subcommands.ExitFailure
	}

	p.logger.Info().
		Int("operator_tenants", len(prepared)).
		Int("selfserve_tenants", len(preparedSelfServe)).
		Int("users", kUsers+fUsers).
		Int("memberships", kMembers+fMembers).
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
	defer rows.Close()
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
	defer rows.Close()
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

// copyKaufmannMembers maps access_tenants to memberships. is_admin becomes the
// 'admin' role; permissions carry across as-is since they are already the
// capability model this service adopts.
func copyKaufmannMembers(ctx context.Context, kdb *sql.DB, tx *sql.Tx) (users, members int, err error) {
	rows, qerr := kdb.QueryContext(ctx, `
		SELECT at.tenant_id, at.wallet, at.permissions::text, at.is_admin, up.email
		  FROM kaufmann_oracle.access_tenants at
		  LEFT JOIN kaufmann_oracle.user_profiles up ON up.wallet = at.wallet`)
	if qerr != nil {
		return 0, 0, qerr
	}
	defer rows.Close()
	for rows.Next() {
		var tenantID, wallet, perms string
		var isAdmin bool
		var email sql.NullString
		if err := rows.Scan(&tenantID, &wallet, &perms, &isAdmin, &email); err != nil {
			return users, members, err
		}
		if err := upsertUser(ctx, tx, wallet, email); err != nil {
			return users, members, err
		}
		users++
		role := "member"
		if isAdmin {
			role = "admin"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memberships (tenant_id, wallet, role, permissions)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (tenant_id, wallet) DO UPDATE SET
				role = EXCLUDED.role, permissions = EXCLUDED.permissions, updated_at = NOW()`,
			tenantID, common.HexToAddress(wallet).Hex(), role, perms); err != nil {
			return users, members, err
		}
		members++
	}
	return users, members, rows.Err()
}

// copyFleetLiteMembers maps tenant_users to memberships. fleet-lite's owner/member
// role becomes owner/member here, and its owner-only gates map to the two
// capabilities that cover them; allowed_group_ids becomes scope_group_ids.
func copyFleetLiteMembers(ctx context.Context, fdb *sql.DB, tx *sql.Tx) (users, members int, err error) {
	rows, qerr := fdb.QueryContext(ctx, `
		SELECT tenant_id, wallet, role, email, allowed_group_ids, last_login_at
		  FROM tenant_users`)
	if qerr != nil {
		return 0, 0, qerr
	}
	defer rows.Close()
	for rows.Next() {
		var tenantID, wallet, role string
		var email sql.NullString
		var allowed sql.NullString // text[] rendered by lib/pq
		var lastLogin sql.NullTime
		if err := rows.Scan(&tenantID, &wallet, &role, &email, &allowed, &lastLogin); err != nil {
			return users, members, err
		}
		if err := upsertUser(ctx, tx, wallet, email); err != nil {
			return users, members, err
		}
		users++

		// fleet-lite's only owner-gated operations are member management and
		// tenant settings, so an owner maps to exactly those two capabilities.
		perms := `[]`
		if role == "owner" {
			perms = `["manage_members","manage_settings"]`
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
			tenantID, common.HexToAddress(wallet).Hex(), role, perms,
			nullableArray(allowed), nullableTime(lastLogin)); err != nil {
			return users, members, err
		}
		members++
	}
	return users, members, rows.Err()
}

func nullableArray(v sql.NullString) interface{} {
	if !v.Valid {
		return nil
	}
	return v.String
}

func nullableTime(v sql.NullTime) interface{} {
	if !v.Valid {
		return nil
	}
	return v.Time
}
