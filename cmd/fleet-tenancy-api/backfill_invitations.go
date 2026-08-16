package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/google/subcommands"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

// backfillInvitationsCmd copies fleet-lite's invitations into this service —
// P2 of docs/plans/04-invitations-into-tenancy.md.
//
// ONE source, unlike the groups backfill: kaufmann has never had invitations.
// So there is no merge, and the interesting work is fidelity instead.
//
// WHAT MUST SURVIVE, and why each is not optional:
//
//   - The ID. Rows are copied under their existing uuid, the same trick every
//     migration here has used, so fleet-lite's local row and this service's
//     row are the same record rather than two records about one invitation.
//     The diff depends on it and so does the cutover.
//
//   - The TOKEN HASH, byte for byte. An invitation link emailed last week must
//     still accept next week — the plaintext token exists only in that email,
//     and the hash is the only thing that can recognise it. This is the
//     outstanding-link guarantee, and it is the reason this backfill exists at
//     all rather than a "new invites go here" cutover.
//
//   - BOTH correlation keys — the id (which rides in Postmark message
//     metadata) and postmark_message_id. Drop either and a bounce for a
//     message sent before the cutover stops resolving to its row.
//
//   - expires_at verbatim. Re-deriving it from INVITE_EXPIRY_HOURS would
//     silently extend or shorten links already in people's inboxes.
//
// THE SCOPE COLUMN CHANGES NAME BUT NOT MEANING: fleet-lite's
// allowed_group_ids becomes scope_group_ids, and the tri-state is preserved
// exactly — NULL stays NULL (unrestricted), {} stays {} (restricted to
// nothing). That inversion is the one that handed 131 memberships an entire
// fleet during the tenant backfill; here it is a straight column copy
// precisely so nothing can re-interpret it.
//
// UPSERT, NOT REPLACE-WHOLESALE — deliberately different from backfill-groups.
// That command clears its tables so a re-run converges on the sources; doing
// the same here would delete any invitation this service issued itself (the
// console's, once P3 lands), and deleting an invitation destroys a live
// emailed link. fleet-lite never hard-deletes an invitation — revoke is a
// status change — so an id-keyed upsert converges just as completely without
// that risk. Rows here that the source does not have are REPORTED, never
// removed.
type backfillInvitationsCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	dryRun bool
}

func (*backfillInvitationsCmd) Name() string { return "backfill-invitations" }
func (*backfillInvitationsCmd) Synopsis() string {
	return "copy fleet-lite's invitations into this service, ids and token hashes intact"
}
func (*backfillInvitationsCmd) Usage() string {
	return `backfill-invitations [-dry-run]:
	Copies fleet-lite-app's invitations into this service (P2 of the
	invitations move), preserving ids, token hashes, expiries and email
	tracking so outstanding accept links keep working across the cutover.

	Connection details come from the environment, matching the other backfills:

	  BACKFILL_FLEETLITE_DSN   postgres://...?search_path=fleets_lite

	kaufmann is not read — it has never had invitations.

	Rows are upserted by id, so a re-run converges on whatever fleet-lite
	currently says. Run it again if invitations are created locally between
	this run and the INVITES_FROM_TENANCY flip.

	-dry-run reports what would be written, including any row whose tenant is
	unknown here and any token-hash conflict, writing nothing.
  `
}

func (p *backfillInvitationsCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&p.dryRun, "dry-run", false, "report only; write nothing")
}

// srcInvitation is one invitation row as fleet-lite holds it. Nullable columns
// stay nullable all the way to the write so NULL-vs-empty is never collapsed.
type srcInvitation struct {
	id                string
	tenantID          string
	email             string
	role              string
	tokenHash         string
	status            string
	invitedByWallet   string
	inviteeWallet     sql.NullString
	scopeGroupIDs     pq.StringArray // nil = unrestricted, {} = restricted to nothing
	postmarkMessageID sql.NullString
	emailStatus       sql.NullString
	emailStatusAt     sql.NullTime
	emailStatusDetail sql.NullString
	createdAt         time.Time
	updatedAt         time.Time
	expiresAt         time.Time
	acceptedAt        sql.NullTime
}

func (p *backfillInvitationsCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	fDSN := os.Getenv("BACKFILL_FLEETLITE_DSN")
	if fDSN == "" {
		p.logger.Error().Msg("BACKFILL_FLEETLITE_DSN is required")
		return subcommands.ExitUsageError
	}

	fdb, err := sql.Open("postgres", fDSN)
	if err != nil {
		p.logger.Err(err).Msg("open fleet-lite")
		return subcommands.ExitFailure
	}
	defer func() { _ = fdb.Close() }()

	store := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	store.WaitForDB(p.logger)
	target := store.DBS().Writer.DB

	// ---- Pass 1: read and verify everything before writing anything ----

	src, err := readInvitations(ctx, fdb)
	if err != nil {
		p.logger.Err(err).Msg("read fleet-lite invitations")
		return subcommands.ExitFailure
	}

	if err := p.checkTenantsExist(ctx, target, src); err != nil {
		p.logger.Err(err).Msg("tenant check — aborting, nothing written")
		return subcommands.ExitFailure
	}
	if err := p.checkTokenHashConflicts(ctx, target, src); err != nil {
		p.logger.Err(err).Msg("token hash conflict — aborting, nothing written")
		return subcommands.ExitFailure
	}
	extra, err := p.reportLocalOnly(ctx, target, src)
	if err != nil {
		p.logger.Err(err).Msg("local-only check failed")
		return subcommands.ExitFailure
	}

	byStatus := map[string]int{}
	pendingLive := 0
	for _, inv := range src {
		byStatus[inv.status]++
		if inv.status == "pending" && inv.expiresAt.After(time.Now()) {
			pendingLive++
		}
	}
	p.logger.Info().
		Int("source_invitations", len(src)).
		Int("pending", byStatus["pending"]).
		Int("accepted", byStatus["accepted"]).
		Int("revoked", byStatus["revoked"]).
		Int("pending_and_unexpired", pendingLive).
		Int("already_here_not_in_source", extra).
		Bool("dry_run", p.dryRun).
		Msg("verification complete")
	// The live links are what the whole exercise protects. Say so out loud, so
	// a run whose count is unexpectedly zero gets noticed rather than passing
	// as "clean".
	p.logger.Info().Int("count", pendingLive).
		Msg("pending, unexpired invitations whose emailed links must keep working after the cutover")

	if p.dryRun {
		return subcommands.ExitSuccess
	}

	// ---- Pass 2: write, one transaction, upsert by id ----

	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		p.logger.Err(err).Msg("begin")
		return subcommands.ExitFailure
	}
	defer func() { _ = tx.Rollback() }()

	written, err := writeInvitations(ctx, tx, src)
	if err != nil {
		p.logger.Err(err).Msg("upsert invitations")
		return subcommands.ExitFailure
	}

	if err := tx.Commit(); err != nil {
		p.logger.Err(err).Msg("commit")
		return subcommands.ExitFailure
	}

	// The counter carries an invariant that can fail — three times this
	// programme has been bitten by a counter that measured the wrong thing.
	if written != len(src) {
		p.logger.Error().Int("written", written).Int("source", len(src)).
			Msg("written count does not reconcile with the source count")
		return subcommands.ExitFailure
	}
	p.logger.Info().Int("invitations", written).Msg("invitation backfill complete")
	return subcommands.ExitSuccess
}

// writeInvitations upserts every source row by id and reports how many it
// wrote. Split out from Execute so the fidelity that matters — the tri-state
// scope, the token hash, the expiry, the wallet normalisation — is testable
// against a real database rather than only exercised in production.
//
// created_by_tenant_id is absent from both the insert and the update on
// purpose: every invitation fleet-lite holds was sent by the tenant itself, so
// it stays NULL here, and a re-run must not clear the column on a row the
// console later issued.
func writeInvitations(ctx context.Context, tx *sql.Tx, src []srcInvitation) (int, error) {
	written := 0
	for _, inv := range src {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO invitations
			  (id, tenant_id, email, role, token_hash, status, invited_by_wallet,
			   invitee_wallet, scope_group_ids, postmark_message_id, email_status,
			   email_status_at, email_status_detail, created_at, updated_at,
			   expires_at, accepted_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			ON CONFLICT (id) DO UPDATE SET
			  email               = EXCLUDED.email,
			  role                = EXCLUDED.role,
			  token_hash          = EXCLUDED.token_hash,
			  status              = EXCLUDED.status,
			  invited_by_wallet   = EXCLUDED.invited_by_wallet,
			  invitee_wallet      = EXCLUDED.invitee_wallet,
			  scope_group_ids     = EXCLUDED.scope_group_ids,
			  postmark_message_id = EXCLUDED.postmark_message_id,
			  email_status        = EXCLUDED.email_status,
			  email_status_at     = EXCLUDED.email_status_at,
			  email_status_detail = EXCLUDED.email_status_detail,
			  updated_at          = EXCLUDED.updated_at,
			  expires_at          = EXCLUDED.expires_at,
			  accepted_at         = EXCLUDED.accepted_at`,
			inv.id, inv.tenantID, inv.email, inv.role, inv.tokenHash, inv.status,
			checksumWallet(inv.invitedByWallet), checksumNullWallet(inv.inviteeWallet),
			inv.scopeGroupIDs, inv.postmarkMessageID, inv.emailStatus,
			inv.emailStatusAt, inv.emailStatusDetail, inv.createdAt, inv.updatedAt,
			inv.expiresAt, inv.acceptedAt); err != nil {
			return written, fmt.Errorf("upsert invitation %s: %w", inv.id, err)
		}
		written++
	}
	return written, nil
}

// checkTenantsExist verifies every tenant the invitations name has a row here.
// A missing one is an FK error mid-write otherwise, which is a worse way to
// learn the tenant backfill has not covered it.
func (p *backfillInvitationsCmd) checkTenantsExist(ctx context.Context, target *sql.DB, src []srcInvitation) error {
	want := map[string]bool{}
	for _, inv := range src {
		want[inv.tenantID] = true
	}
	rows, err := target.QueryContext(ctx, `SELECT id FROM tenants`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		delete(want, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for id := range want {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return fmt.Errorf("invitations reference %d tenant(s) with no row here — run backfill first: %v",
			len(missing), missing)
	}
	return nil
}

// checkTokenHashConflicts finds a token hash already held here under a
// DIFFERENT id. The unique index would abort the transaction mid-write; more
// importantly it would mean two records claim one emailed link, which is a
// question a migration must refuse to answer by itself.
func (p *backfillInvitationsCmd) checkTokenHashConflicts(ctx context.Context, target *sql.DB, src []srcInvitation) error {
	byHash := map[string]string{}
	for _, inv := range src {
		byHash[inv.tokenHash] = inv.id
	}
	rows, err := target.QueryContext(ctx, `SELECT id, token_hash FROM invitations`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return err
		}
		if srcID, ok := byHash[hash]; ok && srcID != id {
			return fmt.Errorf("token hash held here by invitation %s belongs to %s in fleet-lite — "+
				"two records would claim one emailed link", id, srcID)
		}
	}
	return rows.Err()
}

// reportLocalOnly names invitations that exist here but not in fleet-lite, and
// returns how many there were. They are never deleted: before the cutover this
// should be zero, and after it these are exactly the console's own invitations,
// whose emailed links a "converge on the source" delete would destroy.
func (p *backfillInvitationsCmd) reportLocalOnly(ctx context.Context, target *sql.DB, src []srcInvitation) (int, error) {
	have := map[string]bool{}
	for _, inv := range src {
		have[inv.id] = true
	}
	rows, err := target.QueryContext(ctx,
		`SELECT id, tenant_id, email, status, created_by_tenant_id IS NOT NULL FROM invitations`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id, tenantID, email, status string
		var operatorSent bool
		if err := rows.Scan(&id, &tenantID, &email, &status, &operatorSent); err != nil {
			return 0, err
		}
		if have[id] {
			continue
		}
		n++
		p.logger.Info().Str("invitation", id).Str("tenant_id", tenantID).Str("email", email).
			Str("status", status).Bool("operator_sent", operatorSent).
			Msg("invitation exists here but not in fleet-lite — left alone, never deleted")
	}
	return n, rows.Err()
}

// checksumWallet normalises to EIP-55, matching what InvitationService writes:
// fleet-lite lowercases, this service checksums, and one person must not end
// up looking like two. invitations-diff compares wallets case-insensitively
// for the same reason.
func checksumWallet(w string) string {
	if w == "" {
		return ""
	}
	return common.HexToAddress(w).Hex()
}

func checksumNullWallet(w sql.NullString) sql.NullString {
	if !w.Valid || w.String == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: common.HexToAddress(w.String).Hex(), Valid: true}
}

func readInvitations(ctx context.Context, src *sql.DB) ([]srcInvitation, error) {
	rows, err := src.QueryContext(ctx, `
		SELECT id, tenant_id, email, role, token_hash, status, invited_by_wallet,
		       invitee_wallet, allowed_group_ids, postmark_message_id, email_status,
		       email_status_at, email_status_detail, created_at, updated_at,
		       expires_at, accepted_at
		  FROM invitations
		 ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []srcInvitation
	for rows.Next() {
		var inv srcInvitation
		if err := rows.Scan(&inv.id, &inv.tenantID, &inv.email, &inv.role, &inv.tokenHash,
			&inv.status, &inv.invitedByWallet, &inv.inviteeWallet, &inv.scopeGroupIDs,
			&inv.postmarkMessageID, &inv.emailStatus, &inv.emailStatusAt,
			&inv.emailStatusDetail, &inv.createdAt, &inv.updatedAt,
			&inv.expiresAt, &inv.acceptedAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}
