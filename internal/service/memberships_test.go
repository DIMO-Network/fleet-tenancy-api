package service

import (
	"context"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Token ids used only by these tests, distinct from the entitlement tests' set.
const (
	memTokenA int64 = 910001
	memTokenB int64 = 910002
	memTokenC int64 = 910003
	// Deliberately never entitled, so the entitled-only rule has something to
	// refuse.
	memTokenUnentitled int64 = 910009
)

const memTokenList = "{910001,910002,910003,910009}"

func membershipFixture(t *testing.T) (*MembershipService, *db.Store, context.Context) {
	t.Helper()
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()
	svc := NewMembershipService(&l, store)
	ctx := context.Background()

	// The customer holds three vehicles. Memberships can only exist on vehicles
	// the tenant is entitled to, so the entitlements come first.
	for _, tok := range []int64{memTokenA, memTokenB, memTokenC} {
		_, err := store.DBS().Writer.Exec(
			`INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source)
			 VALUES ($1, $2, 'operator')
			 ON CONFLICT (tenant_id, vehicle_token_id) DO UPDATE SET revoked_at = NULL`,
			custTenant, tok)
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		w := store.DBS().Writer
		_, _ = w.Exec(`DELETE FROM vehicle_membership_moves WHERE membership_id IN
			(SELECT id FROM vehicle_memberships WHERE vehicle_token_id = ANY($1))`, memTokenList)
		_, _ = w.Exec(`DELETE FROM vehicle_memberships WHERE vehicle_token_id = ANY($1)`, memTokenList)
		_, _ = w.Exec(`DELETE FROM vehicle_entitlements WHERE vehicle_token_id = ANY($1)`, memTokenList)
		_, _ = w.Exec(`UPDATE tenants SET memberships_enforced = false WHERE id = $1`, custTenant)
	})
	return svc, store, ctx
}

func create(t *testing.T, svc *MembershipService, ctx context.Context, tok int64, term int) *models.VehicleMembership {
	t.Helper()
	m, err := svc.Create(ctx, custTenant, &models.CreateMembershipInput{
		VehicleTokenID: tok, TermMonths: term,
	}, opWallet)
	require.NoError(t, err)
	return m
}

// expireAt forces a membership's expiry, which is the only way to exercise the
// lapse rules without waiting a month.
func expireAt(t *testing.T, store *db.Store, id string, at time.Time) {
	t.Helper()
	_, err := store.DBS().Writer.Exec(
		`UPDATE vehicle_memberships SET expires_at = $1 WHERE id = $2`, at, id)
	require.NoError(t, err)
}

func TestCreateMembership(t *testing.T) {
	svc, _, ctx := membershipFixture(t)

	t.Run("creates and lists, with an expiry the database computed", func(t *testing.T) {
		m := create(t, svc, ctx, memTokenA, 12)
		assert.Equal(t, models.MembershipActive, m.Status)
		assert.Equal(t, 12, m.TermMonths)
		assert.Nil(t, m.CanceledAt)

		starts, err := time.Parse(time.RFC3339, m.StartsAt)
		require.NoError(t, err)
		expires, err := time.Parse(time.RFC3339, m.ExpiresAt)
		require.NoError(t, err)
		// Twelve months on, not 365 days: the point of computing it in Postgres
		// is that it is calendar arithmetic.
		assert.Equal(t, starts.AddDate(1, 0, 0).Year(), expires.Year())
		assert.Equal(t, starts.Month(), expires.Month())

		list, err := svc.List(ctx, custTenant)
		require.NoError(t, err)
		assert.False(t, list.Enforced, "enforcement is off until an operator turns it on")
		require.Len(t, list.Memberships, 1)
	})

	t.Run("refuses a vehicle the customer is not entitled to", func(t *testing.T) {
		_, err := svc.Create(ctx, custTenant, &models.CreateMembershipInput{
			VehicleTokenID: memTokenUnentitled, TermMonths: 12,
		}, opWallet)
		assert.ErrorIs(t, err, ErrVehicleNotEntitled)
	})

	t.Run("refuses a term that is not offered", func(t *testing.T) {
		_, err := svc.Create(ctx, custTenant, &models.CreateMembershipInput{
			VehicleTokenID: memTokenB, TermMonths: 6,
		}, opWallet)
		assert.ErrorIs(t, err, ErrInvalidTerm)
	})

	t.Run("refuses an implicit-mode tenant, whose fleet is not defined by rows here", func(t *testing.T) {
		_, err := svc.Create(ctx, opTenant, &models.CreateMembershipInput{
			VehicleTokenID: memTokenA, TermMonths: 12,
		}, opWallet)
		assert.ErrorIs(t, err, ErrNotExplicitMode)
	})

	t.Run("refuses a second membership on a vehicle that already has a live one", func(t *testing.T) {
		_, err := svc.Create(ctx, custTenant, &models.CreateMembershipInput{
			VehicleTokenID: memTokenA, TermMonths: 24,
		}, opWallet)
		assert.ErrorIs(t, err, ErrMembershipExists,
			"silently replacing paid time is the bug nobody reports until an invoice is wrong")
	})
}

func TestCreateSupersedesALapsedMembership(t *testing.T) {
	svc, store, ctx := membershipFixture(t)

	old := create(t, svc, ctx, memTokenA, 1)
	expireAt(t, store, old.ID, time.Now().Add(-24*time.Hour))

	// A lapsed vehicle has to be able to start fresh, so this is allowed where
	// the unexpired case above is refused.
	fresh, err := svc.Create(ctx, custTenant, &models.CreateMembershipInput{
		VehicleTokenID: memTokenA, TermMonths: 12,
	}, opWallet)
	require.NoError(t, err)
	assert.NotEqual(t, old.ID, fresh.ID)

	list, err := svc.List(ctx, custTenant)
	require.NoError(t, err)
	require.Len(t, list.Memberships, 1, "the lapsed row is superseded, not left alongside")
	assert.Equal(t, fresh.ID, list.Memberships[0].ID)
}

func TestMembershipStatusFollowsTheClock(t *testing.T) {
	svc, store, ctx := membershipFixture(t)

	m := create(t, svc, ctx, memTokenA, 12)

	for _, tc := range []struct {
		name   string
		in     time.Duration
		expect string
	}{
		{"comfortably ahead", 200 * 24 * time.Hour, models.MembershipActive},
		{"inside the warning window", 10 * 24 * time.Hour, models.MembershipExpiringSoon},
		{"just past the window", 31 * 24 * time.Hour, models.MembershipActive},
		{"lapsed", -1 * time.Hour, models.MembershipExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expireAt(t, store, m.ID, time.Now().Add(tc.in))
			list, err := svc.List(ctx, custTenant)
			require.NoError(t, err)
			require.Len(t, list.Memberships, 1)
			assert.Equal(t, tc.expect, list.Memberships[0].Status)
		})
	}
}

func TestMoveMembership(t *testing.T) {
	svc, store, ctx := membershipFixture(t)

	m := create(t, svc, ctx, memTokenA, 24)
	originalExpiry := m.ExpiresAt

	t.Run("carries the paid term to the new vehicle", func(t *testing.T) {
		moved, err := svc.Move(ctx, custTenant, m.ID,
			&models.MoveMembershipInput{VehicleTokenID: memTokenB}, opWallet)
		require.NoError(t, err)
		assert.Equal(t, memTokenB, moved.VehicleTokenID)
		assert.Equal(t, originalExpiry, moved.ExpiresAt,
			"the customer paid for a period, not for a particular vehicle")
		assert.Equal(t, m.ID, moved.ID)
	})

	t.Run("records where it came from", func(t *testing.T) {
		var from, to int64
		err := store.DBS().Reader.QueryRow(
			`SELECT from_token_id, to_token_id FROM vehicle_membership_moves
			  WHERE membership_id = $1`, m.ID).Scan(&from, &to)
		require.NoError(t, err)
		assert.Equal(t, memTokenA, from)
		assert.Equal(t, memTokenB, to)
	})

	t.Run("leaves the old vehicle's entitlement alone", func(t *testing.T) {
		var live bool
		err := store.DBS().Reader.QueryRow(
			`SELECT true FROM vehicle_entitlements
			  WHERE tenant_id = $1 AND vehicle_token_id = $2 AND revoked_at IS NULL`,
			custTenant, memTokenA).Scan(&live)
		require.NoError(t, err, "access and payment are different questions")
		assert.True(t, live)
	})

	t.Run("refuses a target the customer is not entitled to", func(t *testing.T) {
		_, err := svc.Move(ctx, custTenant, m.ID,
			&models.MoveMembershipInput{VehicleTokenID: memTokenUnentitled}, opWallet)
		assert.ErrorIs(t, err, ErrVehicleNotEntitled)
	})

	t.Run("refuses moving onto itself", func(t *testing.T) {
		_, err := svc.Move(ctx, custTenant, m.ID,
			&models.MoveMembershipInput{VehicleTokenID: memTokenB}, opWallet)
		assert.ErrorIs(t, err, ErrSameVehicle)
	})

	t.Run("refuses a target that already has a live membership", func(t *testing.T) {
		create(t, svc, ctx, memTokenC, 12)
		_, err := svc.Move(ctx, custTenant, m.ID,
			&models.MoveMembershipInput{VehicleTokenID: memTokenC}, opWallet)
		assert.ErrorIs(t, err, ErrMembershipExists)
	})

	t.Run("is not found for another tenant's membership", func(t *testing.T) {
		_, err := svc.Move(ctx, otherTen, m.ID,
			&models.MoveMembershipInput{VehicleTokenID: memTokenA}, opWallet)
		assert.ErrorIs(t, err, ErrMembershipNotFound)
	})

	t.Run("is not found for a malformed id, rather than a database error", func(t *testing.T) {
		_, err := svc.Move(ctx, custTenant, "not-a-uuid",
			&models.MoveMembershipInput{VehicleTokenID: memTokenA}, opWallet)
		assert.ErrorIs(t, err, ErrMembershipNotFound)
	})
}

func TestRenewMembershipNeverBackdates(t *testing.T) {
	svc, store, ctx := membershipFixture(t)

	t.Run("early renewal adds to the end of the current term", func(t *testing.T) {
		m := create(t, svc, ctx, memTokenA, 12)
		before, err := time.Parse(time.RFC3339, m.ExpiresAt)
		require.NoError(t, err)

		renewed, err := svc.Renew(ctx, custTenant, m.ID,
			&models.RenewMembershipInput{TermMonths: 12}, opWallet)
		require.NoError(t, err)

		after, err := time.Parse(time.RFC3339, renewed.ExpiresAt)
		require.NoError(t, err)
		assert.True(t, after.After(before.AddDate(0, 11, 0)),
			"renewing early must extend from the existing expiry, not from today")
	})

	t.Run("renewal after a lapse starts from now, not from the old expiry", func(t *testing.T) {
		m := create(t, svc, ctx, memTokenB, 1)
		expireAt(t, store, m.ID, time.Now().AddDate(0, -6, 0))

		renewed, err := svc.Renew(ctx, custTenant, m.ID,
			&models.RenewMembershipInput{TermMonths: 1}, opWallet)
		require.NoError(t, err)
		assert.Equal(t, models.MembershipActive, renewed.Status,
			"a renewal that landed in the past would leave the vehicle still hidden")

		expires, err := time.Parse(time.RFC3339, renewed.ExpiresAt)
		require.NoError(t, err)
		assert.True(t, expires.After(time.Now()))
	})

	t.Run("refuses a term that is not offered", func(t *testing.T) {
		m := create(t, svc, ctx, memTokenC, 12)
		_, err := svc.Renew(ctx, custTenant, m.ID,
			&models.RenewMembershipInput{TermMonths: 7}, opWallet)
		assert.ErrorIs(t, err, ErrInvalidTerm)
	})
}

func TestCancelMembership(t *testing.T) {
	svc, _, ctx := membershipFixture(t)

	m := create(t, svc, ctx, memTokenA, 12)
	require.NoError(t, svc.Cancel(ctx, custTenant, m.ID))

	list, err := svc.List(ctx, custTenant)
	require.NoError(t, err)
	assert.Empty(t, list.Memberships, "cancelled rows are history, not list entries")

	t.Run("cancelling again reports not found, so the controller can treat it as done", func(t *testing.T) {
		assert.ErrorIs(t, svc.Cancel(ctx, custTenant, m.ID), ErrMembershipNotFound)
	})

	t.Run("frees the vehicle for a new membership", func(t *testing.T) {
		fresh, err := svc.Create(ctx, custTenant, &models.CreateMembershipInput{
			VehicleTokenID: memTokenA, TermMonths: 12,
		}, opWallet)
		require.NoError(t, err)
		assert.NotEqual(t, m.ID, fresh.ID)
	})
}

func TestActiveTokenIDsIsWhatFleetLiteWillGateOn(t *testing.T) {
	svc, store, ctx := membershipFixture(t)

	active := create(t, svc, ctx, memTokenA, 12)
	lapsed := create(t, svc, ctx, memTokenB, 1)
	canceled := create(t, svc, ctx, memTokenC, 12)

	expireAt(t, store, lapsed.ID, time.Now().Add(-time.Hour))
	require.NoError(t, svc.Cancel(ctx, custTenant, canceled.ID))

	enforced, tokens, err := svc.ActiveTokenIDs(ctx, custTenant)
	require.NoError(t, err)
	assert.False(t, enforced)
	assert.Equal(t, []int64{memTokenA}, tokens,
		"only unexpired, uncancelled memberships keep a vehicle visible")
	_ = active

	t.Run("reports enforcement once an operator turns it on", func(t *testing.T) {
		_, err := store.DBS().Writer.Exec(
			`UPDATE tenants SET memberships_enforced = true WHERE id = $1`, custTenant)
		require.NoError(t, err)

		enforced, _, err := svc.ActiveTokenIDs(ctx, custTenant)
		require.NoError(t, err)
		assert.True(t, enforced)
	})

	t.Run("an unknown tenant is not found", func(t *testing.T) {
		_, _, err := svc.ActiveTokenIDs(ctx, "aaaaaaaa-0000-0000-0000-0000000000ff")
		assert.ErrorIs(t, err, ErrTenantNotFound)
	})
}
