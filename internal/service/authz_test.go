package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	_ "github.com/lib/pq"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These run against the local Postgres the team already uses for every project
// (brew services). Set FLEET_TENANCY_TEST_DSN to point elsewhere; skip if the
// database isn't reachable so CI without a database still passes.
func testStore(t *testing.T) *db.Store {
	t.Helper()
	settings := db.Settings{
		User: "dimo", Password: "dimo", Host: "localhost", Port: "5432",
		Name: "fleet_tenancy_api", SSLMode: "disable",
		MaxOpenConnections: 5, MaxIdleConnections: 2,
	}
	if v := os.Getenv("FLEET_TENANCY_TEST_HOST"); v != "" {
		settings.Host = v
	}
	// Probe with a plain connection first: WaitForDB fatals rather than
	// returning, so it can't drive a skip.
	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		settings.Host, settings.Port, settings.User, settings.Password, settings.Name, settings.SSLMode)
	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}
	defer func() { _ = probe.Close() }()
	if err := probe.Ping(); err != nil {
		t.Skipf("local postgres not reachable, skipping: %v", err)
	}

	store := db.NewDbConnectionFromSettings(context.Background(), &settings, true)
	store.WaitForDB(zerolog.Nop())
	return &store
}

const (
	opTenant   = "aaaaaaaa-0000-0000-0000-000000000001"
	custTenant = "aaaaaaaa-0000-0000-0000-000000000002"
	otherTen   = "aaaaaaaa-0000-0000-0000-000000000003"
	opWallet   = "0x1111111111111111111111111111111111111111"
	custWallet = "0x2222222222222222222222222222222222222222"
	strangerW  = "0x3333333333333333333333333333333333333333"
)

func seed(t *testing.T, store *db.Store) {
	t.Helper()
	w := store.DBS().Writer
	// Clean slate for these fixture ids only — never touch other rows.
	for _, q := range []string{
		`DELETE FROM tenant_delegations WHERE operator_tenant_id=$1 OR customer_tenant_id=$1`,
		`DELETE FROM memberships WHERE tenant_id IN ($1)`,
	} {
		for _, id := range []string{opTenant, custTenant, otherTen} {
			_, _ = w.Exec(q, id)
		}
	}
	_, _ = w.Exec(`DELETE FROM users WHERE wallet = ANY($1)`,
		"{"+opWallet+","+custWallet+","+strangerW+"}")
	_, _ = w.Exec(`DELETE FROM tenants WHERE id = ANY($1)`,
		"{"+opTenant+","+custTenant+","+otherTen+"}")

	_, err := w.Exec(`INSERT INTO tenants (id,name,kind,entitlement_mode) VALUES
		($1,'Op','operator','implicit'), ($2,'Cust','customer','explicit'), ($3,'Other','customer','explicit')`,
		opTenant, custTenant, otherTen)
	require.NoError(t, err)
	_, err = w.Exec(`UPDATE tenants SET parent_tenant_id=$1 WHERE id IN ($2,$3)`, opTenant, custTenant, otherTen)
	require.NoError(t, err)

	_, err = w.Exec(`INSERT INTO users (wallet) VALUES ($1),($2),($3)`, opWallet, custWallet, strangerW)
	require.NoError(t, err)

	// Operator staff member, full capabilities.
	_, err = w.Exec(`INSERT INTO memberships (tenant_id,wallet,role,permissions)
		VALUES ($1,$2,'owner','["manage_members","manage_settings","onboard_vehicles"]'::jsonb)`,
		opTenant, opWallet)
	require.NoError(t, err)

	// Customer member, restricted to one group.
	_, err = w.Exec(`INSERT INTO memberships (tenant_id,wallet,role,permissions,scope_group_ids)
		VALUES ($1,$2,'member','[]'::jsonb, ARRAY['`+custTenant+`_vans'])`,
		custTenant, custWallet)
	require.NoError(t, err)

	// The operator may manage the customer — management scopes only.
	_, err = w.Exec(`INSERT INTO tenant_delegations (operator_tenant_id,customer_tenant_id,scopes)
		VALUES ($1,$2,ARRAY['manage_members','manage_vehicles','manage_settings'])`,
		opTenant, custTenant)
	require.NoError(t, err)
}

func TestAuthorize(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewAuthzService(&l, store)
	ctx := context.Background()

	t.Run("direct membership", func(t *testing.T) {
		got, err := svc.Authorize(ctx, opTenant, opWallet)
		require.NoError(t, err)
		assert.True(t, got.Member)
		assert.Equal(t, models.ViaDirect, got.Via)
		assert.Equal(t, "owner", got.Role)
		assert.True(t, got.HasCapability(models.CapManageMembers))
		assert.True(t, got.Unrestricted(), "no scope_group_ids means all groups")
	})

	t.Run("group-scoped member", func(t *testing.T) {
		got, err := svc.Authorize(ctx, custTenant, custWallet)
		require.NoError(t, err)
		assert.Equal(t, models.ViaDirect, got.Via)
		assert.False(t, got.Unrestricted())
		assert.Equal(t, []string{custTenant + "_vans"}, got.ScopeGroupIDs)
		assert.False(t, got.HasCapability(models.CapManageMembers), "a plain member manages nothing")
	})

	t.Run("delegation: operator reaches the customer", func(t *testing.T) {
		got, err := svc.Authorize(ctx, custTenant, opWallet)
		require.NoError(t, err)
		assert.Equal(t, models.ViaDelegation, got.Via)
		assert.False(t, got.Member, "delegated access is not membership")
		assert.Equal(t, opTenant, got.OperatorTenantID)
		assert.True(t, got.HasCapability(models.CapManageMembers))
	})

	t.Run("no delegation to an unrelated tenant", func(t *testing.T) {
		got, err := svc.Authorize(ctx, otherTen, opWallet)
		require.NoError(t, err)
		assert.Equal(t, models.ViaNone, got.Via)
		assert.False(t, got.Member)
		assert.Empty(t, got.Permissions)
	})

	t.Run("customer member cannot reach a sibling customer", func(t *testing.T) {
		got, err := svc.Authorize(ctx, otherTen, custWallet)
		require.NoError(t, err)
		assert.Equal(t, models.ViaNone, got.Via)
	})

	t.Run("stranger gets nothing", func(t *testing.T) {
		got, err := svc.Authorize(ctx, custTenant, strangerW)
		require.NoError(t, err)
		assert.Equal(t, models.ViaNone, got.Via)
	})

	t.Run("unknown tenant is no-access, not an error", func(t *testing.T) {
		got, err := svc.Authorize(ctx, "aaaaaaaa-0000-0000-0000-0000000000ff", opWallet)
		require.NoError(t, err)
		assert.Equal(t, models.ViaNone, got.Via)
	})

	t.Run("wallet casing does not matter", func(t *testing.T) {
		lower, err := svc.Authorize(ctx, opTenant, "0x1111111111111111111111111111111111111111")
		require.NoError(t, err)
		assert.True(t, lower.Member, "callers lowercase or checksum inconsistently; both must work")
	})
}
