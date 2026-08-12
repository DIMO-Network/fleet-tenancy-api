package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWallet is a wallet the shared fixtures don't use, so these tests can write
// freely without disturbing the authz fixtures in the same database.
const (
	newWallet   = "0x4444444444444444444444444444444444444444"
	actorWallet = "0x5555555555555555555555555555555555555555"
	// Deliberately lowercased: callers disagree on casing (kaufmann checksums,
	// fleet-lite lowercases) and both must land on one row.
	newWalletLower = "0x4444444444444444444444444444444444444444"
)

func scope(raw string) json.RawMessage { return json.RawMessage(raw) }

func TestMemberUpsert(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewMemberService(&l, store)
	authz := NewAuthzService(&l, store)
	ctx := context.Background()

	t.Cleanup(func() {
		w := store.DBS().Writer
		_, _ = w.Exec(`DELETE FROM memberships WHERE wallet = ANY($1)`, "{"+newWallet+"}")
		_, _ = w.Exec(`DELETE FROM users WHERE wallet = ANY($1)`, "{"+newWallet+","+actorWallet+"}")
	})

	t.Run("creates the user row and the membership together", func(t *testing.T) {
		err := svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			Email:           "new@example.com",
			Role:            "admin",
			Permissions:     []string{models.CapManageMembers, models.CapReports},
			ScopeGroupIDs:   scope("null"),
			GrantedByWallet: actorWallet,
		})
		require.NoError(t, err)

		got, err := authz.Authorize(ctx, opTenant, newWallet)
		require.NoError(t, err)
		assert.True(t, got.Member)
		assert.Equal(t, models.ViaDirect, got.Via)
		assert.Equal(t, "admin", got.Role)
		assert.True(t, got.HasCapability(models.CapManageMembers))
		assert.True(t, got.Unrestricted(), "null scope means every group")
	})

	// The inversion that matters. nil is "sees everything" and empty is "sees
	// nothing", and conflating them handed 131 memberships an entire fleet
	// during the backfill.
	t.Run("empty scope restricts to nothing, and is not null", func(t *testing.T) {
		err := svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			Permissions:   []string{},
			ScopeGroupIDs: scope("[]"),
		})
		require.NoError(t, err)

		got, err := authz.Authorize(ctx, opTenant, newWallet)
		require.NoError(t, err)
		assert.True(t, got.Member)
		assert.False(t, got.Unrestricted(), "[] must not read as unrestricted")
		assert.Empty(t, got.ScopeGroupIDs)
		assert.NotNil(t, got.ScopeGroupIDs, "empty is a restriction, not an absence")
	})

	t.Run("named groups round-trip", func(t *testing.T) {
		err := svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			ScopeGroupIDs: scope(`["` + opTenant + `_vans","` + opTenant + `_north"]`),
		})
		require.NoError(t, err)

		got, err := authz.Authorize(ctx, opTenant, newWallet)
		require.NoError(t, err)
		assert.Equal(t, []string{opTenant + "_vans", opTenant + "_north"}, got.ScopeGroupIDs)
	})

	// An omitted field must not silently mean unrestricted.
	t.Run("absent scope is refused", func(t *testing.T) {
		err := svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			Permissions: []string{models.CapReports},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "scopeGroupIds is required")
	})

	// Replace, don't merge: a capability removed at the caller must disappear
	// here, or the two can never converge.
	t.Run("upsert replaces permissions rather than merging", func(t *testing.T) {
		require.NoError(t, svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			Permissions:   []string{models.CapManageMembers, models.CapReports},
			ScopeGroupIDs: scope("null"),
		}))
		require.NoError(t, svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			Permissions:   []string{models.CapReports},
			ScopeGroupIDs: scope("null"),
		}))

		got, err := authz.Authorize(ctx, opTenant, newWallet)
		require.NoError(t, err)
		assert.Equal(t, []string{models.CapReports}, got.Permissions)
		assert.False(t, got.HasCapability(models.CapManageMembers), "removed capability must not survive")
	})

	t.Run("lowercase wallet updates the same row", func(t *testing.T) {
		require.NoError(t, svc.Upsert(ctx, opTenant, newWalletLower, &models.MemberWrite{
			Role:          "owner",
			ScopeGroupIDs: scope("null"),
		}))

		var count int
		require.NoError(t, store.DBS().Reader.QueryRow(
			`SELECT count(*) FROM memberships WHERE tenant_id=$1 AND lower(wallet)=lower($2)`,
			opTenant, newWallet).Scan(&count))
		assert.Equal(t, 1, count, "one person, one membership row")
	})

	t.Run("empty email does not clear a known one", func(t *testing.T) {
		require.NoError(t, svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			Email:         "keep@example.com",
			ScopeGroupIDs: scope("null"),
		}))
		require.NoError(t, svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
			ScopeGroupIDs: scope("null"),
		}))

		var email string
		require.NoError(t, store.DBS().Reader.QueryRow(
			`SELECT email FROM users WHERE wallet=$1`, newWallet).Scan(&email))
		assert.Equal(t, "keep@example.com", email)
	})

	t.Run("unknown tenant is refused, not created", func(t *testing.T) {
		err := svc.Upsert(ctx, "aaaaaaaa-0000-0000-0000-00000000dead", newWallet,
			&models.MemberWrite{ScopeGroupIDs: scope("null")})
		require.ErrorIs(t, err, ErrTenantNotFound)
	})
}

func TestMemberRemove(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewMemberService(&l, store)
	authz := NewAuthzService(&l, store)
	ctx := context.Background()

	t.Cleanup(func() {
		w := store.DBS().Writer
		_, _ = w.Exec(`DELETE FROM memberships WHERE wallet = ANY($1)`, "{"+newWallet+"}")
		_, _ = w.Exec(`DELETE FROM users WHERE wallet = ANY($1)`, "{"+newWallet+"}")
	})

	require.NoError(t, svc.Upsert(ctx, opTenant, newWallet, &models.MemberWrite{
		ScopeGroupIDs: scope("null"),
	}))

	t.Run("removes access", func(t *testing.T) {
		require.NoError(t, svc.Remove(ctx, opTenant, newWallet))

		got, err := authz.Authorize(ctx, opTenant, newWallet)
		require.NoError(t, err)
		assert.False(t, got.Member)
		assert.Equal(t, models.ViaNone, got.Via)
	})

	t.Run("removing twice reports not found", func(t *testing.T) {
		err := svc.Remove(ctx, opTenant, newWallet)
		require.ErrorIs(t, err, ErrMemberNotFound)
	})

	t.Run("leaves other tenants alone", func(t *testing.T) {
		got, err := authz.Authorize(ctx, custTenant, custWallet)
		require.NoError(t, err)
		assert.True(t, got.Member, "removing one membership must not touch another")
	})
}
