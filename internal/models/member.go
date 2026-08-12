package models

import "encoding/json"

// MemberWrite is the body of PUT /v1/tenants/{id}/members/{wallet}.
//
// The write half of the hot path. Until this existed, kaufmann and fleet-lite
// *read* their authorization answers from this service while still *writing*
// memberships to their own tables, so anyone granted access after the cutover
// was granted it somewhere nothing consults. One source of truth for a decision
// means one place it is written, not only one place it is read.
type MemberWrite struct {
	// Email of the member, if known. Upserted onto the users row; an empty
	// value leaves any existing address alone rather than clearing it, since
	// the caller may simply not know it.
	Email string `json:"email,omitempty"`

	// Role is a display label and a preset. It is never an authorization
	// input — Permissions is. Empty defaults to "member".
	Role string `json:"role,omitempty"`

	// Permissions is authoritative. Callers send the shared vocabulary
	// (manage_members, manage_settings, onboard_vehicles, reports); a caller
	// with its own historical names must translate before calling.
	Permissions []string `json:"permissions"`

	// ScopeGroupIDs is three-valued and deliberately typed as raw JSON so all
	// three cases stay distinguishable:
	//
	//	null  -> unrestricted, sees every group
	//	[]    -> restricted to nothing
	//	[...] -> restricted to those groups
	//
	// Absent is NOT accepted, and that is the point. As a plain []string,
	// "field omitted" and "explicitly unrestricted" would both arrive as nil,
	// so a caller that forgot the field would silently grant the whole fleet.
	// That exact inversion — nil means everything, empty means nothing — handed
	// 131 memberships an entire 524-vehicle fleet during the backfill. Here it
	// is a 400 instead.
	ScopeGroupIDs json.RawMessage `json:"scopeGroupIds"`

	// GrantedByWallet records who performed the grant, for the audit trail.
	GrantedByWallet string `json:"grantedByWallet,omitempty"`
}

// Scope resolves ScopeGroupIDs into (groups, unrestricted, present).
//
// present is false when the field was omitted entirely, which callers must
// treat as a bad request rather than a default.
func (m *MemberWrite) Scope() (groups []string, unrestricted bool, present bool) {
	if len(m.ScopeGroupIDs) == 0 {
		return nil, false, false
	}
	if string(m.ScopeGroupIDs) == "null" {
		return nil, true, true
	}
	var out []string
	if err := json.Unmarshal(m.ScopeGroupIDs, &out); err != nil {
		return nil, false, false
	}
	if out == nil {
		out = []string{}
	}
	return out, false, true
}
