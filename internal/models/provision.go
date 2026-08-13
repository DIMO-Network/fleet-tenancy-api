package models

import "time"

// MintedToken is what GET /v1/tenants/{id}/dimo-token returns: a developer JWT
// minted from the tenant's effective credential. The credential itself never
// leaves the service — this type existing is the alternative to an endpoint
// that returns key material.
type MintedToken struct {
	Token string `json:"token"`
	// ExpiresAt is the token's own exp claim, so a caller can cache it rather
	// than calling per request. Zero when the claim is absent.
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	// ClientID names the license the token was minted from.
	ClientID string `json:"clientId"`
	// CredentialTenantID is the tenant holding that license — the subject
	// itself, or its operator when the subject is a managed customer. Callers
	// that cache per credential rather than per tenant key on this.
	CredentialTenantID string `json:"credentialTenantId"`
}

// ProvisionRequest is the body of POST /v1/tenants/{id}/members/provision:
// add a person to a tenant knowing only their email. The service resolves the
// email through accounts-api — creating the DIMO account when none exists —
// and writes the membership against the resulting wallet.
//
// Membership fields carry MemberWrite's exact semantics, including the
// three-valued scopeGroupIds that must be present. Email is required here,
// where it is optional on a direct membership write: it is the lookup key.
type ProvisionRequest struct {
	MemberWrite
}

// ProvisionResponse reports what provisioning resolved. Created distinguishes
// "found an existing DIMO account" from "registered a new one", which the
// console surfaces — a person with an existing account signs in as usual, a
// created one goes through first-login.
type ProvisionResponse struct {
	Wallet  string `json:"wallet"`
	Created bool   `json:"created"`
}
