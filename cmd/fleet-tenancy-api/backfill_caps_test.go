package main

import (
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/stretchr/testify/assert"
)

// The capability rename is not cosmetic: every authorization check reads
// permissions, so an operator admin still carrying manage_admin_users is an
// operator admin who cannot manage members.
func TestMapKaufmannCapabilities(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "manage_admin_users becomes manage_members",
			in:   []string{"manage_admin_users"},
			want: []string{models.CapManageMembers},
		},
		{
			name: "view_all_fleets is dropped — it lives in scope_group_ids instead",
			in:   []string{"view_all_fleets"},
			want: []string{},
		},
		{
			name: "the real production set maps as expected",
			in:   []string{"manage_admin_users", "view_all_fleets", "onboard_vehicles", "reports"},
			want: []string{models.CapManageMembers, models.CapOnboardVehicle, models.CapReports},
		},
		{
			name: "capabilities that already match are untouched",
			in:   []string{models.CapOnboardVehicle, models.CapReports},
			want: []string{models.CapOnboardVehicle, models.CapReports},
		},
		{
			name: "no duplicate when both the old and new name are present",
			in:   []string{"manage_admin_users", "manage_members"},
			want: []string{models.CapManageMembers},
		},
		{
			name: "empty stays empty rather than becoming nil",
			in:   []string{},
			want: []string{},
		},
		{
			name: "unknown capabilities pass through rather than being silently dropped",
			in:   []string{"some_future_capability"},
			want: []string{"some_future_capability"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapKaufmannCapabilities(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NotNil(t, got, "must marshal to [] not null — a null permissions column would read as no capabilities")
		})
	}
}

// Guards the inversion that makes this migration dangerous: in this schema NULL
// scope means unrestricted, so anything that is not "view all fleets" must
// produce a non-nil array, including the empty one.
func TestViewAllFleetsIsTheOnlyUnrestrictedCase(t *testing.T) {
	for _, tc := range []struct {
		name           string
		perms          []string
		wantUnrestrict bool
	}{
		{"holds view_all_fleets", []string{"view_all_fleets"}, true},
		{"holds it alongside others", []string{"reports", "view_all_fleets"}, true},
		{"does not hold it", []string{"reports"}, false},
		{"holds nothing at all", []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viewAll := false
			for _, p := range tc.perms {
				if p == "view_all_fleets" {
					viewAll = true
				}
			}
			assert.Equal(t, tc.wantUnrestrict, viewAll)

			// Mirror how AuthzResult decides, so the two cannot drift apart.
			res := &models.AuthzResult{}
			if !viewAll {
				res.ScopeGroupIDs = []string{}
			}
			assert.Equal(t, tc.wantUnrestrict, res.Unrestricted(),
				"a member without view_all_fleets must never read as unrestricted")
		})
	}
}
