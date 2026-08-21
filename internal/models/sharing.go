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

// ShareVehicleInput is a request to grant a wallet SACD permissions on a
// vehicle.
type ShareVehicleInput struct {
	// Grantee is the wallet receiving the permissions.
	Grantee string `json:"grantee"`

	// DurationDays is how long the grant lasts. Zero or absent means
	// indefinite, which SACD expresses as forty years — the same convention as
	// the onboarding mint.
	DurationDays int `json:"durationDays"`

	// Wallet is the member making the request, used for the capability check
	// and the audit trail. Supplied by the calling app from its session rather
	// than inferred here: this service authenticates applications, and the
	// human behind the request is the app's to assert.
	Wallet string `json:"wallet"`
}

// ShareVehicleResult is the 202 body. The share is asynchronous — it waits on a
// bundler — so the caller gets a job id and polls.
type ShareVehicleResult struct {
	JobID int64 `json:"jobId"`
}

// SharedOpInput is a request to run one typed shared-account operation on a
// vehicle. The op field names one of four known operations — transfer_vehicle,
// burn_synthetic, burn_vehicle, grant_sacd — and there is deliberately no way
// to carry calldata: the narrow shape is the security boundary, and the
// validation lives with the enum in sharing.SharedOpArgs.Validate.
type SharedOpInput struct {
	Op string `json:"op"`

	// TargetWallet receives the vehicle. transfer_vehicle only.
	TargetWallet string `json:"targetWallet"`

	// SyntheticTokenID names the synthetic device NFT to burn. burn_synthetic
	// only. Caller-supplied because the caller's device inventory is the live
	// record of which device sits on which vehicle — see sharing.SharedOpArgs.
	SyntheticTokenID int64 `json:"syntheticTokenId"`

	// ActorWallet is the member behind the request, for the audit trail.
	// Optional, unlike ShareVehicleInput.Wallet, because the expected caller
	// is another service's background worker that checked its human at its own
	// HTTP boundary — the same BFF split as invitations.
	ActorWallet string `json:"actorWallet"`
}

// SharedOpResult is the 202 body: a job id to poll, exactly like a share.
type SharedOpResult struct {
	JobID int64 `json:"jobId"`
}

// ShareStatus reports on a queued share.
//
// The shape mirrors kaufmann-oracle's single-job TransferJobStatus rather than
// its per-VIN VinsStatusResult: success is the IsSuccessful boolean, never a
// "Success" string. The two shapes coexist in kaufmann and confusing them is a
// recorded trap, so this one is deliberately the boolean.
type ShareStatus struct {
	JobID        int64    `json:"jobId"`
	State        string   `json:"state"`
	IsSuccessful bool     `json:"isSuccessful"`
	Errors       []string `json:"errors"`
}
