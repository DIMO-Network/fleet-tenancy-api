package sharing

import (
	"math/big"
	"time"
)

// Permission is a SACD permission, numbered as the DIMO contracts and the
// frontend number them. The value IS the position: permission n occupies bits
// 2n and 2n+1 of the mask.
//
// The two low bits (0 and 1) are reserved and always zero, which is why the
// first real permission starts at 1 rather than 0.
type Permission int

const (
	NonLocationTelemetry Permission = iota + 1
	Commands
	CurrentLocation
	AllTimeLocation
	Credentials
	Streams
	RawData
	ApproximateLocation
)

// Permissions packs a set of permissions into the SACD bitmask.
//
// The encoding is two bits per permission — "11" granted, "00" not — with two
// reserved low bits, which is what b2b-fleet-mgr-app's sacdPermissionValue
// produces and what the contracts read. Writing it as a shift per permission
// rather than as a string of bit pairs makes the position of each permission
// checkable against the enum above; the string form in the frontend builds the
// mask most-significant-first, which is easy to transcribe backwards.
//
// Cross-checked against the frontend for both the default set and the full
// set: the full mask here equals 0xFFFF<<2, the constant kaufmann-oracle uses.
func Permissions(granted ...Permission) *big.Int {
	mask := new(big.Int)
	for _, p := range granted {
		mask.Or(mask, new(big.Int).Lsh(big.NewInt(0b11), uint(2*p)))
	}
	return mask
}

// DefaultPermissions is what a customer grants when they share a vehicle.
//
// Everything except APPROXIMATE_LOCATION, mirroring b2b-fleet-mgr-app's
// defaultPermissions. Two of these deserve to be noticed rather than inherited:
//
//   - COMMANDS is included. Sharing a vehicle is expected to include operating
//     it — lock and unlock — and b2b grants it by default. Decided 2026-08-18;
//     it is the one default that hands a stranger physical control, so change
//     it deliberately or not at all.
//   - APPROXIMATE_LOCATION is excluded, which is not a privacy win: the mask
//     already grants CURRENT_LOCATION and ALLTIME_LOCATION. It is the coarse
//     alternative to precise location, offered for grantees who should get
//     less, and it is redundant next to the precise ones.
//
// v1 exposes no permission picker, so every share made through this service
// carries exactly this mask.
func DefaultPermissions() *big.Int {
	return Permissions(defaultPermissionList()...)
}

// defaultPermissionList is DefaultPermissions before packing. The SACD
// document names each permission individually, so both forms are needed and
// they must not drift — hence one list feeding both.
func defaultPermissionList() []Permission {
	return []Permission{
		NonLocationTelemetry,
		Commands,
		CurrentLocation,
		AllTimeLocation,
		Credentials,
		Streams,
		RawData,
	}
}

// FullPermissions grants every permission, including APPROXIMATE_LOCATION.
// Equals 0xFFFF<<2. Not used by customer-initiated shares — it is here because
// it is the mask kaufmann-oracle's post-transfer re-share uses, and having both
// in one place is what makes the difference between them legible.
func FullPermissions() *big.Int {
	return Permissions(
		NonLocationTelemetry, Commands, CurrentLocation, AllTimeLocation,
		Credentials, Streams, RawData, ApproximateLocation,
	)
}

// NoPermissions is the empty mask — every permission ungranted. It is half of
// what a revocation writes; see RevokedExpiration for the other half.
func NoPermissions() *big.Int { return big.NewInt(0) }

// RevokedExpiration is the zero expiration a revocation writes alongside
// NoPermissions.
//
// EITHER ONE ALONE REVOKES, and writing both is the point. SACD's check is
//
//	block.timestamp < expiration && (permissions >> 2n) & 3 == 3
//
// so a zero mask fails the second clause and a zero expiration fails the first.
// A revocation is the one operation that must not depend on which clause a
// given contract version evaluates, or on a node's view of block.timestamp: it
// leaves a record that reads as unambiguously dead to anything inspecting it,
// on-chain or off.
//
// Note SACD grants the token's owner every permission regardless of any record
// (`ownerOf(tokenId) == grantee` short-circuits the check), so revoking an
// owner is meaningless rather than dangerous. ValidateGrantee refuses to create
// such a grant in the first place.
func RevokedExpiration() *big.Int { return big.NewInt(0) }

// indefiniteYears is how far out an "indefinite" share is set. SACD has no
// never-expires value, so both the onboarding mint and kaufmann's re-share use
// forty years and this matches them.
const indefiniteYears = 40

// ExpirationFrom converts a share duration into a SACD expiration timestamp.
// A duration of zero means indefinite.
//
// Takes the current time rather than calling time.Now so the caller controls
// the clock and the result is testable.
func ExpirationFrom(now time.Time, d time.Duration) *big.Int {
	if d <= 0 {
		return big.NewInt(now.AddDate(indefiniteYears, 0, 0).Unix())
	}
	return big.NewInt(now.Add(d).Unix())
}
