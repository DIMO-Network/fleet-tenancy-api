package sharing

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Owner mode routes through the owner caller and never touches the fleet one.
// Asserted on both workers, because sending an owner-mode op through
// fleet.SendCall would wrap the signature for the weighted-ECDSA validator the
// AA wallet does not have — the exact mix-up sendByMode exists to prevent.
func TestShareWorker_OwnerModeDispatch(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubAuthorizer{owner: testOwner, pk: pk, ownerMode: true}
	fleet := &stubFleet{}
	ownerCli := &stubOwnerCaller{result: receipt()}

	w := workerFixture(t, auth, fleet)
	w.owner = ownerCli

	require.NoError(t, w.Work(context.Background(), job(validArgs())))
	assert.Equal(t, 1, ownerCli.calls, "the owner caller carried the op")
	assert.Zero(t, fleet.calls, "the fleet caller must not see an owner-mode op")
	assert.Equal(t, testOwner, ownerCli.wallet)
	assert.Same(t, pk, ownerCli.pk, "signed with the root key the authorizer resolved")
}

func TestRevokeWorker_OwnerModeDispatch(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubAuthorizer{owner: testOwner, pk: pk, ownerMode: true}
	fleet := &stubFleet{}
	ownerCli := &stubOwnerCaller{result: receipt()}

	w := revokeFixture(t, auth, fleet)
	w.owner = ownerCli

	require.NoError(t, w.Work(context.Background(), revokeJob(validRevokeArgs())))
	assert.Equal(t, 1, ownerCli.calls)
	assert.Zero(t, fleet.calls)
}

// The belt behind the authorizer's config gate: owner mode selected with no
// owner client is a named refusal, never a wrong-validator send.
func TestShareWorker_OwnerModeWithoutClientRefuses(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubAuthorizer{owner: testOwner, pk: pk, ownerMode: true}
	fleet := &stubFleet{result: receipt()}

	w := workerFixture(t, auth, fleet) // fixture leaves w.owner nil

	err = w.Work(context.Background(), job(validArgs()))
	assert.ErrorIs(t, err, ErrOwnerModeNotConfigured)
	assert.Zero(t, fleet.calls, "falling back to the signer path would sign with the wrong key")
}

// Typed shared operations refuse owner-mode vehicles until plan 08 step 7 —
// the worker-side guard for a job that was queued before the vehicle moved
// onto the AA wallet.
func TestSharedOpWorker_RefusesOwnerMode(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk, ownerMode: true}
	fleet := &opFleet{}
	w := opWorkerFixture(t, auth, &stubSignerGate{}, fleet)

	err = w.Work(context.Background(), opJob(opArgs(OpBurnVehicle)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported yet")
	assert.Zero(t, fleet.calls, "nothing may be sent for an owner-mode vehicle")
}
