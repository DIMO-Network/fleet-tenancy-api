package service

import (
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The P5b integrity rules live in the database, not the service layer, because
// every writer — this service, a backfill, a psql session — must be bound by
// them equally. These tests exercise the triggers and the FK through plain SQL
// for the same reason.
func TestGroupReferenceIntegrity(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	w := store.DBS().Writer

	mustExec := func(q string, args ...any) {
		t.Helper()
		_, err := w.Exec(q, args...)
		require.NoError(t, err)
	}
	cleanup := func() {
		_, _ = w.Exec(`DELETE FROM memberships WHERE tenant_id = $1 AND wallet = $2`, custTenant, custWallet)
		_, _ = w.Exec(`DELETE FROM fleet_groups WHERE tenant_id IN ($1, $2)`, custTenant, opTenant)
		_, _ = w.Exec(`DELETE FROM vehicle_entitlements WHERE tenant_id = $1`, custTenant)
	}
	cleanup()
	t.Cleanup(cleanup)

	mustExec(`INSERT INTO fleet_groups (id, tenant_id, name, color) VALUES ($1, $2, 'Vans', '#112233')`,
		custTenant+"_vans", custTenant)
	mustExec(`INSERT INTO fleet_groups (id, tenant_id, name, color) VALUES ($1, $2, 'Pool', '#445566')`,
		opTenant+"_pool", opTenant)

	t.Run("a scope naming a nonexistent group is refused", func(t *testing.T) {
		_, err := w.Exec(`
			INSERT INTO memberships (tenant_id, wallet, scope_group_ids)
			VALUES ($1, $2, $3)`, custTenant, custWallet, pq.Array([]string{custTenant + "_ghost"}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "_ghost")
	})

	t.Run("a scope naming another tenant's group is refused", func(t *testing.T) {
		// The group exists — in the operator's tenant. Scope is meaningful only
		// within the membership's own tenant, so this is the cross-tenant leak
		// the trigger exists to stop.
		_, err := w.Exec(`
			INSERT INTO memberships (tenant_id, wallet, scope_group_ids)
			VALUES ($1, $2, $3)`, custTenant, custWallet, pq.Array([]string{opTenant + "_pool"}))
		require.Error(t, err)
	})

	t.Run("a valid scope writes, and deleting the group strips it to empty, not NULL", func(t *testing.T) {
		mustExec(`
			INSERT INTO memberships (tenant_id, wallet, scope_group_ids)
			VALUES ($1, $2, $3)`, custTenant, custWallet, pq.Array([]string{custTenant + "_vans"}))

		mustExec(`DELETE FROM fleet_groups WHERE id = $1`, custTenant+"_vans")

		var scope pq.StringArray
		var isNull bool
		require.NoError(t, w.QueryRow(`
			SELECT COALESCE(scope_group_ids, '{}'), scope_group_ids IS NULL
			  FROM memberships WHERE tenant_id = $1 AND wallet = $2`,
			custTenant, custWallet).Scan(&scope, &isNull))

		// NULL means unrestricted. A member whose only group was deleted must
		// end scoped to NOTHING — collapsing {} to NULL would escalate them to
		// seeing the whole fleet as a side effect of a group deletion.
		assert.False(t, isNull, "scope must not become NULL")
		assert.Empty(t, scope)

		// Recreate for the remaining subtests.
		mustExec(`INSERT INTO fleet_groups (id, tenant_id, name, color) VALUES ($1, $2, 'Vans', '#112233')`,
			custTenant+"_vans", custTenant)
	})

	t.Run("entitlement provenance may name an operator-side group", func(t *testing.T) {
		// source_group_id is provenance for assign-by-group: the group belongs
		// to the OPERATOR, the row to the customer. A tenant-paired constraint
		// here would refuse every legitimate row.
		mustExec(`
			INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source, source_group_id)
			VALUES ($1, 4242, 'operator', $2)`, custTenant, opTenant+"_pool")
	})

	t.Run("entitlement provenance naming no group is refused", func(t *testing.T) {
		_, err := w.Exec(`
			INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source, source_group_id)
			VALUES ($1, 4243, 'operator', $2)`, custTenant, opTenant+"_ghost")
		require.Error(t, err)
	})

	t.Run("deleting the source group nulls provenance, keeps the grant", func(t *testing.T) {
		mustExec(`DELETE FROM fleet_groups WHERE id = $1`, opTenant+"_pool")

		var n int
		require.NoError(t, w.QueryRow(`
			SELECT COUNT(*) FROM vehicle_entitlements
			 WHERE tenant_id = $1 AND vehicle_token_id = 4242`, custTenant).Scan(&n))
		assert.Equal(t, 1, n, "the entitlement itself must survive")

		var src *string
		require.NoError(t, w.QueryRow(`
			SELECT source_group_id FROM vehicle_entitlements
			 WHERE tenant_id = $1 AND vehicle_token_id = 4242`, custTenant).Scan(&src))
		assert.Nil(t, src, "provenance ends when the group it names does")
	})

	t.Run("NULL scope stays legal — it means unrestricted", func(t *testing.T) {
		mustExec(`
			INSERT INTO memberships (tenant_id, wallet, scope_group_ids)
			VALUES ($1, $2, NULL)
			ON CONFLICT (tenant_id, wallet) DO UPDATE SET scope_group_ids = NULL`,
			custTenant, custWallet)
	})
}
