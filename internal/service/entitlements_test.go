package service

import (
	"context"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Token ids used only by these tests, well clear of anything real.
const (
	tokenA int64 = 900001
	tokenB int64 = 900002
	tokenC int64 = 900003
)

func entitlementFixture(t *testing.T) (*EntitlementService, *TenantService, context.Context) {
	t.Helper()
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewEntitlementService(&l, store)
	tenants := NewTenantService(&l, store)

	t.Cleanup(func() {
		_, _ = store.DBS().Writer.Exec(
			`DELETE FROM vehicle_entitlements WHERE vehicle_token_id = ANY($1)`,
			"{900001,900002,900003}")
	})
	return svc, tenants, context.Background()
}

func TestAssignAndRevoke(t *testing.T) {
	svc, _, ctx := entitlementFixture(t)

	t.Run("assigns and lists", func(t *testing.T) {
		res, err := svc.Assign(ctx, custTenant, &models.AssignVehiclesInput{
			TokenIDs: []int64{tokenA, tokenB},
		}, opWallet)
		require.NoError(t, err)
		assert.ElementsMatch(t, []int64{tokenA, tokenB}, res.Assigned)
		assert.Empty(t, res.Rejected)

		list, err := svc.List(ctx, custTenant)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Equal(t, "operator", list[0].Source)
	})

	t.Run("group provenance is recorded, which is what makes drift knowable", func(t *testing.T) {
		res, err := svc.Assign(ctx, custTenant, &models.AssignVehiclesInput{
			TokenIDs: []int64{tokenC}, FromGroupID: "grp-vans",
		}, opWallet)
		require.NoError(t, err)
		require.Len(t, res.Assigned, 1)

		list, err := svc.List(ctx, custTenant)
		require.NoError(t, err)
		for _, e := range list {
			if e.VehicleTokenID == tokenC {
				require.NotNil(t, e.SourceGroupID)
				assert.Equal(t, "grp-vans", *e.SourceGroupID)
			}
		}
	})

	t.Run("re-assigning what the tenant already holds is a no-op, not a conflict", func(t *testing.T) {
		res, err := svc.Assign(ctx, custTenant, &models.AssignVehiclesInput{
			TokenIDs: []int64{tokenA},
		}, opWallet)
		require.NoError(t, err)
		assert.Empty(t, res.Rejected, "the caller asked for a state that already holds")
	})

	t.Run("revoking removes it from the list", func(t *testing.T) {
		require.NoError(t, svc.Revoke(ctx, custTenant, tokenB))

		list, err := svc.List(ctx, custTenant)
		require.NoError(t, err)
		for _, e := range list {
			assert.NotEqual(t, tokenB, e.VehicleTokenID)
		}
	})

	t.Run("revoking twice reports not found", func(t *testing.T) {
		require.ErrorIs(t, svc.Revoke(ctx, custTenant, tokenB), ErrEntitlementNotFound)
	})

	t.Run("a revoked vehicle can be assigned again", func(t *testing.T) {
		res, err := svc.Assign(ctx, custTenant, &models.AssignVehiclesInput{
			TokenIDs: []int64{tokenB},
		}, opWallet)
		require.NoError(t, err)
		assert.Equal(t, []int64{tokenB}, res.Assigned, "revocation is history, not a tombstone")
	})
}

// The exclusivity invariant, which is the thing keeping one customer's vehicles
// away from another under the same operator.
func TestExclusivityInvariant(t *testing.T) {
	svc, _, ctx := entitlementFixture(t)

	_, err := svc.Assign(ctx, custTenant, &models.AssignVehiclesInput{
		TokenIDs: []int64{tokenA},
	}, opWallet)
	require.NoError(t, err)

	t.Run("a second customer cannot take it, and is told who has it", func(t *testing.T) {
		res, err := svc.Assign(ctx, otherTen, &models.AssignVehiclesInput{
			TokenIDs: []int64{tokenA},
		}, opWallet)
		require.NoError(t, err, "a conflict is a partial success, not a failed request")
		assert.Empty(t, res.Assigned)
		require.Len(t, res.Rejected, 1)
		assert.Equal(t, tokenA, res.Rejected[0].TokenID)
		assert.Equal(t, "Cust", res.Rejected[0].HeldBy, "the console needs to say who has it")
	})

	// The case the whole partial-success shape exists for.
	t.Run("the rest of a batch still goes through", func(t *testing.T) {
		res, err := svc.Assign(ctx, otherTen, &models.AssignVehiclesInput{
			TokenIDs: []int64{tokenA, tokenB, tokenC},
		}, opWallet)
		require.NoError(t, err)
		assert.ElementsMatch(t, []int64{tokenB, tokenC}, res.Assigned,
			"one contested vehicle must not sink the other thirty-nine")
		require.Len(t, res.Rejected, 1)
		assert.Equal(t, tokenA, res.Rejected[0].TokenID)
	})

	t.Run("revoking frees it for the other customer", func(t *testing.T) {
		require.NoError(t, svc.Revoke(ctx, custTenant, tokenA))

		res, err := svc.Assign(ctx, otherTen, &models.AssignVehiclesInput{
			TokenIDs: []int64{tokenA},
		}, opWallet)
		require.NoError(t, err)
		assert.Equal(t, []int64{tokenA}, res.Assigned)
	})

	// The database has the final say, because the service check reads before it
	// writes and a race can slip between the two.
	t.Run("the unique index refuses a duplicate the service check would miss", func(t *testing.T) {
		_, err := svc.pdb.DBS().Writer.Exec(
			`INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source)
			 VALUES ($1, $2, 'operator')`, custTenant, tokenA)
		require.Error(t, err, "one active holder per vehicle, enforced by the schema")
		assert.True(t, isUniqueViolation(err))
	})
}

func TestAssignRefusesImplicitModeTenant(t *testing.T) {
	svc, _, ctx := entitlementFixture(t)

	// The operator resolves its fleet from its licence's privileged set, so an
	// entitlement row for it would be a row nothing reads.
	_, err := svc.Assign(ctx, opTenant, &models.AssignVehiclesInput{
		TokenIDs: []int64{tokenA},
	}, opWallet)
	require.ErrorIs(t, err, ErrNotExplicitMode)
}

func TestAssertEntitled(t *testing.T) {
	svc, _, ctx := entitlementFixture(t)

	_, err := svc.Assign(ctx, custTenant, &models.AssignVehiclesInput{
		TokenIDs: []int64{tokenA},
	}, opWallet)
	require.NoError(t, err)

	t.Run("true for the holder", func(t *testing.T) {
		ok, err := svc.AssertEntitled(ctx, custTenant, tokenA)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	// The leak this choke point exists to prevent.
	t.Run("false for a sibling customer under the same operator", func(t *testing.T) {
		ok, err := svc.AssertEntitled(ctx, otherTen, tokenA)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("false once revoked", func(t *testing.T) {
		require.NoError(t, svc.Revoke(ctx, custTenant, tokenA))
		ok, err := svc.AssertEntitled(ctx, custTenant, tokenA)
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

// Cross-tenant isolation, stated as a property rather than as one example.
//
// The design calls this test load-bearing rather than defensive: under D2/D5
// every customer's data is reachable with the operator's developer JWT, so this
// is the mechanism, and a table-driven check over the operations means a new
// one is covered by default rather than by somebody remembering.
func TestCrossTenantIsolation(t *testing.T) {
	svc, tenants, ctx := entitlementFixture(t)

	_, err := svc.Assign(ctx, custTenant, &models.AssignVehiclesInput{
		TokenIDs: []int64{tokenA, tokenB},
	}, opWallet)
	require.NoError(t, err)

	ops := []struct {
		name string
		run  func() (leaked bool, err error)
	}{
		{
			name: "listing another customer's vehicles returns none of them",
			run: func() (bool, error) {
				list, err := svc.List(ctx, otherTen)
				for _, e := range list {
					if e.VehicleTokenID == tokenA || e.VehicleTokenID == tokenB {
						return true, err
					}
				}
				return false, err
			},
		},
		{
			name: "asserting entitlement across customers is false",
			run: func() (bool, error) {
				ok, err := svc.AssertEntitled(ctx, otherTen, tokenA)
				return ok, err
			},
		},
		{
			name: "revoking another customer's vehicle does not touch it",
			run: func() (bool, error) {
				err := svc.Revoke(ctx, otherTen, tokenA)
				if err != nil && err != ErrEntitlementNotFound {
					return false, err
				}
				ok, aerr := svc.AssertEntitled(ctx, custTenant, tokenA)
				return !ok, aerr // leaked if the real holder lost it
			},
		},
		{
			name: "a member list does not spill across tenants",
			run: func() (bool, error) {
				members, err := tenants.ListMembers(ctx, otherTen)
				for _, m := range members {
					if m.Wallet == custWallet {
						return true, err
					}
				}
				return false, err
			},
		},
	}

	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			leaked, err := op.run()
			require.NoError(t, err)
			assert.False(t, leaked, "one customer must never reach another's data")
		})
	}
}
