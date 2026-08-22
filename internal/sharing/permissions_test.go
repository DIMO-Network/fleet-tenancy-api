package sharing

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bitmask is the one value in this feature that cannot be checked by
// reading the result: a wrong mask produces a share that succeeds on-chain and
// grants the wrong things. These expectations were derived by transcribing
// b2b-fleet-mgr-app's sacdPermissionValue (add-vin-element.ts) and running it,
// not by reading this implementation back to itself.
func TestPermissions_MatchesFrontendEncoding(t *testing.T) {
	// "00" reserved, then two bits per permission, most significant first:
	// APPROXIMATE_LOCATION, RAW_DATA, STREAMS, CREDENTIALS, ALLTIME_LOCATION,
	// CURRENT_LOCATION, COMMANDS, NONLOCATION_TELEMETRY.
	for name, tc := range map[string]struct {
		granted []Permission
		want    int64
	}{
		"nothing granted":      {nil, 0},
		"telemetry only":       {[]Permission{NonLocationTelemetry}, 0b1100},
		"commands only":        {[]Permission{Commands}, 0b110000},
		"approximate only":     {[]Permission{ApproximateLocation}, 0b11 << 16},
		"telemetry + commands": {[]Permission{NonLocationTelemetry, Commands}, 0b111100},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, big.NewInt(tc.want), Permissions(tc.granted...))
		})
	}
}

// 0xFFFC is what the frontend produces for its defaultPermissions — every
// permission except APPROXIMATE_LOCATION.
func TestDefaultPermissions_IsFrontendDefault(t *testing.T) {
	assert.Equal(t, big.NewInt(0xFFFC), DefaultPermissions())
}

// COMMANDS grants lock/unlock to the grantee. It is in the default set by an
// explicit decision (2026-08-18), not by inheritance, and this test exists so
// that removing it is a deliberate act with a failing test attached rather than
// a quiet edit to a list.
func TestDefaultPermissions_IncludesCommands(t *testing.T) {
	withoutCommands := Permissions(
		NonLocationTelemetry, CurrentLocation, AllTimeLocation,
		Credentials, Streams, RawData,
	)
	assert.NotEqual(t, withoutCommands, DefaultPermissions(),
		"COMMANDS is a deliberate default: sharing includes operating the vehicle")

	commandBits := new(big.Int).And(DefaultPermissions(), Permissions(Commands))
	assert.Equal(t, Permissions(Commands), commandBits)
}

// APPROXIMATE_LOCATION is the only permission left out, and not for privacy —
// the mask already grants precise location. Documented here so nobody "fixes"
// the omission by adding it and quietly widening every share.
func TestDefaultPermissions_ExcludesOnlyApproximateLocation(t *testing.T) {
	missing := new(big.Int).AndNot(FullPermissions(), DefaultPermissions())
	assert.Equal(t, Permissions(ApproximateLocation), missing)
}

// The full mask must equal 0xFFFF<<2, the constant kaufmann-oracle uses for its
// post-transfer re-share. If the two ever diverge, one of the services is
// granting something the other is not.
func TestFullPermissions_MatchesKaufmannConstant(t *testing.T) {
	assert.Equal(t, new(big.Int).Lsh(big.NewInt(0xFFFF), 2), FullPermissions())
}

// The two low bits are reserved and must stay clear whatever is granted.
func TestPermissions_ReservedLowBitsAlwaysClear(t *testing.T) {
	for _, mask := range []*big.Int{DefaultPermissions(), FullPermissions(), Permissions(Commands)} {
		assert.Zero(t, new(big.Int).And(mask, big.NewInt(0b11)).Sign(),
			"the two low bits are reserved")
	}
}

func TestExpirationFrom(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	t.Run("a duration expires that far out", func(t *testing.T) {
		got := ExpirationFrom(now, 24*time.Hour)
		require.Equal(t, big.NewInt(now.Add(24*time.Hour).Unix()), got)
	})

	// SACD has no never-expires value, so "indefinite" is forty years — the
	// same convention as the onboarding mint and kaufmann's re-share.
	t.Run("zero means forty years, matching the mint", func(t *testing.T) {
		got := ExpirationFrom(now, 0)
		assert.Equal(t, big.NewInt(now.AddDate(40, 0, 0).Unix()), got)
	})

	t.Run("a negative duration is indefinite, not in the past", func(t *testing.T) {
		got := ExpirationFrom(now, -time.Hour)
		assert.Equal(t, big.NewInt(now.AddDate(40, 0, 0).Unix()), got,
			"a share must never be created already expired")
	})
}

// A revocation writes both zeroes. Each alone would revoke — SACD checks
// `block.timestamp < expiration && (permissions >> 2n) & 3 == 3`, so a zero
// mask fails the second clause and a zero expiration the first — and writing
// both is what makes the record unambiguously dead regardless of which clause
// a given contract version or indexer looks at.
func TestRevocationWritesBothZeroes(t *testing.T) {
	assert.Equal(t, big.NewInt(0), NoPermissions(), "an empty mask grants nothing")
	assert.Equal(t, big.NewInt(0), RevokedExpiration(), "a zero expiration is already past")

	// The inverse property is the one that matters: neither granting mask may
	// ever collide with the revoking one, or a revocation would re-grant.
	assert.NotEqual(t, NoPermissions(), DefaultPermissions())
	assert.NotEqual(t, NoPermissions(), FullPermissions())
}

// ExpirationFrom must never produce the revoking expiration, or an ordinary
// share would land already dead and read as a silent failure.
func TestExpirationFromNeverProducesARevokedRecord(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	for _, d := range []time.Duration{0, -time.Hour, time.Hour, 365 * 24 * time.Hour} {
		assert.NotEqual(t, RevokedExpiration(), ExpirationFrom(now, d),
			"a share's expiration must never equal the revoking one")
	}
}
