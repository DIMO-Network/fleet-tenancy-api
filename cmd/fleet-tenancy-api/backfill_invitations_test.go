package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The backfill's whole job is fidelity, and every field it must not mangle is
// a field some earlier migration in this programme did mangle. So this runs
// against a real database rather than asserting on a struct: the tri-state
// scope only round-trips through Postgres, and NULL-vs-{} is exactly the
// distinction that silently handed 131 memberships an entire fleet.
func backfillTestDB(t *testing.T) *sql.DB {
	t.Helper()
	host := "localhost"
	if v := os.Getenv("FLEET_TENANCY_TEST_HOST"); v != "" {
		host = v
	}
	// search_path matches what the service's own store sets — the tables live
	// in the fleet_tenancy_api schema, not public.
	dsn := fmt.Sprintf("host=%s port=5432 user=dimo password=dimo dbname=fleet_tenancy_api "+
		"sslmode=disable search_path=fleet_tenancy_api", host)
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

const (
	bfTenant = "cccccccc-0000-0000-0000-000000000001"
	// Lowercased on purpose: fleet-lite stores wallets this way and the
	// backfill must checksum them, or one person becomes two rows.
	bfInviter         = "0xabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	bfInviterChecksum = "0xABcdEFABcdEFabcdEfAbCdefabcdeFABcDEFabCD"
	bfInvitee         = "0xbbbbccccddddeeeeffff1111222233334444aaaa"
	bfInviteeChecksum = "0xBBBbCcCCDDdDEEeefFFf1111222233334444aAAA"
)

func seedBackfillTenant(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.Exec(`INSERT INTO tenants (id,name,kind,entitlement_mode)
		VALUES ($1,'Backfill Fixture','customer','explicit') ON CONFLICT (id) DO NOTHING`, bfTenant)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.Exec(`DELETE FROM invitations WHERE tenant_id = $1`, bfTenant)
		_, _ = conn.Exec(`DELETE FROM tenants WHERE id = $1`, bfTenant)
	})
}

func runWrite(t *testing.T, conn *sql.DB, src []srcInvitation) int {
	t.Helper()
	tx, err := conn.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	n, err := writeInvitations(context.Background(), tx, src)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return n
}

func TestBackfillInvitationsFidelity(t *testing.T) {
	conn := backfillTestDB(t)
	seedBackfillTenant(t, conn)

	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	rows := []srcInvitation{
		{
			id: "dddddddd-0000-0000-0000-000000000001", tenantID: bfTenant,
			email: "unrestricted@example.com", role: "owner",
			tokenHash: "hash-unrestricted", status: "pending",
			invitedByWallet: bfInviter,
			scopeGroupIDs:   nil, // fleet-lite NULL = unrestricted
			createdAt:       created, updatedAt: created, expiresAt: expires,
			postmarkMessageID: sql.NullString{String: "pm-aaa", Valid: true},
			emailStatus:       sql.NullString{String: "delivered", Valid: true},
			emailStatusAt:     sql.NullTime{Time: created, Valid: true},
		},
		{
			id: "dddddddd-0000-0000-0000-000000000002", tenantID: bfTenant,
			email: "nothing@example.com", role: "member",
			tokenHash: "hash-nothing", status: "pending",
			invitedByWallet: bfInviter,
			scopeGroupIDs:   pq.StringArray{}, // {} = restricted to nothing
			createdAt:       created, updatedAt: created, expiresAt: expires,
		},
		{
			id: "dddddddd-0000-0000-0000-000000000003", tenantID: bfTenant,
			email: "scoped@example.com", role: "member",
			tokenHash: "hash-scoped", status: "accepted",
			invitedByWallet: bfInviter,
			inviteeWallet:   sql.NullString{String: bfInvitee, Valid: true},
			scopeGroupIDs:   pq.StringArray{bfTenant + "_vans"},
			createdAt:       created, updatedAt: created, expiresAt: expires,
			acceptedAt: sql.NullTime{Time: created, Valid: true},
		},
	}

	assert.Equal(t, 3, runWrite(t, conn, rows))

	t.Run("ids, token hashes and expiries survive verbatim", func(t *testing.T) {
		// The outstanding-link guarantee: a link emailed before the cutover is
		// recognised after it only if the id and the hash are both unchanged.
		for _, want := range rows {
			var hash string
			var expiresAt time.Time
			require.NoError(t, conn.QueryRow(
				`SELECT token_hash, expires_at FROM invitations WHERE id = $1`, want.id).
				Scan(&hash, &expiresAt))
			assert.Equal(t, want.tokenHash, hash, "the hash is the only thing that can recognise an emailed token")
			assert.Equal(t, want.expiresAt.UTC(), expiresAt.UTC(),
				"re-deriving the expiry would silently move links already in inboxes")
		}
	})

	t.Run("the scope tri-state is preserved, NULL distinct from empty", func(t *testing.T) {
		var isNull bool
		require.NoError(t, conn.QueryRow(
			`SELECT scope_group_ids IS NULL FROM invitations WHERE id = $1`, rows[0].id).Scan(&isNull))
		assert.True(t, isNull, "NULL means unrestricted and must not become an empty array")

		var scope pq.StringArray
		require.NoError(t, conn.QueryRow(
			`SELECT scope_group_ids FROM invitations WHERE id = $1`, rows[1].id).Scan(&scope))
		assert.NotNil(t, scope, "{} means restricted to nothing and must not become NULL")
		assert.Empty(t, scope)

		require.NoError(t, conn.QueryRow(
			`SELECT scope_group_ids FROM invitations WHERE id = $1`, rows[2].id).Scan(&scope))
		assert.Equal(t, pq.StringArray{bfTenant + "_vans"}, scope)
	})

	t.Run("both webhook correlation keys survive", func(t *testing.T) {
		// Drop either and a bounce for a message sent before the cutover stops
		// resolving: the id rides in Postmark metadata, the message id is the
		// fallback.
		var msgID, status sql.NullString
		require.NoError(t, conn.QueryRow(
			`SELECT postmark_message_id, email_status FROM invitations WHERE id = $1`, rows[0].id).
			Scan(&msgID, &status))
		assert.Equal(t, "pm-aaa", msgID.String)
		assert.Equal(t, "delivered", status.String)
	})

	t.Run("wallets are checksummed so one person stays one row", func(t *testing.T) {
		var invitedBy string
		require.NoError(t, conn.QueryRow(
			`SELECT invited_by_wallet FROM invitations WHERE id = $1`, rows[0].id).Scan(&invitedBy))
		assert.Equal(t, bfInviterChecksum, invitedBy)
		assert.NotEqual(t, bfInviter, invitedBy, "fleet-lite's lowercase form must not survive as-is")
	})

	t.Run("status and acceptance carry over", func(t *testing.T) {
		var status string
		var acceptedAt sql.NullTime
		var invitee sql.NullString
		require.NoError(t, conn.QueryRow(
			`SELECT status, accepted_at, invitee_wallet FROM invitations WHERE id = $1`, rows[2].id).
			Scan(&status, &acceptedAt, &invitee))
		assert.Equal(t, "accepted", status)
		assert.True(t, acceptedAt.Valid)
		assert.Equal(t, bfInviteeChecksum, invitee.String)
	})
}

// A re-run must converge on whatever fleet-lite currently says — invitations
// can be revoked or resent in the window between the backfill and the flag
// flip — while never touching a row this service issued itself.
func TestBackfillInvitationsRerunConverges(t *testing.T) {
	conn := backfillTestDB(t)
	seedBackfillTenant(t, conn)

	created := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	expires := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	row := srcInvitation{
		id: "dddddddd-0000-0000-0000-000000000010", tenantID: bfTenant,
		email: "rerun@example.com", role: "member", tokenHash: "hash-original",
		status: "pending", invitedByWallet: bfInviter,
		createdAt: created, updatedAt: created, expiresAt: expires,
	}
	runWrite(t, conn, []srcInvitation{row})

	// An invitation this service issued — the console's, once P3 lands. Its
	// emailed link is live and nothing in a "converge on the source" pass may
	// destroy it.
	_, err := conn.Exec(`INSERT INTO invitations
		(id, tenant_id, email, role, token_hash, status, invited_by_wallet,
		 created_by_tenant_id, expires_at)
		VALUES ($1,$2,'console@example.com','member','hash-console','pending',$3,$2,NOW() + INTERVAL '7 days')`,
		"dddddddd-0000-0000-0000-000000000011", bfTenant, bfInviter)
	require.NoError(t, err)

	// fleet-lite resends (fresh hash, later expiry) and then revokes.
	row.tokenHash = "hash-resent"
	row.status = "revoked"
	row.expiresAt = expires.Add(48 * time.Hour)
	row.updatedAt = created.Add(time.Hour)
	runWrite(t, conn, []srcInvitation{row})

	var hash, status string
	var expiresAt time.Time
	require.NoError(t, conn.QueryRow(
		`SELECT token_hash, status, expires_at FROM invitations WHERE id = $1`, row.id).
		Scan(&hash, &status, &expiresAt))
	assert.Equal(t, "hash-resent", hash, "a resend in the window must land on the re-run")
	assert.Equal(t, "revoked", status)
	assert.Equal(t, row.expiresAt.UTC(), expiresAt.UTC())

	var consoleStatus string
	var consoleOwner sql.NullString
	require.NoError(t, conn.QueryRow(
		`SELECT status, created_by_tenant_id FROM invitations WHERE id = $1`,
		"dddddddd-0000-0000-0000-000000000011").Scan(&consoleStatus, &consoleOwner))
	assert.Equal(t, "pending", consoleStatus,
		"a row this service issued must survive a re-run untouched — deleting it would kill a live link")
	assert.Equal(t, bfTenant, consoleOwner.String,
		"and its operator attribution must not be cleared by the backfill's NULL")
}

// The unique index on token_hash makes this a mid-write abort; catching it up
// front names both records instead of leaving a constraint error to decode.
func TestBackfillInvitationsTokenHashConflictIsCaught(t *testing.T) {
	conn := backfillTestDB(t)
	seedBackfillTenant(t, conn)

	_, err := conn.Exec(`INSERT INTO invitations
		(id, tenant_id, email, role, token_hash, status, invited_by_wallet, expires_at)
		VALUES ($1,$2,'held@example.com','member','hash-shared','pending',$3,NOW() + INTERVAL '7 days')`,
		"dddddddd-0000-0000-0000-000000000020", bfTenant, bfInviter)
	require.NoError(t, err)

	cmd := &backfillInvitationsCmd{}
	err = cmd.checkTokenHashConflicts(context.Background(), conn, []srcInvitation{{
		id: "dddddddd-0000-0000-0000-000000000021", tenantID: bfTenant, tokenHash: "hash-shared",
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "two records would claim one emailed link")

	// The same id holding the same hash is the ordinary re-run case, not a
	// conflict.
	require.NoError(t, cmd.checkTokenHashConflicts(context.Background(), conn, []srcInvitation{{
		id: "dddddddd-0000-0000-0000-000000000020", tenantID: bfTenant, tokenHash: "hash-shared",
	}}))
}

func TestBackfillInvitationsRefusesUnknownTenant(t *testing.T) {
	conn := backfillTestDB(t)
	cmd := &backfillInvitationsCmd{}
	err := cmd.checkTenantsExist(context.Background(), conn, []srcInvitation{{
		id: "dddddddd-0000-0000-0000-000000000030", tenantID: "cccccccc-0000-0000-0000-0000000000ff",
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run backfill first")
}
