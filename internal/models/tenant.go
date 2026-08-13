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
	ExternalRef      *string `json:"externalRef"`
	CreatedAt        string  `json:"createdAt"`

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
	Name             *string `json:"name,omitempty"`
	Status           *string `json:"status,omitempty"`
	FleetLiteEnabled *bool   `json:"fleetLiteEnabled,omitempty"`
	ExternalRef      *string `json:"externalRef,omitempty"`
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
