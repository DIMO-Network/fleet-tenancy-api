package main

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func noEmail() sql.NullString { return sql.NullString{} }
func noTime() sql.NullTime    { return sql.NullTime{} }

// The case that prompted this: a wallet present in both sources for the Kaufmann
// tenant. Before merging, whichever source wrote last won outright, which
// demoted a kaufmann admin to role=member with no capabilities at all.
func TestMergeDoesNotLoseCapabilities(t *testing.T) {
	a := &memberAccess{}
	// kaufmann: admin with real capabilities, restricted to one group.
	a.merge("admin", []string{models.CapReports, models.CapOnboardVehicle, models.CapManageMembers},
		false, []string{"tenantA_demos-isuzu"}, noEmail(), noTime())
	// fleet-lite: the same person, plain member, no capabilities.
	a.merge("member", nil, false, []string{"tenantA_demos-isuzu"}, noEmail(), noTime())

	perms, err := a.permissionsJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `["manage_members","onboard_vehicles","reports"]`, perms,
		"the kaufmann admin's capabilities must survive being mentioned by fleet-lite")
	assert.Equal(t, "admin", a.role, "the more privileged label wins")
}

func TestMergeScopeTakesTheMorePermissiveSide(t *testing.T) {
	t.Run("unrestricted anywhere wins over a group list", func(t *testing.T) {
		a := &memberAccess{}
		a.merge("member", nil, false, []string{"tenantA_vans"}, noEmail(), noTime())
		a.merge("owner", nil, true, nil, noEmail(), noTime())
		assert.Nil(t, a.scopeArg(), "NULL is unrestricted; a group list must not narrow it back")
	})

	t.Run("order does not matter", func(t *testing.T) {
		a := &memberAccess{}
		a.merge("owner", nil, true, nil, noEmail(), noTime())
		a.merge("member", nil, false, []string{"tenantA_vans"}, noEmail(), noTime())
		assert.Nil(t, a.scopeArg())
	})

	t.Run("two group sets combine", func(t *testing.T) {
		a := &memberAccess{}
		a.merge("member", nil, false, []string{"tenantA_vans"}, noEmail(), noTime())
		a.merge("member", nil, false, []string{"tenantA_north", "tenantA_vans"}, noEmail(), noTime())
		assert.Equal(t, pq.Array([]string{"tenantA_north", "tenantA_vans"}), a.scopeArg(),
			"deduplicated and sorted, so a re-run writes identical bytes")
	})

	// The inversion that makes this dangerous: an empty array is "sees nothing",
	// nil is "sees everything". Restricted-with-no-groups must never become nil.
	t.Run("restricted with no groups is an empty array, never nil", func(t *testing.T) {
		a := &memberAccess{}
		a.merge("member", nil, false, nil, noEmail(), noTime())
		got := a.scopeArg()
		require.NotNil(t, got, "nil here would grant the whole fleet to someone entitled to nothing")
		assert.Equal(t, pq.Array([]string{}), got)
	})
}

func TestMergeKeepsBestEmailAndLatestLogin(t *testing.T) {
	early := sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	late := sql.NullTime{Time: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), Valid: true}

	a := &memberAccess{}
	a.merge("member", nil, false, nil, noEmail(), late)
	a.merge("member", nil, false, nil, sql.NullString{String: "x@example.com", Valid: true}, early)

	assert.Equal(t, "x@example.com", a.email.String, "an email from either source is better than none")
	assert.Equal(t, late.Time, a.lastLogin.Time, "the most recent login wins regardless of merge order")
}

func TestRoleRankOrdering(t *testing.T) {
	assert.Greater(t, roleRank("owner"), roleRank("admin"))
	assert.Greater(t, roleRank("admin"), roleRank("member"))
	assert.Equal(t, roleRank("member"), roleRank("anything-unrecognised"),
		"an unknown label must not outrank a known one")
}

// Merging must be idempotent, or a re-run would drift.
func TestMergeIsIdempotent(t *testing.T) {
	build := func(times int) *memberAccess {
		a := &memberAccess{}
		for i := 0; i < times; i++ {
			a.merge("admin", []string{models.CapReports}, false, []string{"tenantA_vans"}, noEmail(), noTime())
		}
		return a
	}
	one, three := build(1), build(3)

	p1, err := one.permissionsJSON()
	require.NoError(t, err)
	p3, err := three.permissionsJSON()
	require.NoError(t, err)

	assert.Equal(t, p1, p3)
	assert.Equal(t, one.scopeArg(), three.scopeArg())
	assert.Equal(t, one.role, three.role)
}

// Regression: roleRank("") equals roleRank("member"), so a strict > comparison
// left every plain member with an empty role. Caught by diffing production
// before and after, not by any earlier test.
func TestMergeFillsTheZeroRole(t *testing.T) {
	a := &memberAccess{}
	a.merge("member", nil, false, nil, noEmail(), noTime())
	assert.Equal(t, "member", a.role, "a plain member must not be left with an empty role")

	b := &memberAccess{}
	b.merge("owner", nil, false, nil, noEmail(), noTime())
	b.merge("member", nil, false, nil, noEmail(), noTime())
	assert.Equal(t, "owner", b.role, "and filling the zero value must not let a lesser role win later")
}
