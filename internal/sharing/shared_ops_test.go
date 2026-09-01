package sharing

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/go-transactions/contracts/sdid"
	"github.com/DIMO-Network/go-transactions/contracts/vehicleid"
	zerodev "github.com/DIMO-Network/go-zerodev"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	opSacdAddr     = common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d")
	opVehicleNft   = common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF")
	opSyntheticNft = common.HexToAddress("0x4804e8D1661cd1a1e5dDdE1ff458A7f878c0aC6D")
	opTarget       = common.HexToAddress("0x4444444444444444444444444444444444444444")
	opClientID     = common.HexToAddress("0x5555555555555555555555555555555555555555")
	opRunTime      = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
)

type stubOpAuthorizer struct {
	owner       common.Address
	pk          *ecdsa.PrivateKey
	ownerMode   bool
	err         error
	calls       int
	clientID    common.Address
	clientIDErr error
}

func (s *stubOpAuthorizer) AuthorizeShare(context.Context, string, int64) (common.Address, *ecdsa.PrivateKey, bool, error) {
	s.calls++
	if s.err != nil {
		return common.Address{}, nil, false, s.err
	}
	return s.owner, s.pk, s.ownerMode, nil
}

func (s *stubOpAuthorizer) GranteeClientID(context.Context, string) (common.Address, error) {
	if s.clientIDErr != nil {
		return common.Address{}, s.clientIDErr
	}
	return s.clientID, nil
}

type stubSignerGate struct {
	err       error
	calls     int
	lastOwner string
}

func (s *stubSignerGate) MaySignFor(_ context.Context, _, owner string) error {
	s.calls++
	s.lastOwner = owner
	return s.err
}

// sentOp records one SendCall so a test can assert on the sequence — the
// transfer op sends two UserOps and their order and kernels are the point.
type sentOp struct {
	kernel common.Address
	pk     *ecdsa.PrivateKey
	msg    *ethereum.CallMsg
	waited bool
}

type opFleet struct {
	calls   []sentOp
	results []*zerodev.UserOperationResult // by call index; missing means a receipt
	errs    []error                        // by call index; missing means nil
}

func (f *opFleet) SendCall(_ context.Context, kernel common.Address, pk *ecdsa.PrivateKey,
	msg *ethereum.CallMsg, waitForReceipt bool) (*zerodev.UserOperationResult, error) {
	i := len(f.calls)
	f.calls = append(f.calls, sentOp{kernel: kernel, pk: pk, msg: msg, waited: waitForReceipt})
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	result := receipt()
	if i < len(f.results) {
		result = f.results[i]
	}
	return result, err
}

func opWorkerFixture(t *testing.T, auth *stubOpAuthorizer, gate *stubSignerGate, fleet *opFleet) *SharedOpWorker {
	t.Helper()
	logger := zerolog.Nop()
	settings := &config.Settings{
		SacdAddress:         opSacdAddr.Hex(),
		VehicleNftAddress:   opVehicleNft.Hex(),
		SyntheticNftAddress: opSyntheticNft.Hex(),
		ChainID:             137,
	}
	w := NewSharedOpWorker(&logger, settings, auth, gate, fleet)
	w.now = func() time.Time { return opRunTime }
	return w
}

func opJob(args SharedOpArgs) *river.Job[SharedOpArgs] {
	return &river.Job[SharedOpArgs]{JobRow: &rivertype.JobRow{ID: 9}, Args: args}
}

func opArgs(op SharedOp) SharedOpArgs {
	args := SharedOpArgs{TenantID: "t1", TokenID: 42, Op: op}
	switch op {
	case OpTransferVehicle:
		args.TargetWallet = opTarget.Hex()
	case OpBurnSynthetic:
		args.SyntheticTokenID = 77
	}
	return args
}

// The op/field matrix IS the endpoint's contract, so every cell is pinned: the
// four ops with their own fields pass, and any field that belongs to a
// different op — or an op outside the enum — is refused before anything runs.
func TestSharedOpArgs_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*SharedOpArgs)
		ok     bool
	}{
		"transfer with a target":         {func(a *SharedOpArgs) {}, true},
		"burn_synthetic with an id":      {func(a *SharedOpArgs) { a.Op = OpBurnSynthetic; a.TargetWallet = ""; a.SyntheticTokenID = 77 }, true},
		"burn_vehicle bare":              {func(a *SharedOpArgs) { a.Op = OpBurnVehicle; a.TargetWallet = "" }, true},
		"grant_sacd bare":                {func(a *SharedOpArgs) { a.Op = OpGrantSacd; a.TargetWallet = "" }, true},
		"unknown op":                     {func(a *SharedOpArgs) { a.Op = "mint_vehicle"; a.TargetWallet = "" }, false},
		"empty op":                       {func(a *SharedOpArgs) { a.Op = ""; a.TargetWallet = "" }, false},
		"transfer without a target":      {func(a *SharedOpArgs) { a.TargetWallet = "" }, false},
		"transfer to a malformed target": {func(a *SharedOpArgs) { a.TargetWallet = "not-hex" }, false},
		"transfer to the zero address":   {func(a *SharedOpArgs) { a.TargetWallet = common.Address{}.Hex() }, false},
		"transfer with a synthetic id":   {func(a *SharedOpArgs) { a.SyntheticTokenID = 77 }, false},
		"burn_synthetic without an id":   {func(a *SharedOpArgs) { a.Op = OpBurnSynthetic; a.TargetWallet = "" }, false},
		"burn_synthetic with a target":   {func(a *SharedOpArgs) { a.Op = OpBurnSynthetic; a.SyntheticTokenID = 77 }, false},
		"burn_vehicle with a target":     {func(a *SharedOpArgs) { a.Op = OpBurnVehicle }, false},
		"burn_vehicle with synthetic id": {func(a *SharedOpArgs) { a.Op = OpBurnVehicle; a.TargetWallet = ""; a.SyntheticTokenID = 7 }, false},
		"grant_sacd with a target":       {func(a *SharedOpArgs) { a.Op = OpGrantSacd }, false},
		"zero token id":                  {func(a *SharedOpArgs) { a.TokenID = 0 }, false},
		"negative token id":              {func(a *SharedOpArgs) { a.TokenID = -42 }, false},
	} {
		t.Run(name, func(t *testing.T) {
			args := opArgs(OpTransferVehicle)
			tc.mutate(&args)
			err := args.Validate()
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrOpInvalid)
			}
		})
	}
}

// The transfer is two UserOps in one job, in order: safeTransferFrom sent from
// the CURRENT owner's kernel to the VehicleId contract, then the re-share sent
// from the NEW owner's kernel to the SACD contract — both signed by the same
// tenant key from the job's single authorization. That sequence staying one
// unit is the reason the op exists here instead of as two calls from the
// caller.
func TestSharedOpWorker_TransferChainsReShareOnNewOwnersKernel(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk, clientID: opClientID}
	gate := &stubSignerGate{}
	fleet := &opFleet{}

	require.NoError(t, opWorkerFixture(t, auth, gate, fleet).
		Work(context.Background(), opJob(opArgs(OpTransferVehicle))))

	require.Len(t, fleet.calls, 2, "a transfer is the move plus the chained re-share")

	transfer := fleet.calls[0]
	assert.Equal(t, testOwner, transfer.kernel, "the move is sent from the current owner's kernel")
	assert.Equal(t, pk, transfer.pk, "signed by the tenant's signer")
	assert.True(t, transfer.waited)
	require.NotNil(t, transfer.msg.To)
	assert.Equal(t, opVehicleNft, *transfer.msg.To, "safeTransferFrom is aimed at the VehicleId NFT")
	wantTransfer, err := BuildSafeTransferFromCall(opVehicleNft, testOwner, opTarget, 42)
	require.NoError(t, err)
	assert.Equal(t, wantTransfer.Data, transfer.msg.Data)

	reshare := fleet.calls[1]
	assert.Equal(t, opTarget, reshare.kernel, "the re-share runs on the NEW owner's kernel")
	assert.Equal(t, pk, reshare.pk, "same signer key — one resolution for the whole unit")
	require.NotNil(t, reshare.msg.To)
	assert.Equal(t, opSacdAddr, *reshare.msg.To, "the grant is aimed at the SACD contract")
	wantGrant, err := BuildSetPermissionsCall(opSacdAddr, opVehicleNft, 42, opClientID,
		FullPermissions(), ExpirationFrom(opRunTime, 0), "")
	require.NoError(t, err)
	assert.Equal(t, wantGrant.Data, reshare.msg.Data,
		"the tenant re-acquires FULL permissions indefinitely, matching kaufmann's shareWithTenant")

	assert.Equal(t, 1, gate.calls, "signer authority is re-checked against the new owner")
	assert.Equal(t, opTarget.Hex(), gate.lastOwner)
}

// A transfer to a wallet that never registered the tenant's signer — an
// external customer wallet — is legitimate and simply cannot be re-shared. The
// job must succeed on the transfer alone, with no second UserOp.
func TestSharedOpWorker_TransferSkipsReShareForUnsignableTarget(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk, clientID: opClientID}
	gate := &stubSignerGate{err: errors.New("owner account has not authorized this tenant's signer")}
	fleet := &opFleet{}

	require.NoError(t, opWorkerFixture(t, auth, gate, fleet).
		Work(context.Background(), opJob(opArgs(OpTransferVehicle))))

	assert.Len(t, fleet.calls, 1, "no re-share may be attempted on a kernel we cannot sign for")
}

// Once the transfer has landed, a re-share failure must not fail the job:
// recording a landed transfer as failed is exactly the state where the caller's
// bookkeeping and the chain disagree, and the standalone grant_sacd op exists
// as the recovery for the missing grant.
func TestSharedOpWorker_ReShareFailureDoesNotFailTheJob(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk, clientID: opClientID}
	fleet := &opFleet{errs: []error{nil, errors.New("bundler exploded")}}

	err := opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(opArgs(OpTransferVehicle)))

	assert.NoError(t, err, "the requested operation — the transfer — succeeded")
	assert.Len(t, fleet.calls, 2, "the re-share was attempted")
}

// A client id that cannot be resolved also only costs the re-share.
func TestSharedOpWorker_ReShareSkippedWithoutClientID(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk,
		clientIDErr: errors.New("tenant's effective credential has no usable DIMO client id")}
	fleet := &opFleet{}

	require.NoError(t, opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(opArgs(OpTransferVehicle))))
	assert.Len(t, fleet.calls, 1)
}

// If the transfer leg itself fails, the job fails and the re-share must not
// run — granting on a kernel the vehicle never reached would be acting on a
// world that does not exist.
func TestSharedOpWorker_FailedTransferStopsTheChain(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk, clientID: opClientID}
	fleet := &opFleet{errs: []error{errors.New("bundler exploded")}}

	err := opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(opArgs(OpTransferVehicle)))

	require.Error(t, err)
	assert.Len(t, fleet.calls, 1, "nothing may run after the transfer failed")
}

// The endpoint refuses target == owner synchronously, so reaching it in the
// worker means the vehicle moved between submit and run — most plausibly an
// earlier attempt whose receipt timed out after landing. The worker converges:
// skip the transfer leg, still run the re-share.
func TestSharedOpWorker_AlreadyTransferredConvergesOnReShare(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: opTarget, pk: pk, clientID: opClientID}
	fleet := &opFleet{}

	require.NoError(t, opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(opArgs(OpTransferVehicle))))

	require.Len(t, fleet.calls, 1, "no self-transfer is sent")
	assert.Equal(t, opTarget, fleet.calls[0].kernel)
	require.NotNil(t, fleet.calls[0].msg.To)
	assert.Equal(t, opSacdAddr, *fleet.calls[0].msg.To, "the one call is the re-share")
}

// burn_synthetic burns the SYNTHETIC device NFT at the synthetic contract —
// not the vehicle, and not at the vehicle contract. The calldata carries the
// caller-supplied synthetic token id, never the path's vehicle token id.
func TestSharedOpWorker_BurnSyntheticTargetsSyntheticContract(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk}
	fleet := &opFleet{}

	require.NoError(t, opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(opArgs(OpBurnSynthetic))))

	require.Len(t, fleet.calls, 1)
	call := fleet.calls[0]
	assert.Equal(t, testOwner, call.kernel, "sent from the owner's kernel")
	assert.Equal(t, pk, call.pk)
	require.NotNil(t, call.msg.To)
	assert.Equal(t, opSyntheticNft, *call.msg.To)
	assert.Equal(t, sdid.NewSdid().PackBurn(big.NewInt(77)), call.msg.Data,
		"burns synthetic token 77, not vehicle token 42")
}

// burn_vehicle burns the vehicle NFT itself, at the VehicleId contract.
func TestSharedOpWorker_BurnVehicleTargetsVehicleContract(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk}
	fleet := &opFleet{}

	require.NoError(t, opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(opArgs(OpBurnVehicle))))

	require.Len(t, fleet.calls, 1)
	call := fleet.calls[0]
	assert.Equal(t, testOwner, call.kernel)
	require.NotNil(t, call.msg.To)
	assert.Equal(t, opVehicleNft, *call.msg.To)
	want, err := vehicleid.NewVehicleid().TryPackBurn(big.NewInt(42))
	require.NoError(t, err)
	assert.Equal(t, want, call.msg.Data)
}

// Standalone grant_sacd grants the tenant's client id on the CURRENT owner's
// kernel — the recovery op for a transfer whose chained re-share was lost —
// and unlike the chained form, its failures are the job's failures.
func TestSharedOpWorker_GrantSacd(t *testing.T) {
	pk, _ := crypto.GenerateKey()

	t.Run("grants full permissions on the current owner's kernel", func(t *testing.T) {
		auth := &stubOpAuthorizer{owner: testOwner, pk: pk, clientID: opClientID}
		fleet := &opFleet{}
		require.NoError(t, opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
			Work(context.Background(), opJob(opArgs(OpGrantSacd))))

		require.Len(t, fleet.calls, 1)
		call := fleet.calls[0]
		assert.Equal(t, testOwner, call.kernel)
		require.NotNil(t, call.msg.To)
		assert.Equal(t, opSacdAddr, *call.msg.To)
		want, err := BuildSetPermissionsCall(opSacdAddr, opVehicleNft, 42, opClientID,
			FullPermissions(), ExpirationFrom(opRunTime, 0), "")
		require.NoError(t, err)
		assert.Equal(t, want.Data, call.msg.Data)
	})

	t.Run("an unresolvable client id fails the job", func(t *testing.T) {
		auth := &stubOpAuthorizer{owner: testOwner, pk: pk, clientIDErr: errors.New("no client id")}
		fleet := &opFleet{}
		err := opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
			Work(context.Background(), opJob(opArgs(OpGrantSacd)))
		require.Error(t, err)
		assert.Empty(t, fleet.calls, "nothing to grant to means nothing may be sent")
	})

	t.Run("a failed send fails the job", func(t *testing.T) {
		auth := &stubOpAuthorizer{owner: testOwner, pk: pk, clientID: opClientID}
		fleet := &opFleet{errs: []error{errors.New("bundler exploded")}}
		err := opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
			Work(context.Background(), opJob(opArgs(OpGrantSacd)))
		assert.Error(t, err, "here the grant IS the requested operation")
	})
}

// THE POINT OF RE-AUTHORIZING IN THE WORKER, inherited from the share worker
// and sharper here: a burn is irreversible in a way even a grant is not, and a
// job can sit in the queue while the vehicle is sold or the signer revoked.
func TestSharedOpWorker_RefusesWhenAuthorizationChangedSinceSubmit(t *testing.T) {
	denied := errors.New("owner account has not authorized this tenant's signer")
	auth := &stubOpAuthorizer{err: denied}
	fleet := &opFleet{}

	for _, op := range []SharedOp{OpTransferVehicle, OpBurnSynthetic, OpBurnVehicle, OpGrantSacd} {
		t.Run(string(op), func(t *testing.T) {
			fleet.calls = nil
			err := opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
				Work(context.Background(), opJob(opArgs(op)))
			require.Error(t, err)
			assert.ErrorIs(t, err, denied)
			assert.Empty(t, fleet.calls, "nothing may reach the bundler once authorization fails")
		})
	}
}

// The worker re-validates even though the endpoint already did: it is the last
// point before calldata is built, and a malformed row enqueued by any future
// path must die here rather than pack.
func TestSharedOpWorker_RevalidatesArgsBeforeAuthorizing(t *testing.T) {
	auth := &stubOpAuthorizer{owner: testOwner}
	fleet := &opFleet{}

	args := opArgs(OpBurnSynthetic)
	args.SyntheticTokenID = 0
	err := opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(args))

	require.ErrorIs(t, err, ErrOpInvalid)
	assert.Zero(t, auth.calls, "invalid args fail before any upstream work")
	assert.Empty(t, fleet.calls)
}

// No receipt is a failure, not a silent success — and the reason MaxAttempts
// stays 1: the burn may still land after the poll window.
func TestSharedOpWorker_MissingReceiptIsFailure(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubOpAuthorizer{owner: testOwner, pk: pk}
	fleet := &opFleet{results: []*zerodev.UserOperationResult{{}}}

	err := opWorkerFixture(t, auth, &stubSignerGate{}, fleet).
		Work(context.Background(), opJob(opArgs(OpBurnVehicle)))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "may still land")
}

// One attempt. Sharper than for shares: a retried burn is not idempotent in
// any useful sense — the token is gone, and the retry can only fail confusingly
// or, worse, burn something that was re-minted meanwhile.
func TestSharedOpArgs_DoesNotRetry(t *testing.T) {
	opts := SharedOpArgs{}.InsertOpts()
	assert.Equal(t, 1, opts.MaxAttempts)
	assert.Equal(t, QueueName, opts.Queue, "shared ops ride the sharing queue and its worker budget")
}

// The transfer's timeout must clear TWO receipt-poll windows — the move and
// the chained re-share — and every op's must stay under the rescue window, or
// River rescues a live job and the irreversible call is sent twice.
func TestSharedOpWorker_TimeoutsSitBetweenPollingAndRescue(t *testing.T) {
	w := opWorkerFixture(t, &stubOpAuthorizer{}, &stubSignerGate{}, &opFleet{})
	pollWindow := time.Duration(receiptPollingDelaySeconds*receiptPollingRetries) * time.Second

	transfer := w.Timeout(opJob(opArgs(OpTransferVehicle)))
	assert.Greater(t, transfer, 2*pollWindow, "a transfer is two UserOps, each with a full receipt window")
	assert.Less(t, transfer, rescueStuckJobsAfter)

	for _, op := range []SharedOp{OpBurnSynthetic, OpBurnVehicle, OpGrantSacd} {
		single := w.Timeout(opJob(opArgs(op)))
		assert.Greater(t, single, pollWindow, "%s must survive its receipt poll", op)
		assert.Less(t, single, rescueStuckJobsAfter)
	}
}
