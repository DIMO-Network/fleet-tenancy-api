package models

// ShareableOwnersInput asks which of a caller's vehicle owners this tenant may
// sign for.
//
// The owners are supplied by the caller rather than derived here because the
// caller already has them: fleet-lite stores owner_address per vehicle, while
// this service's entitlement rows hold token ids and nothing else. Deriving
// them here would mean an identity-api lookup per vehicle to rebuild a fact the
// caller was already holding.
type ShareableOwnersInput struct {
	Owners []string `json:"owners"`
}

// ShareableOwnersResult is the subset of the submitted owners whose kernel
// accounts registered this tenant's signer — i.e. the owners whose vehicles can
// be shared without the owner's own passkey.
//
// Addresses come back EIP-55 checksummed regardless of how they were sent, so
// the caller can compare them against its own stored addresses as strings.
type ShareableOwnersResult struct {
	Owners []string `json:"owners"`
}
