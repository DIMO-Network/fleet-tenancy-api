package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	t0 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
)

// The rule fleet-lite's importer learned the hard way (fleet-lite-app#111):
// metadata is adopted by timestamp, never by processing order. The same group
// arriving from both sources must resolve identically whichever is read first.
func TestMergeGroupsNewerMetadataWins(t *testing.T) {
	kauf := srcGroup{id: "tA_vans", tenantID: "tA", name: "Vans", color: "#111111",
		createdAt: t0, updatedAt: t2, source: "kaufmann"}
	flite := srcGroup{id: "tA_vans", tenantID: "tA", name: "Vans (old)", color: "#222222",
		createdAt: t1, updatedAt: t1, source: "fleet-lite"}

	for name, order := range map[string][]srcGroup{
		"kaufmann first":   {kauf, flite},
		"fleet-lite first": {flite, kauf},
	} {
		t.Run(name, func(t *testing.T) {
			merged, err := mergeGroups(order)
			require.NoError(t, err)
			require.Len(t, merged, 1)
			g := merged["tA_vans"]
			assert.Equal(t, "Vans", g.name, "the newer side's name wins regardless of read order")
			assert.Equal(t, "#111111", g.color)
			assert.Equal(t, t0, g.createdAt, "created_at keeps the earlier value")
			assert.Equal(t, t2, g.updatedAt)
			assert.Equal(t, 2, g.sources)
		})
	}
}

func TestMergeGroupsRefusesTenantDisagreement(t *testing.T) {
	_, err := mergeGroups([]srcGroup{
		{id: "tA_vans", tenantID: "tA", source: "kaufmann", updatedAt: t1},
		{id: "tA_vans", tenantID: "tB", source: "fleet-lite", updatedAt: t2},
	})
	require.Error(t, err, "an id claiming two tenants is unrepresentable and must not be guessed at")
	assert.Contains(t, err.Error(), "tA_vans")
}

// UNIQUE (tenant_id, name) would abort the write mid-transaction; the check
// must instead name both ids up front. Two ids share a name legitimately when
// a group was renamed at one source and a new group took the old name.
func TestCheckNameCollisions(t *testing.T) {
	ok := map[string]*mergedGroup{
		"tA_vans": {srcGroup: srcGroup{id: "tA_vans", tenantID: "tA", name: "Vans"}},
		"tB_vans": {srcGroup: srcGroup{id: "tB_vans", tenantID: "tB", name: "Vans"}},
	}
	assert.NoError(t, checkNameCollisions(ok), "the same name in two tenants is fine")

	bad := map[string]*mergedGroup{
		"tA_vans":   {srcGroup: srcGroup{id: "tA_vans", tenantID: "tA", name: "Vans"}},
		"tA_vans-2": {srcGroup: srcGroup{id: "tA_vans-2", tenantID: "tA", name: "Vans"}},
	}
	err := checkNameCollisions(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tA_vans")
	assert.Contains(t, err.Error(), "tA_vans-2")
}

func TestMergeMembershipsUnions(t *testing.T) {
	merged := map[string]*mergedGroup{
		"tA_vans": {srcGroup: srcGroup{id: "tA_vans", tenantID: "tA", name: "Vans"}},
	}
	out, err := mergeMemberships([]groupMembership{
		{tenantID: "tA", tokenID: 1, groupID: "tA_vans", createdAt: t1}, // kaufmann
		{tenantID: "tA", tokenID: 1, groupID: "tA_vans", createdAt: t0}, // fleet-lite, same row, older
		{tenantID: "tA", tokenID: 2, groupID: "tA_vans", createdAt: t2}, // fleet-lite only
	}, merged)
	require.NoError(t, err)
	require.Len(t, out, 2, "the union, not either side alone")
	assert.Equal(t, t0, out[membKey{"tA", 1, "tA_vans"}], "duplicates keep the earliest created_at")
}

func TestMergeMembershipsRefusesCrossTenantRows(t *testing.T) {
	merged := map[string]*mergedGroup{
		"tA_vans": {srcGroup: srcGroup{id: "tA_vans", tenantID: "tA", name: "Vans"}},
	}

	_, err := mergeMemberships([]groupMembership{
		{tenantID: "tB", tokenID: 1, groupID: "tA_vans"},
	}, merged)
	require.Error(t, err, "fleet-lite's schema can represent this row; the target schema exists so it cannot")
	assert.Contains(t, err.Error(), "tA_vans")

	_, err = mergeMemberships([]groupMembership{
		{tenantID: "tA", tokenID: 1, groupID: "tA_gone"},
	}, merged)
	require.Error(t, err, "a membership of a group neither source holds")
}
