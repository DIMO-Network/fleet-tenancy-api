package service

import (
	"context"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// cleanupCustomers removes the tenants a test created under the operator
// fixture, leaving the shared fixtures alone.
func cleanupCustomers(t *testing.T, svc *TenantService) {
	t.Helper()
	t.Cleanup(func() {
		w := svc.pdb.DBS().Writer
		_, _ = w.Exec(`DELETE FROM tenant_delegations
		                WHERE operator_tenant_id = $1
		                  AND customer_tenant_id NOT IN ($2, $3)`, opTenant, custTenant, otherTen)
		_, _ = w.Exec(`DELETE FROM tenants
		                WHERE parent_tenant_id = $1 AND id NOT IN ($2, $3)`, opTenant, custTenant, otherTen)
	})
}

func TestCreateCustomer(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	cleanupCustomers(t, svc)
	ctx := context.Background()

	t.Run("creates a managed, explicit-mode customer under the operator", func(t *testing.T) {
		got, err := svc.CreateCustomer(ctx, opTenant, &models.CreateTenantInput{
			Name: "Northwind Logistics", ExternalRef: strPtr("ACME-1"),
		}, opWallet)
		require.NoError(t, err)

		assert.Equal(t, "Northwind Logistics", got.Name)
		assert.Equal(t, models.KindCustomer, got.Kind)
		require.NotNil(t, got.ParentTenantID)
		assert.Equal(t, opTenant, *got.ParentTenantID)
		assert.True(t, got.Managed)
		assert.Equal(t, models.EntitlementExplicit, got.EntitlementMode,
			"a managed customer's fleet is the rows its operator writes")
		assert.True(t, got.FleetLiteEnabled)
		assert.Equal(t, models.StatusActive, got.Status)
		require.NotNil(t, got.ExternalRef)
		assert.Equal(t, "ACME-1", *got.ExternalRef)
	})

	// Authorization checks the delegation row rather than parent_tenant_id, so
	// a customer created without one is a tenant its own operator cannot reach.
	t.Run("writes the delegation that lets the operator manage it", func(t *testing.T) {
		created, err := svc.CreateCustomer(ctx, opTenant,
			&models.CreateTenantInput{Name: "Cedar Valley"}, opWallet)
		require.NoError(t, err)

		authz := NewAuthzService(&l, store)
		got, err := authz.Authorize(ctx, created.ID, opWallet)
		require.NoError(t, err)
		assert.Equal(t, models.ViaDelegation, got.Via,
			"operator staff reach a customer by delegation, never by membership")
		assert.Contains(t, got.Permissions, "manage_members")
	})

	t.Run("names are unique per operator", func(t *testing.T) {
		_, err := svc.CreateCustomer(ctx, opTenant,
			&models.CreateTenantInput{Name: "Duplicate Co"}, opWallet)
		require.NoError(t, err)

		_, err = svc.CreateCustomer(ctx, opTenant,
			&models.CreateTenantInput{Name: "duplicate co"}, opWallet)
		require.ErrorIs(t, err, ErrNameTaken, "case-insensitive within one operator")
	})

	t.Run("a customer tenant cannot have customers of its own", func(t *testing.T) {
		_, err := svc.CreateCustomer(ctx, custTenant,
			&models.CreateTenantInput{Name: "Sub-sub"}, opWallet)
		require.ErrorIs(t, err, ErrNotAnOperator)
	})

	t.Run("an empty name is refused", func(t *testing.T) {
		_, err := svc.CreateCustomer(ctx, opTenant,
			&models.CreateTenantInput{Name: "   "}, opWallet)
		require.Error(t, err)
	})
}

func TestListChildrenCounts(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	cleanupCustomers(t, svc)
	ctx := context.Background()

	children, err := svc.ListChildren(ctx, opTenant)
	require.NoError(t, err)
	require.NotEmpty(t, children)

	var cust *models.Tenant
	for i := range children {
		if children[i].ID == custTenant {
			cust = &children[i]
		}
	}
	require.NotNil(t, cust, "the seeded customer is a child of the operator")

	assert.Equal(t, 1, cust.UserCount, "counts come from the rows, not a stored counter")
	assert.Equal(t, 0, cust.VehicleCount)
}

func TestUpdateTenant(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	cleanupCustomers(t, svc)
	ctx := context.Background()

	created, err := svc.CreateCustomer(ctx, opTenant,
		&models.CreateTenantInput{Name: "Patchable", ExternalRef: strPtr("REF-1")}, opWallet)
	require.NoError(t, err)

	t.Run("patching one field leaves the others alone", func(t *testing.T) {
		got, err := svc.Update(ctx, created.ID, &models.UpdateTenantInput{Name: strPtr("Renamed")})
		require.NoError(t, err)
		assert.Equal(t, "Renamed", got.Name)
		require.NotNil(t, got.ExternalRef)
		assert.Equal(t, "REF-1", *got.ExternalRef, "an absent field must not clear the column")
		assert.True(t, got.FleetLiteEnabled)
	})

	t.Run("an explicit empty external ref clears it", func(t *testing.T) {
		got, err := svc.Update(ctx, created.ID, &models.UpdateTenantInput{ExternalRef: strPtr("")})
		require.NoError(t, err)
		assert.Nil(t, got.ExternalRef)
	})

	t.Run("fleet-lite visibility toggles", func(t *testing.T) {
		got, err := svc.Update(ctx, created.ID, &models.UpdateTenantInput{FleetLiteEnabled: boolPtr(false)})
		require.NoError(t, err)
		assert.False(t, got.FleetLiteEnabled)
	})

	t.Run("an unknown status is refused rather than stored", func(t *testing.T) {
		_, err := svc.Update(ctx, created.ID, &models.UpdateTenantInput{Status: strPtr("paused")})
		require.ErrorIs(t, err, ErrInvalidStatus)
	})

	t.Run("an unknown tenant is not found", func(t *testing.T) {
		_, err := svc.Update(ctx, "aaaaaaaa-0000-0000-0000-0000000000ff",
			&models.UpdateTenantInput{Name: strPtr("nope")})
		require.ErrorIs(t, err, ErrTenantNotFound)
	})
}

// Suspension has to actually take access away. It was decorative until now: the
// status rode along on every authz response and no caller read it, so the
// console could say a customer's users can no longer sign in while they still
// could.
func TestSuspensionRemovesAccess(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	authz := NewAuthzService(&l, store)
	cleanupCustomers(t, svc)
	ctx := context.Background()

	before, err := authz.Authorize(ctx, custTenant, custWallet)
	require.NoError(t, err)
	require.True(t, before.Member, "precondition: the member has access while active")

	_, err = svc.Update(ctx, custTenant, &models.UpdateTenantInput{Status: strPtr(models.StatusSuspended)})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = svc.Update(context.Background(), custTenant,
			&models.UpdateTenantInput{Status: strPtr(models.StatusActive)})
	})

	t.Run("a member of a suspended tenant may not act", func(t *testing.T) {
		got, err := authz.Authorize(ctx, custTenant, custWallet)
		require.NoError(t, err)
		assert.False(t, got.Member)
		assert.Equal(t, models.ViaNone, got.Via)
		assert.Empty(t, got.Permissions)
	})

	t.Run("the status is still reported, so a caller can say why", func(t *testing.T) {
		got, err := authz.Authorize(ctx, custTenant, custWallet)
		require.NoError(t, err)
		assert.Equal(t, models.StatusSuspended, got.TenantStatus)
	})

	// Otherwise suspending a customer would lock its operator out of the very
	// screen used to un-suspend it.
	t.Run("delegated management is refused too", func(t *testing.T) {
		got, err := authz.Authorize(ctx, custTenant, opWallet)
		require.NoError(t, err)
		assert.Equal(t, models.ViaNone, got.Via)
	})

	t.Run("other tenants are unaffected", func(t *testing.T) {
		got, err := authz.Authorize(ctx, opTenant, opWallet)
		require.NoError(t, err)
		assert.True(t, got.Member)
	})

	t.Run("resuming restores access", func(t *testing.T) {
		_, err := svc.Update(ctx, custTenant, &models.UpdateTenantInput{Status: strPtr(models.StatusActive)})
		require.NoError(t, err)

		got, err := authz.Authorize(ctx, custTenant, custWallet)
		require.NoError(t, err)
		assert.True(t, got.Member, "assignments and memberships survive a suspension")
	})
}

func TestListMembersScopeStates(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	ctx := context.Background()

	members, err := svc.ListMembers(ctx, custTenant)
	require.NoError(t, err)
	require.Len(t, members, 1)

	m := members[0]
	assert.Equal(t, custWallet, m.Wallet)
	require.NotNil(t, m.ScopeGroupIDs, "a group-restricted member must not read as unrestricted")
	assert.Equal(t, []string{custTenant + "_vans"}, m.ScopeGroupIDs)

	opMembers, err := svc.ListMembers(ctx, opTenant)
	require.NoError(t, err)
	require.NotEmpty(t, opMembers)
	for _, om := range opMembers {
		if om.Wallet == opWallet {
			assert.Nil(t, om.ScopeGroupIDs, "nil scope is unrestricted and must survive the round trip")
		}
	}
}
