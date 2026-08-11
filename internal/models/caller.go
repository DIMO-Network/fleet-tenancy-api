package models

// CallerTenant is the *caller* of the service-to-service surface, resolved from
// the developer-license JWT — not the tenant a request is asking about. The two
// are separate on purpose: `/v1/authz?tenant_id=` names the subject of the
// question, while this names who is asking.
//
// Keeping both lets a handler log or, later, enforce the relationship between
// them. See the cross-tenant note in app.go.
type CallerTenant struct {
	// TenantID owns the developer license the caller authenticated with.
	TenantID string `json:"tenantId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`

	// ClientID is the license as presented, before case normalisation.
	ClientID string `json:"clientId"`

	// IsService lifts the scope check: this credential may ask about any
	// tenant, not only those whose effective credential is its own. Intended
	// for a shared proxy such as b2b-fleet-mgr-app. Default false.
	IsService bool `json:"isService"`
}

// TenantRef is the public shape of a tenant lookup — what
// GET /v1/resolve/client-id/{clientId} returns.
//
// It deliberately carries no credential material, not even the encrypted form.
// Credentials never leave this service; callers that need to act as a tenant ask
// for a minted token instead.
type TenantRef struct {
	TenantID         string  `json:"tenantId"`
	Name             string  `json:"name"`
	Kind             string  `json:"kind"`
	Status           string  `json:"status"`
	ParentTenantID   *string `json:"parentTenantId"`
	EntitlementMode  string  `json:"entitlementMode"`
	FleetLiteEnabled bool    `json:"fleetLiteEnabled"`
}
