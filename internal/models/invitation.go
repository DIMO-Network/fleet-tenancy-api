package models

import "encoding/json"

// Invitation is the wire shape of an invitation everywhere it appears: list
// rows, create/resend responses, and the accept response. Field names follow
// fleet-lite's invitationJSON — the flow moved here, and matching names keep
// P2's client a rename away — except the scope, which carries this service's
// tri-state under its name here.
type Invitation struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	Email    string `json:"email"`
	// Role is a display label and the accept-time preset, never an
	// authorization input; accept derives the written permissions from it.
	Role      string `json:"role"`
	Status    string `json:"status"` // pending | accepted | revoked
	InvitedBy string `json:"invitedBy,omitempty"`
	// InviteeWallet is the wallet the invitation actually bound to at accept —
	// which may differ from the emailed address's expected owner.
	InviteeWallet *string `json:"inviteeWallet,omitempty"`
	// CreatedByTenantID is set when the issuing tenant was not the subject
	// tenant — the operator console's invites carry the operator here. Nil
	// means the tenant invited its own member.
	CreatedByTenantID *string `json:"createdByTenantId,omitempty"`
	// ScopeGroupIDs is the membership tri-state, verbatim: null = unrestricted,
	// [] = restricted to nothing, [...] = restricted to those groups. It flows
	// into the membership unchanged at accept. Deliberately no omitempty — the
	// null must appear on the wire, because it means something.
	ScopeGroupIDs []string `json:"scopeGroupIds"`
	// Email-delivery tracking, stamped on send and upgraded by the Postmark
	// webhook. Absent means the email never dispatched — the send failed, or
	// sending is disabled.
	EmailStatus       *string `json:"emailStatus,omitempty"`
	EmailStatusAt     *string `json:"emailStatusAt,omitempty"`
	EmailStatusDetail *string `json:"emailStatusDetail,omitempty"`
	CreatedAt         string  `json:"createdAt"`
	ExpiresAt         string  `json:"expiresAt"`
	AcceptedAt        *string `json:"acceptedAt,omitempty"`
	// EmailSent is set only on create/resend responses (true = dispatched,
	// false = saved but delivery failed). Omitted when listing.
	EmailSent *bool `json:"emailSent,omitempty"`
}

// InvitationCreate is the body of POST /v1/tenants/{id}/invitations.
type InvitationCreate struct {
	Email string `json:"email"`
	// Role is "owner" or "member"; anything else lands as "member", matching
	// the flow this ports. Owner invites are always unrestricted.
	Role string `json:"role,omitempty"`
	// Locale picks the email template language ("es" gets the Spanish alias,
	// everything else English). The inviter's locale, as in fleet-lite.
	Locale string `json:"locale,omitempty"`
	// ScopeGroupIDs is three-valued and raw for the same reason as
	// MemberWrite's: an omitted field must be a 400, not a silent grant of the
	// whole fleet. It becomes the membership's scope verbatim at accept.
	ScopeGroupIDs json.RawMessage `json:"scopeGroupIds"`
	// InvitedByWallet is the person driving the request in the calling app,
	// for the audit trail and the email's "invited by" line.
	InvitedByWallet string `json:"invitedByWallet"`
	// CreatedByTenantID is set by the controller from the *authenticated*
	// caller when it differs from the subject tenant — never from the body, so
	// the audit trail records who actually called.
	CreatedByTenantID string `json:"-"`
}

// Scope resolves ScopeGroupIDs exactly as MemberWrite.Scope does — same
// tri-state, same absent-is-an-error contract.
func (i *InvitationCreate) Scope() (groups []string, unrestricted bool, present bool) {
	return decodeScope(i.ScopeGroupIDs)
}

// InvitationAccept is the body of POST /v1/invitations/accept. There is no
// tenant in the path or body: the token resolves it. The wallet is asserted by
// the trusted service caller — the same trust the membership write-through
// already extends, since that caller could PUT the membership directly.
type InvitationAccept struct {
	Token  string `json:"token"`
	Wallet string `json:"wallet"`
	// Email of the accepting user, if the caller knows it — filled onto the
	// users row where one is missing, never overwriting.
	Email string `json:"email,omitempty"`
}
