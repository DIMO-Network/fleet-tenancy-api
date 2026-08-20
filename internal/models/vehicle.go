package models

import "time"

// VehicleMetadataInput asks the roster what a set of vehicles IS.
//
// The token ids are supplied by the caller rather than derived here, and that
// is the whole shape of the endpoint: this is a JOIN, not a gate. The caller
// has already resolved which vehicles it may show — entitled ∩ active
// memberships ∩ group scope, every leg of it answered by this service — and is
// now asking for the make, model and owner of the set it holds. Deriving the
// set here would mean re-implementing that intersection a second time, in a
// second place, with a second opinion about group scope: precisely the
// duplication plan 07 exists to remove.
//
// Because the caller must NAME the tokens, there is no enumeration: nothing
// here lists the roster, or lets a caller walk it.
type VehicleMetadataInput struct {
	TokenIDs []int64 `json:"tokenIds"`
}

// VehicleMetadata is one roster row — the chain's answer about a vehicle,
// reconciled from identity-api rather than authored by anyone.
//
// Owner comes back EIP-55 checksummed regardless of how it was stored, so a
// caller can compare it against its own addresses as strings. Everything else
// is omitted when unknown rather than sent empty: a field this service has
// never read is different from one the chain says is blank.
type VehicleMetadata struct {
	VehicleTokenID int64      `json:"vehicleTokenId"`
	Owner          string     `json:"owner,omitempty"`
	DefinitionID   string     `json:"definitionId,omitempty"`
	Make           string     `json:"make,omitempty"`
	Model          string     `json:"model,omitempty"`
	Year           int        `json:"year,omitempty"`
	MintedAt       *time.Time `json:"mintedAt,omitempty"`
	VIN            string     `json:"vin,omitempty"`
	LicensePlate   string     `json:"licensePlate,omitempty"`

	// SyntheticDeviceTokenID and AftermarketDeviceTokenID say whether the
	// vehicle is connected, and by what. Null means the chain reports no such
	// device — a fact, not a gap, and the difference a caller renders a
	// connection indicator from.
	//
	// Token ids only. Serial, IMEI and the device's own mint time belong to
	// kaufmann's device table; this is the roster, and the boundary between
	// them is what plan 07 draws.
	SyntheticDeviceTokenID   *int64 `json:"syntheticDeviceTokenId,omitempty"`
	AftermarketDeviceTokenID *int64 `json:"aftermarketDeviceTokenId,omitempty"`

	// ReconciledAt is when identity-api last confirmed this row, so a caller
	// can see staleness as a timestamp instead of inferring it. UnseenSince is
	// set when no licence we hold returned the vehicle any more — usually a
	// revoked SACD. Neither is a reason to hide the vehicle; both are worth
	// showing to whoever asks why it looks odd.
	ReconciledAt time.Time  `json:"reconciledAt"`
	UnseenSince  *time.Time `json:"unseenSince,omitempty"`
}

// VehicleMetadataResult holds the rows the roster HAS, which may be fewer than
// were asked for.
//
// A MISSING TOKEN IS NOT AN EXCLUSION, and a caller must not treat it as one.
// The set was decided before this call; a token with no row here means only
// that the roster has not seen it yet — a vehicle entitled minutes ago, before
// the nightly reconcile. Dropping it from the rendered list would turn a
// metadata gap back into a missing vehicle, which is the bug plan 07 step 2
// fixed one layer up and step 4 must not reintroduce one layer down. See
// fleet-lite's mergeResolvedVehicles and its MetadataPending flag.
type VehicleMetadataResult struct {
	Vehicles []VehicleMetadata `json:"vehicles"`
}
