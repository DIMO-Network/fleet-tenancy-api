package models

// Tenant kinds and statuses. Stored as text; these are the accepted values.
const (
	KindOperator = "operator"
	KindCustomer = "customer"

	StatusActive    = "active"
	StatusSuspended = "suspended"

	// EntitlementImplicit — the tenant's fleet is everything its effective
	// developer licence is privileged on. Operator and self-serve tenants.
	EntitlementImplicit = "implicit"
	// EntitlementExplicit — the tenant's fleet is the vehicle_entitlements rows
	// its operator wrote. Managed customer tenants.
	EntitlementExplicit = "explicit"
)

// DelegationScopes are the management rights an operator holds over a customer.
// Management only — a delegation never grants a fleet-lite session.
var DelegationScopes = []string{"manage_members", "manage_vehicles", "manage_settings"}

// Tenant is the wire shape of one tenant.
//
// It carries no credential material, not even encrypted. Credentials never
// leave this service; a caller that needs to act as a tenant asks for a minted
// token.
type Tenant struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Kind             string  `json:"kind"`
	ParentTenantID   *string `json:"parentTenantId"`
	Status           string  `json:"status"`
	Managed          bool    `json:"managed"`
	EntitlementMode  string  `json:"entitlementMode"`
	FleetLiteEnabled bool    `json:"fleetLiteEnabled"`
	// MembershipsEnforced hides this tenant's vehicles that have no active
	// membership from fleet-lite. Off unless an operator deliberately turns it
	// on — see the migration for why the default is load-bearing.
	MembershipsEnforced bool    `json:"membershipsEnforced"`
	ExternalRef         *string `json:"externalRef"`
	CreatedAt           string  `json:"createdAt"`

	// Counts, populated on the children listing that backs the console list.
	// Derived per request rather than stored: a denormalised counter drifts
	// from the rows it counts, and these are cheap.
	VehicleCount int     `json:"vehicleCount"`
	UserCount    int     `json:"userCount"`
	LastActivity *string `json:"lastActivityAt"`
}

// CreateTenantInput creates a customer tenant under an operator.
//
// Kind, parent, managed and entitlement mode are not accepted from the caller:
// this endpoint creates exactly one thing — a managed, explicit-mode customer
// parented to the calling operator. Letting a caller choose would let it create
// an operator, or an unparented self-serve tenant with no owner.
type CreateTenantInput struct {
	Name        string  `json:"name"`
	ExternalRef *string `json:"externalRef,omitempty"`
}

// UpdateTenantInput patches a tenant. Every field is optional; a nil pointer
// means "leave alone", which is why these are pointers rather than values —
// with plain types, "" and false are indistinguishable from absent, and
// PATCHing a name would silently clear the external ref.
type UpdateTenantInput struct {
	Name                *string `json:"name,omitempty"`
	Status              *string `json:"status,omitempty"`
	FleetLiteEnabled    *bool   `json:"fleetLiteEnabled,omitempty"`
	MembershipsEnforced *bool   `json:"membershipsEnforced,omitempty"`
	ExternalRef         *string `json:"externalRef,omitempty"`
}

// Surfaces a wallet-tenants listing can be filtered for. The surface names the
// product asking, and the filter is what membership in that product means:
// fleet-lite sessions require an active, fleet-lite-visible tenant, while the
// console works on operator tenants only.
const (
	SurfaceFleetLite = "fleet_lite"
	SurfaceB2B       = "b2b"
)

// WalletTenant is one row of "which tenants does this wallet belong to" — the
// tenant joined with the wallet's own membership in it. Direct memberships
// only: a delegation is an operator's management right and never appears in a
// tenant list a session could be opened from.
type WalletTenant struct {
	TenantID        string `json:"tenantId"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	EntitlementMode string `json:"entitlementMode"`
	Role            string `json:"role"`
	// Permissions is authoritative for what the wallet may do; Role is a label.
	Permissions []string `json:"permissions"`
	// ScopeGroupIDs nil means unrestricted; an empty array means restricted to
	// nothing. Same encoding as Member and the authz answer.
	ScopeGroupIDs []string `json:"scopeGroupIds"`
}

// Member is the wire shape of one membership.
type Member struct {
	Wallet      string   `json:"wallet"`
	Email       *string  `json:"email"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
	// ScopeGroupIDs nil means unrestricted; an empty array means restricted to
	// nothing. The two are opposites and both are meaningful.
	ScopeGroupIDs     []string `json:"scopeGroupIds"`
	GrantedByTenantID *string  `json:"grantedByTenantId"`
	GrantedByWallet   *string  `json:"grantedByWallet"`
	LastLoginAt       *string  `json:"lastLoginAt"`
	CreatedAt         string   `json:"createdAt"`
}

// Entitlement is one vehicle a tenant may see.
//
// Token id and provenance only — VIN, plate and model belong to the oracle. A
// copy of them here would be a second, staler source of fleet data, and this
// service is deliberately not one.
type Entitlement struct {
	VehicleTokenID int64  `json:"vehicleTokenId"`
	Source         string `json:"source"`
	// SourceGroupID names an OPERATOR-side fleet group used to select vehicles
	// at assign time. Provenance only, never a cross-tenant link: the
	// customer's own groups are separate and theirs. It is what makes drift
	// knowable — the caller diffs it against the group's current membership,
	// which only the caller can see.
	SourceGroupID   *string `json:"sourceGroupId"`
	GrantedByWallet *string `json:"grantedByWallet"`
	CreatedAt       string  `json:"createdAt"`
}

// AssignVehiclesInput assigns vehicles to a tenant.
//
// TokenIDs arrives already expanded. When an operator bulk-assigns a fleet
// group, the caller resolves that group to token ids against its own system —
// kaufmann's operator groups for b2b, fleet-lite's own for fleet-lite — and
// sends the list plus the group as provenance. This service records which group
// they came from and stays free of fleet-domain concepts.
type AssignVehiclesInput struct {
	TokenIDs []int64 `json:"tokenIds"`
	// FromGroupID is optional provenance for a bulk assign-by-group.
	FromGroupID string `json:"fromGroupId,omitempty"`
}

// RejectedVehicle is one vehicle an assignment could not take.
type RejectedVehicle struct {
	TokenID int64  `json:"tokenId"`
	Reason  string `json:"reason"`
	HeldBy  string `json:"heldBy"`
}

// AssignResult reports a partial success.
//
// Partial is the normal outcome, not an error: an operator selecting forty
// vehicles, two of which another customer holds, wants the thirty-eight and an
// account of the two — not a failed request with no way to tell which two.
type AssignResult struct {
	Assigned []int64           `json:"assigned"`
	Rejected []RejectedVehicle `json:"rejected"`
}
