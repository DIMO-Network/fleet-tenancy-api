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
	scOperator    = "bbbbbbbb-0000-0000-0000-000000000001"
	scManagedCust = "bbbbbbbb-0000-0000-0000-000000000002" // child, no license of its own
	scOwnCredCust = "bbbbbbbb-0000-0000-0000-000000000003" // child, but has its own license
	scDelegated   = "bbbbbbbb-0000-0000-0000-000000000004" // unparented, reached by delegation
	scStranger    = "bbbbbbbb-0000-0000-0000-000000000005"
	scOtherOp     = "bbbbbbbb-0000-0000-0000-000000000006"
)

func seedScope(t *testing.T, store *db.Store) {
	t.Helper()
	w := store.DBS().Writer
	ids := "{" + scOperator + "," + scManagedCust + "," + scOwnCredCust + "," + scDelegated + "," + scStranger + "," + scOtherOp + "}"
	_, _ = w.Exec(`DELETE FROM tenant_delegations WHERE operator_tenant_id = ANY($1) OR customer_tenant_id = ANY($1)`, ids)
	_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = ANY($1)`, ids)
	_, _ = w.Exec(`DELETE FROM tenants WHERE id = ANY($1)`, ids)

	_, err := w.Exec(`INSERT INTO tenants (id,name,kind,entitlement_mode) VALUES
		($1,'ScopeOp','operator','implicit'),
		($2,'Managed','customer','explicit'),
		($3,'OwnCred','customer','explicit'),
		($4,'Delegated','customer','explicit'),
		($5,'Stranger','customer','explicit'),
		($6,'OtherOp','operator','implicit')`,
		scOperator, scManagedCust, scOwnCredCust, scDelegated, scStranger, scOtherOp)
	require.NoError(t, err)

	_, err = w.Exec(`UPDATE tenants SET parent_tenant_id=$1 WHERE id IN ($2,$3)`, scOperator, scManagedCust, scOwnCredCust)
	require.NoError(t, err)

	// The managed customer deliberately has no dimo_client_id: it resolves to
	// the operator's license. The other child has its own.
	_, err = w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id) VALUES
		($1,'0xAAAA000000000000000000000000000000000001'),
		($2, NULL),
		($3,'0xAAAA000000000000000000000000000000000003')`,
		scOperator, scManagedCust, scOwnCredCust)
	require.NoError(t, err)

	_, err = w.Exec(`INSERT INTO tenant_delegations (operator_tenant_id, customer_tenant_id, scopes)
		VALUES ($1,$2,ARRAY['manage_members'])`, scOperator, scDelegated)
	require.NoError(t, err)
}

func scopeCaller(id string, service bool) *models.CallerTenant {
	return &models.CallerTenant{TenantID: id, IsService: service}
}

func TestCallerMayAccess(t *testing.T) {
	store := testStore(t)
	seedScope(t, store)
	l := zerolog.Nop()
	svc := NewTenantService(&l, store)
	ctx := context.Background()

	t.Run("a caller may ask about itself", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scOperator, false), scOperator)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	// The case that makes "caller must equal subject" the wrong rule. This
	// customer holds no license of its own and is reached with the operator's,
	// so an equality check would deny the operator access to its own customer.
	t.Run("an operator reaches a managed customer that has no license", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scOperator, false), scManagedCust)
		require.NoError(t, err)
		assert.True(t, ok, "the customer resolves to the caller's credential, so the caller must reach it")
	})

	// A child that has its own credential does not resolve to the parent's, so
	// the parent link alone must not grant access — a delegation would.
	t.Run("a child with its own license is not reachable by parentage alone", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scOperator, false), scOwnCredCust)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("a delegation grants access", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scOperator, false), scDelegated)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("an unrelated tenant is refused", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scOperator, false), scStranger)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	// The exposure this whole change closes: a customer tenant's own license
	// must not reach an operator it has nothing to do with.
	t.Run("a customer cannot read another operator's tenant", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scOwnCredCust, false), scOtherOp)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("scope is not symmetric — a child cannot read its parent", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scManagedCust, false), scOperator)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("a service caller reaches anything", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scStranger, true), scOperator)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("no caller means no access", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, nil, scOperator)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	// A typo must read as a client error, not a 500 from a failed uuid cast.
	t.Run("a malformed tenant id is a caller error", func(t *testing.T) {
		_, err := svc.CallerMayAccess(ctx, scopeCaller(scOperator, false), "not-a-uuid")
		assert.True(t, errors.Is(err, ErrInvalidTenantID), "got %v", err)
	})

	t.Run("a service caller is not tripped up by a malformed id either", func(t *testing.T) {
		ok, err := svc.CallerMayAccess(ctx, scopeCaller(scOperator, true), "not-a-uuid")
		require.NoError(t, err)
		assert.True(t, ok)
	})
}
