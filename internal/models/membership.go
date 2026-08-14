package models

import "slices"

// Vehicle memberships — the commercial record, one per vehicle.
//
// A membership is a term the customer has paid for on a specific vehicle, and
// it can be moved to a different vehicle when that one is discontinued. It is
// deliberately separate from the entitlement: the entitlement decides whether a
// customer may SEE a vehicle, the membership decides whether it is PAID FOR.
// See the migration for why they are not one table.

// MembershipTerms are the terms an operator may buy, in months. Enforced here
// and again by a CHECK constraint on term_months, so a term that reaches a row
// is one of these.
var MembershipTerms = []int{1, 12, 24, 36, 48}

// IsValidMembershipTerm reports whether months is an offered term.
func IsValidMembershipTerm(months int) bool {
	return slices.Contains(MembershipTerms, months)
}

// Membership statuses. Computed from the clock on every read rather than
// stored: an expiry that depends on a job having run is an expiry that silently
// does not happen when the job fails.
const (
	MembershipActive       = "active"
	MembershipExpiringSoon = "expiring_soon"
	MembershipExpired      = "expired"
	MembershipCanceled     = "canceled"
)

// MembershipExpiringSoonDays is how long before expiry a membership starts
// warning. The console's whole job on expiry is to surface this early enough
// that an operator renews before the customer loses the vehicle.
const MembershipExpiringSoonDays = 30

// VehicleMembership is one membership as served on the wire.
//
// Token id only — no VIN, plate or model, for the same reason Entitlement
// carries none: fleet data belongs to the oracle, and a copy here would be a
// second, staler source of it. Callers join against their own vehicle list.
type VehicleMembership struct {
	ID             string  `json:"id"`
	VehicleTokenID int64   `json:"vehicleTokenId"`
	TermMonths     int     `json:"termMonths"`
	StartsAt       string  `json:"startsAt"`
	ExpiresAt      string  `json:"expiresAt"`
	CanceledAt     *string `json:"canceledAt"`
	Status         string  `json:"status"`
}

// MembershipList is the list response.
//
// Enforced rides along with the list so one call answers both "which vehicles
// are paid for" and "is that currently deciding what the customer sees". They
// are read together by fleet-lite on its vehicle path, and two calls could
// return answers from either side of a change.
type MembershipList struct {
	Enforced    bool                `json:"enforced"`
	Memberships []VehicleMembership `json:"memberships"`
}

// CreateMembershipInput starts a membership on a vehicle.
//
// StartsAt is optional and defaults to now. It exists so an operator recording
// a membership that was agreed earlier can date it correctly rather than
// silently giving the customer extra time.
type CreateMembershipInput struct {
	VehicleTokenID int64   `json:"vehicleTokenId"`
	TermMonths     int     `json:"termMonths"`
	StartsAt       *string `json:"startsAt,omitempty"`
}

// MoveMembershipInput moves a membership to another vehicle, carrying its
// remaining term with it — the customer paid for a period, not for a
// particular vehicle.
type MoveMembershipInput struct {
	VehicleTokenID int64 `json:"vehicleTokenId"`
}

// RenewMembershipInput extends a membership by a further term.
type RenewMembershipInput struct {
	TermMonths int `json:"termMonths"`
}
