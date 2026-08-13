package service

import (
	"context"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func groupFixture(t *testing.T) (*GroupService, context.Context) {
	t.Helper()
	store := testStore(t)
	seed(t, store)
	w := store.DBS().Writer
	_, _ = w.Exec(`DELETE FROM fleet_groups WHERE tenant_id = ANY($1)`,
		"{"+opTenant+","+custTenant+","+otherTen+"}")
	t.Cleanup(func() {
		_, _ = w.Exec(`DELETE FROM fleet_groups WHERE tenant_id = ANY($1)`,
			"{"+opTenant+","+custTenant+","+otherTen+"}")
	})
	l := zerolog.Nop()
	return NewGroupService(&l, store), context.Background()
}

func str(s string) *string { return &s }

// The id convention is the load-bearing part: ids minted here must be
// indistinguishable from the ids fleet-lite and kaufmann already hold, or the
// P3 backfill would produce two ids for one group.
func TestGroupIDFor(t *testing.T) {
	assert.Equal(t, "t1_vans", GroupIDFor("t1", "Vans"))
	assert.Equal(t, "t1_north-region", GroupIDFor("t1", "  North Region "))
	assert.Equal(t, "t1_a-b", GroupIDFor("t1", "A_&_B"), "runs of non-alphanumerics collapse to one dash")
	assert.Equal(t, "t1_7-vans", GroupIDFor("t1", "7 Vans!"))
	assert.Equal(t, "", GroupIDFor("t1", "!!!"), "a name with no alphanumerics yields no id")
}

func TestGroupCRUD(t *testing.T) {
	svc, ctx := groupFixture(t)

	t.Run("create mints the R1-convention id", func(t *testing.T) {
		g, err := svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "North Vans", Color: "#FF5733"})
		require.NoError(t, err)
		assert.Equal(t, custTenant+"_north-vans", g.ID)
		assert.Equal(t, "North Vans", g.Name)
		assert.Zero(t, g.VehicleCount)
	})

	t.Run("same name in another tenant is fine, in the same tenant is not", func(t *testing.T) {
		_, err := svc.Create(ctx, opTenant, &models.CreateGroupInput{Name: "North Vans", Color: "#000000"})
		require.NoError(t, err, "uniqueness is per tenant, not global — kaufmann's global unique was the defect")
		_, err = svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "North Vans", Color: "#000000"})
		assert.ErrorIs(t, err, ErrGroupNameTaken)
	})

	t.Run("two names with one slug collide as name-taken", func(t *testing.T) {
		_, err := svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "North! Vans", Color: "#000000"})
		assert.ErrorIs(t, err, ErrGroupNameTaken, "the id is the same, and the id is the identity")
	})

	t.Run("validation refuses what cannot become a group", func(t *testing.T) {
		_, err := svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "", Color: "#000000"})
		assert.ErrorIs(t, err, ErrInvalidGroupInput)
		_, err = svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "Ok", Color: "red"})
		assert.ErrorIs(t, err, ErrInvalidGroupInput)
		_, err = svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "!!!", Color: "#000000"})
		assert.ErrorIs(t, err, ErrInvalidGroupInput)
	})

	t.Run("an unknown tenant is ErrTenantNotFound", func(t *testing.T) {
		_, err := svc.Create(ctx, "aaaaaaaa-dead-dead-dead-000000000009",
			&models.CreateGroupInput{Name: "X", Color: "#000000"})
		assert.ErrorIs(t, err, ErrTenantNotFound)
	})

	t.Run("rename keeps the id", func(t *testing.T) {
		g, err := svc.Update(ctx, custTenant, custTenant+"_north-vans",
			&models.UpdateGroupInput{Name: str("South Vans"), Color: str("#00FF00")})
		require.NoError(t, err)
		assert.Equal(t, custTenant+"_north-vans", g.ID,
			"scope_group_ids and published attestations hold the id; a rename must not orphan them")
		assert.Equal(t, "South Vans", g.Name)
		assert.Equal(t, "#00FF00", g.Color)
	})

	t.Run("groups are invisible across tenants", func(t *testing.T) {
		_, err := svc.Get(ctx, opTenant, custTenant+"_north-vans")
		assert.ErrorIs(t, err, ErrGroupNotFound)
		_, err = svc.Update(ctx, opTenant, custTenant+"_north-vans", &models.UpdateGroupInput{Name: str("X")})
		assert.ErrorIs(t, err, ErrGroupNotFound)
		err = svc.Delete(ctx, opTenant, custTenant+"_north-vans")
		assert.ErrorIs(t, err, ErrGroupNotFound)
	})
}

func TestGroupVehicles(t *testing.T) {
	svc, ctx := groupFixture(t)
	g, err := svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "Vans", Color: "#123456"})
	require.NoError(t, err)

	t.Run("add is idempotent and set-shaped", func(t *testing.T) {
		require.NoError(t, svc.AddVehicles(ctx, custTenant, g.ID, []int64{101, 102}))
		require.NoError(t, svc.AddVehicles(ctx, custTenant, g.ID, []int64{102, 103}))

		got, err := svc.ListVehicles(ctx, custTenant, g.ID)
		require.NoError(t, err)
		assert.Equal(t, []int64{101, 102, 103}, got)

		full, err := svc.Get(ctx, custTenant, g.ID)
		require.NoError(t, err)
		assert.Equal(t, 3, full.VehicleCount)
	})

	t.Run("a vehicle may be in several groups", func(t *testing.T) {
		g2, err := svc.Create(ctx, custTenant, &models.CreateGroupInput{Name: "Priority", Color: "#654321"})
		require.NoError(t, err)
		require.NoError(t, svc.AddVehicles(ctx, custTenant, g2.ID, []int64{101}))
		got, err := svc.ListVehicles(ctx, custTenant, g2.ID)
		require.NoError(t, err)
		assert.Equal(t, []int64{101}, got)
	})

	t.Run("remove is idempotent", func(t *testing.T) {
		require.NoError(t, svc.RemoveVehicle(ctx, custTenant, g.ID, 102))
		require.NoError(t, svc.RemoveVehicle(ctx, custTenant, g.ID, 102))
		got, err := svc.ListVehicles(ctx, custTenant, g.ID)
		require.NoError(t, err)
		assert.Equal(t, []int64{101, 103}, got)
	})

	t.Run("membership writes require the group to be the tenant's", func(t *testing.T) {
		err := svc.AddVehicles(ctx, opTenant, g.ID, []int64{999})
		assert.ErrorIs(t, err, ErrGroupNotFound,
			"an operator must not be able to edit membership of a customer's group by naming its id")
	})

	t.Run("deleting the group cascades its memberships", func(t *testing.T) {
		require.NoError(t, svc.Delete(ctx, custTenant, g.ID))
		_, err := svc.ListVehicles(ctx, custTenant, g.ID)
		assert.ErrorIs(t, err, ErrGroupNotFound)
	})
}
