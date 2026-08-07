package service

import (
	"context"
	"errors"
	"testing"

	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	resolveTenant = "aaaaaaaa-0000-0000-0000-0000000000a1"
	resolveClient = "0xAbCdEf0000000000000000000000000000000001"
	noCredTenant  = "aaaaaaaa-0000-0000-0000-0000000000a2"
)

func seedResolve(t *testing.T, store *db.Store) {
	t.Helper()
	w := store.DBS().Writer
	_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = ANY($1)`,
		"{"+resolveTenant+","+noCredTenant+"}")
	_, _ = w.Exec(`DELETE FROM tenants WHERE id = ANY($1)`,
		"{"+resolveTenant+","+noCredTenant+"}")

	_, err := w.Exec(`INSERT INTO tenants (id,name,kind,entitlement_mode,fleet_lite_enabled) VALUES
		($1,'ResolveCo','operator','implicit',TRUE),
		($2,'NoCreds','customer','explicit',FALSE)`, resolveTenant, noCredTenant)
	require.NoError(t, err)

	_, err = w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id) VALUES ($1,$2)`,
		resolveTenant, resolveClient)
	require.NoError(t, err)
}

func TestResolveByClientID(t *testing.T) {
	store := testStore(t)
	seedResolve(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	ctx := context.Background()

	t.Run("resolves a registered client id", func(t *testing.T) {
		ref, err := svc.ResolveByClientID(ctx, resolveClient)
		require.NoError(t, err)
		assert.Equal(t, resolveTenant, ref.TenantID)
		assert.Equal(t, "ResolveCo", ref.Name)
		assert.Equal(t, "operator", ref.Kind)
		assert.Equal(t, "implicit", ref.EntitlementMode)
		assert.True(t, ref.FleetLiteEnabled)
		assert.Nil(t, ref.ParentTenantID, "an operator has no parent")
	})

	// The unique index is on lower(dimo_client_id), so the lookup must match
	// that expression or it will miss rows the constraint considers identical.
	// Ethereum addresses arrive checksummed from some callers and lowercased
	// from others; both must resolve.
	t.Run("is case-insensitive in both directions", func(t *testing.T) {
		for _, variant := range []string{
			"0xabcdef0000000000000000000000000000000001",
			"0xABCDEF0000000000000000000000000000000001",
			resolveClient,
		} {
			ref, err := svc.ResolveByClientID(ctx, variant)
			require.NoError(t, err, "variant %s", variant)
			assert.Equal(t, resolveTenant, ref.TenantID, "variant %s", variant)
		}
	})

	t.Run("an unknown client id is ErrTenantNotFound", func(t *testing.T) {
		_, err := svc.ResolveByClientID(ctx, "0x000000000000000000000000000000000000dead")
		assert.True(t, errors.Is(err, ErrTenantNotFound), "got %v", err)
	})

	// A tenant with no credentials row must not resolve. Otherwise a caller
	// presenting any license could be mapped onto a tenant that never
	// registered one.
	t.Run("a tenant without credentials does not resolve", func(t *testing.T) {
		_, err := svc.ResolveByClientID(ctx, noCredTenant)
		assert.True(t, errors.Is(err, ErrTenantNotFound), "got %v", err)
	})

	t.Run("empty client id is rejected before querying", func(t *testing.T) {
		_, err := svc.ResolveByClientID(ctx, "")
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrTenantNotFound),
			"an empty id is a caller mistake, not a missing tenant")
	})
}
