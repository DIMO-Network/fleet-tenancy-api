package sharing

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	zerodev "github.com/DIMO-Network/go-zerodev"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func revokeFixture(t *testing.T, auth *stubAuthorizer, fleet *stubFleet) *RevokeWorker {
	t.Helper()
	logger := zerolog.Nop()
	settings := &config.Settings{
		SacdAddress:       "0x3c152B5d96769661008Ff404224d6530FCAC766d",
		VehicleNftAddress: "0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF",
		ChainID:           137,
	}
	return NewRevokeWorker(&logger, settings, auth, fleet, nil)
}

func revokeJob(args RevokeArgs) *river.Job[RevokeArgs] {
	return &river.Job[RevokeArgs]{JobRow: &rivertype.JobRow{ID: 9}, Args: args}
}

func validRevokeArgs() RevokeArgs {
	return RevokeArgs{
		TenantID: "t1", TokenID: 42,
		Grantee:     testGrantee.Hex(),
		ActorWallet: "0x3333333333333333333333333333333333333333",
	}
}

// WHAT A REVOCATION ACTUALLY WRITES, asserted against the calldata rather than
// inferred from the call succeeding. Both zeroes matter: SACD checks
// `block.timestamp < expiration && (permissions >> 2n) & 3 == 3`, so each one
// alone would revoke, and writing both is what makes the record unambiguously
// dead to anything reading it.
func TestRevokeWorker_WritesZeroedPermissionsAndExpiration(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubAuthorizer{owner: testOwner, pk: pk}
	fleet := &stubFleet{result: receipt()}

	require.NoError(t, revokeFixture(t, auth, fleet).Work(context.Background(), revokeJob(validRevokeArgs())))

	want, err := BuildSetPermissionsCall(
		common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d"),
		common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF"),
		42, testGrantee, big.NewInt(0), big.NewInt(0), "")
	require.NoError(t, err)
	assert.Equal(t, want.Data, fleet.msg.Data,
		"a revocation is setPermissions with a zero mask and a zero expiration")
}

// A revocation must never be confusable with a grant of nothing-in-particular:
// if the mask were left at the default, the call would RE-GRANT the share it
// was meant to end. This is the single most damaging way this worker could be
// wrong, so it is pinned directly.
func TestRevokeWorker_DoesNotSendAGrantingMask(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubAuthorizer{owner: testOwner, pk: pk}
	fleet := &stubFleet{result: receipt()}
	require.NoError(t, revokeFixture(t, auth, fleet).Work(context.Background(), revokeJob(validRevokeArgs())))

	for name, mask := range map[string]*big.Int{
		"default": DefaultPermissions(),
		"full":    FullPermissions(),
	} {
		granting, err := BuildSetPermissionsCall(
			common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d"),
			common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF"),
			42, testGrantee, mask, ExpirationFrom(time.Now(), 365*24*time.Hour), "")
		require.NoError(t, err)
		assert.NotEqual(t, granting.Data, fleet.msg.Data,
			"a revocation must not send the %s mask — that would re-grant the share", name)
	}
}

// Sent FROM the owner's kernel, signed BY the tenant's signer — the same
// asymmetry as a grant. Revoking is not a lesser act mechanically: it is the
// same privileged write to the same record.
func TestRevokeWorker_SendsFromOwnerKernelSignedByTenant(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubAuthorizer{owner: testOwner, pk: pk}
	fleet := &stubFleet{result: receipt()}

	require.NoError(t, revokeFixture(t, auth, fleet).Work(context.Background(), revokeJob(validRevokeArgs())))

	require.Equal(t, 1, fleet.calls)
	assert.Equal(t, testOwner, fleet.kernel)
	assert.Equal(t, pk, fleet.pk)
	assert.True(t, fleet.waited)
	require.NotNil(t, fleet.msg.To)
	assert.Equal(t, common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d"), *fleet.msg.To)
}

// THE INSTINCT THIS GUARDS AGAINST is relaxing authorization because taking
// access away feels safe. It is not: standing to write this vehicle's SACD
// record is what is being checked, not the direction of the write. If the
// vehicle left the fleet or the owner revoked our signer, this service may not
// touch the record at all.
func TestRevokeWorker_RefusesWhenAuthorizationChangedSinceSubmit(t *testing.T) {
	denied := errors.New("owner account has not authorized this tenant's signer")
	auth := &stubAuthorizer{err: denied}
	fleet := &stubFleet{result: receipt()}

	err := revokeFixture(t, auth, fleet).Work(context.Background(), revokeJob(validRevokeArgs()))

	require.Error(t, err)
	assert.ErrorIs(t, err, denied)
	assert.Zero(t, fleet.calls, "nothing may reach the bundler once authorization fails")
}

// A malformed grantee here is worse than in a grant: it would write a zeroed
// record against an address nobody holds, leave the real share live, and report
// success. Checked before any upstream work.
func TestRevokeWorker_RejectsMalformedGranteeBeforeAuthorizing(t *testing.T) {
	auth := &stubAuthorizer{owner: testOwner}
	fleet := &stubFleet{result: receipt()}

	args := validRevokeArgs()
	args.Grantee = "not-an-address"
	err := revokeFixture(t, auth, fleet).Work(context.Background(), revokeJob(args))

	require.Error(t, err)
	assert.Zero(t, auth.calls)
	assert.Zero(t, fleet.calls)
}

// No receipt means unknown, not failed — and here the stake is inverted from a
// grant's, because the customer believes access is gone when it may not be. The
// message has to say so.
func TestRevokeWorker_MissingReceiptSaysTheShareMayStillBeLive(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubAuthorizer{owner: testOwner, pk: pk}
	fleet := &stubFleet{result: &zerodev.UserOperationResult{}}

	err := revokeFixture(t, auth, fleet).Work(context.Background(), revokeJob(validRevokeArgs()))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "may or may not still be live",
		"the customer must not read an unconfirmed revocation as a completed one")
}

// One attempt, despite revocation being idempotent on-chain. The retry that
// looks free is the one that zeroes a NEW grant made between a timed-out
// receipt poll and the retry firing.
func TestRevokeArgs_DoesNotRetry(t *testing.T) {
	opts := RevokeArgs{}.InsertOpts()
	assert.Equal(t, 1, opts.MaxAttempts, "a revocation must not be retried automatically")
	assert.Equal(t, QueueName, opts.Queue)
}

// Same window as a share, and for the same two reasons at both ends.
func TestRevokeWorker_TimeoutSitsBetweenPollingAndRescue(t *testing.T) {
	w := revokeFixture(t, &stubAuthorizer{}, &stubFleet{})
	timeout := w.Timeout(nil)

	pollWindow := time.Duration(receiptPollingDelaySeconds*receiptPollingRetries) * time.Second
	assert.Greater(t, timeout, pollWindow)
	assert.Less(t, timeout, rescueStuckJobsAfter)
}

// The two kinds must stay distinct, because the status reader dispatches on
// kind and the audit trail is the reason revocation is its own job at all.
func TestRevokeArgs_KindIsDistinctFromShare(t *testing.T) {
	assert.NotEqual(t, ShareArgs{}.Kind(), RevokeArgs{}.Kind())
	assert.Equal(t, "vehicle_share_revoke", RevokeArgs{}.Kind())
}
