package service

import (
	"context"
	"errors"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	wtOperator  = "cccccccc-0000-0000-0000-000000000001"
	wtManaged   = "cccccccc-0000-0000-0000-000000000002" // child of operator, no license
	wtSuspended = "cccccccc-0000-0000-0000-000000000003" // child of operator, suspended
	wtHidden    = "cccccccc-0000-0000-0000-000000000004" // child of operator, fleet_lite_enabled=false
	wtForeign   = "cccccccc-0000-0000-0000-000000000005" // unparented, own license — outside operator scope

	wtWallet  = "0x4444444444444444444444444444444444444444"
	wtWallet2 = "0x5555555555555555555555555555555555555555"
)

func seedWalletTenants(t *testing.T, store *db.Store) {
	t.Helper()
	w := store.DBS().Writer
	ids := "{" + wtOperator + "," + wtManaged + "," + wtSuspended + "," + wtHidden + "," + wtForeign + "}"
	_, _ = w.Exec(`DELETE FROM memberships WHERE tenant_id = ANY($1)`, ids)
	_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = ANY($1)`, ids)
	_, _ = w.Exec(`DELETE FROM tenants WHERE id = ANY($1)`, ids)
	_, _ = w.Exec(`DELETE FROM users WHERE wallet = ANY($1)`, "{"+wtWallet+","+wtWallet2+"}")

	_, err := w.Exec(`INSERT INTO tenants (id,name,kind,entitlement_mode,status,fleet_lite_enabled) VALUES
		($1,'WT Operator','operator','implicit','active',TRUE),
		($2,'WT Managed','customer','explicit','active',TRUE),
		($3,'WT Suspended','customer','explicit','suspended',TRUE),
		($4,'WT Hidden','customer','explicit','active',FALSE),
		($5,'WT Foreign','customer','explicit','active',TRUE)`,
		wtOperator, wtManaged, wtSuspended, wtHidden, wtForeign)
	require.NoError(t, err)
	_, err = w.Exec(`UPDATE tenants SET parent_tenant_id=$1 WHERE id IN ($2,$3,$4)`,
		wtOperator, wtManaged, wtSuspended, wtHidden)
	require.NoError(t, err)

	// The foreign tenant holds its own license, so it is outside the operator
	// caller's scope even though the wallet is a member there.
	_, err = w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id) VALUES
		($1,'0xBBBB000000000000000000000000000000000001'),
		($2,'0xBBBB000000000000000000000000000000000005')`,
		wtOperator, wtForeign)
	require.NoError(t, err)

	_, err = w.Exec(`INSERT INTO users (wallet, email) VALUES ($1,'wt@example.com'),($2,NULL)`, wtWallet, wtWallet2)
	require.NoError(t, err)

	// One wallet, five memberships — every filter has a row to remove.
	for _, m := range []struct {
		tenant, role, perms string
	}{
		{wtOperator, "owner", `["manage_members","manage_settings"]`},
		{wtManaged, "admin", `["manage_members"]`},
		{wtSuspended, "member", `[]`},
		{wtHidden, "member", `[]`},
		{wtForeign, "member", `[]`},
	} {
		_, err = w.Exec(`INSERT INTO memberships (tenant_id,wallet,role,permissions)
			VALUES ($1,$2,$3,$4::jsonb)`, m.tenant, wtWallet, m.role, m.perms)
		require.NoError(t, err)
	}

	// A second wallet restricted to nothing, for the scope encoding.
	_, err = w.Exec(`INSERT INTO memberships (tenant_id,wallet,role,permissions,scope_group_ids)
		VALUES ($1,$2,'member','[]'::jsonb, ARRAY[]::text[])`, wtManaged, wtWallet2)
	require.NoError(t, err)
}

func TestListTenantsForWallet(t *testing.T) {
	store := testStore(t)
	seedWalletTenants(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	ctx := context.Background()

	asOperator := &models.CallerTenant{TenantID: wtOperator}
	asService := &models.CallerTenant{TenantID: wtForeign, IsService: true}

	names := func(rows []models.WalletTenant) []string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}

	t.Run("fleet_lite surface hides suspended and non-fleet-lite tenants", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asService, wtWallet, models.SurfaceFleetLite)
		require.NoError(t, err)
		assert.Equal(t, []string{"WT Foreign", "WT Managed", "WT Operator"}, names(rows))
	})

	// The scope rule mirrors CallerMayAccess: the foreign tenant's membership
	// exists, but its detail read would 403 for this caller, so the list must
	// not disclose it either.
	t.Run("an ordinary caller sees only tenants inside its scope", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asOperator, wtWallet, models.SurfaceFleetLite)
		require.NoError(t, err)
		assert.Equal(t, []string{"WT Managed", "WT Operator"}, names(rows))
	})

	t.Run("no surface returns every membership the caller may see", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asService, wtWallet, "")
		require.NoError(t, err)
		assert.Len(t, rows, 5)
	})

	t.Run("b2b surface returns operator tenants only", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asService, wtWallet, models.SurfaceB2B)
		require.NoError(t, err)
		assert.Equal(t, []string{"WT Operator"}, names(rows))
	})

	t.Run("membership fields ride along", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asOperator, wtWallet, models.SurfaceFleetLite)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		managed := rows[0]
		require.Equal(t, wtManaged, managed.TenantID)
		assert.Equal(t, "admin", managed.Role)
		assert.Equal(t, []string{"manage_members"}, managed.Permissions)
		assert.Equal(t, "explicit", managed.EntitlementMode)
		assert.Nil(t, managed.ScopeGroupIDs, "no scope_group_ids means unrestricted")
	})

	// The inversion that bit the backfill: [] must survive as [], not nil.
	t.Run("restricted-to-nothing stays an empty array, not nil", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asService, wtWallet2, models.SurfaceFleetLite)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.NotNil(t, rows[0].ScopeGroupIDs)
		assert.Empty(t, rows[0].ScopeGroupIDs)
	})

	// The two source systems disagree about wallet casing, so the lookup must
	// not care. wtWallet2's rows were inserted with the 0x5555… literal; ask
	// with a different hex casing of the same address.
	t.Run("wallet casing does not matter", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asService, "0X5555555555555555555555555555555555555555", "")
		require.NoError(t, err)
		assert.Len(t, rows, 1)
	})

	t.Run("a wallet in no tenant gets an empty list", func(t *testing.T) {
		rows, err := svc.ListTenantsForWallet(ctx, asService, "0x9999999999999999999999999999999999999999", "")
		require.NoError(t, err)
		assert.Empty(t, rows)
	})

	t.Run("an unknown surface is a caller error", func(t *testing.T) {
		_, err := svc.ListTenantsForWallet(ctx, asService, wtWallet, "mobile")
		assert.True(t, errors.Is(err, ErrInvalidSurface), "got %v", err)
	})

	t.Run("no caller means no answer", func(t *testing.T) {
		_, err := svc.ListTenantsForWallet(ctx, nil, wtWallet, "")
		assert.Error(t, err)
	})
}

func TestTouchLogin(t *testing.T) {
	store := testStore(t)
	seedWalletTenants(t, store)
	l := zerolog.Nop()
	svc := NewMemberService(&l, store)
	ctx := context.Background()

	t.Run("stamps last_login_at", func(t *testing.T) {
		found, err := svc.TouchLogin(ctx, wtManaged, wtWallet, "")
		require.NoError(t, err)
		assert.True(t, found)

		var stamped bool
		err = store.DBS().Reader.QueryRow(
			`SELECT last_login_at IS NOT NULL FROM memberships
			  WHERE tenant_id=$1 AND lower(wallet)=lower($2)`, wtManaged, wtWallet).Scan(&stamped)
		require.NoError(t, err)
		assert.True(t, stamped)
	})

	// Fill-if-empty: the login email adds knowledge where there was none and
	// never overwrites what provisioning recorded.
	t.Run("fills a missing email but never overwrites one", func(t *testing.T) {
		found, err := svc.TouchLogin(ctx, wtManaged, wtWallet2, "login2@example.com")
		require.NoError(t, err)
		assert.True(t, found)

		var email string
		require.NoError(t, store.DBS().Reader.QueryRow(
			`SELECT email FROM users WHERE lower(wallet)=lower($1)`, wtWallet2).Scan(&email))
		assert.Equal(t, "login2@example.com", email)

		_, err = svc.TouchLogin(ctx, wtManaged, wtWallet, "usurper@example.com")
		require.NoError(t, err)
		require.NoError(t, store.DBS().Reader.QueryRow(
			`SELECT email FROM users WHERE lower(wallet)=lower($1)`, wtWallet).Scan(&email))
		assert.Equal(t, "wt@example.com", email, "an existing email must survive a login")
	})

	t.Run("a wallet with no membership reports not found, not an error", func(t *testing.T) {
		found, err := svc.TouchLogin(ctx, wtManaged, "0x9999999999999999999999999999999999999999", "")
		require.NoError(t, err)
		assert.False(t, found)
	})
}
