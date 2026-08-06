// Package models holds the wire and domain types shared across the service.
package models

// AccessVia records how an authorization answer was reached.
type AccessVia string

const (
	// ViaDirect — the wallet has a membership row in the tenant.
	ViaDirect AccessVia = "direct"
	// ViaDelegation — the wallet belongs to an operator tenant holding a
	// delegation over this one. Management only: b2b acts on it, fleet-lite
	// refuses it outright, because operator staff are b2b-only and there is no
	// impersonation.
	ViaDelegation AccessVia = "delegation"
	// ViaNone — no access.
	ViaNone AccessVia = "none"
)

// Capability strings. permissions[] is what authorization checks read; the role
// on a membership is a display label and a preset, never an authorization input.
//
// Note there is deliberately no view_all_fleets capability: it would encode the
// same fact as ScopeGroupIDs == nil, with no defined resolution when the two
// disagree. Group scope has exactly one home.
const (
	CapManageMembers  = "manage_members"
	CapManageSettings = "manage_settings"
	CapOnboardVehicle = "onboard_vehicles"
	CapReports        = "reports"
)

// AuthzResult answers "what may this wallet do in this tenant?".
type AuthzResult struct {
	TenantID string    `json:"tenantId"`
	Wallet   string    `json:"wallet"`
	Member   bool      `json:"member"`
	Role     string    `json:"role,omitempty"`
	Via      AccessVia `json:"via"`

	// Permissions is authoritative for authorization decisions.
	Permissions []string `json:"permissions"`

	// ScopeGroupIDs nil means unrestricted; a slice limits the caller to those
	// fleet groups. Matches fleet-lite's existing allowed_group_ids convention.
	ScopeGroupIDs []string `json:"scopeGroupIds"`

	// OperatorTenantID names the operator when Via is ViaDelegation.
	OperatorTenantID string `json:"operatorTenantId,omitempty"`

	TenantStatus string `json:"tenantStatus"`

	// CacheTTLSeconds is how long a caller may reuse this answer. Callers cache
	// because this is on every request in two apps; the cost is that revocation
	// is eventually consistent by up to this window.
	CacheTTLSeconds int `json:"cacheTtlSeconds"`
}

// HasCapability reports whether the result grants a capability.
func (a *AuthzResult) HasCapability(c string) bool {
	for _, p := range a.Permissions {
		if p == c {
			return true
		}
	}
	return false
}

// Unrestricted reports whether the caller sees every group in the tenant.
func (a *AuthzResult) Unrestricted() bool { return a.ScopeGroupIDs == nil }
