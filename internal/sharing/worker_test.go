package sharing

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	zerodev "github.com/DIMO-Network/go-zerodev"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testOwner   = common.HexToAddress("0x1111111111111111111111111111111111111111")
	testGrantee = common.HexToAddress("0x2222222222222222222222222222222222222222")
)

type stubAuthorizer struct {
	owner common.Address
	pk    *ecdsa.PrivateKey
	err   error
	calls int
}

func (s *stubAuthorizer) AuthorizeShare(context.Context, string, int64) (common.Address, *ecdsa.PrivateKey, error) {
	s.calls++
	if s.err != nil {
		return common.Address{}, nil, s.err
	}
	return s.owner, s.pk, nil
}

type stubFleet struct {
	calls  int
	kernel common.Address
	pk     *ecdsa.PrivateKey
	msg    *ethereum.CallMsg
	waited bool
	result *zerodev.UserOperationResult
	err    error
}

func (s *stubFleet) SendCall(_ context.Context, kernel common.Address, pk *ecdsa.PrivateKey,
	msg *ethereum.CallMsg, waitForReceipt bool) (*zerodev.UserOperationResult, error) {
	s.calls++
	s.kernel, s.pk, s.msg, s.waited = kernel, pk, msg, waitForReceipt
	return s.result, s.err
}

func receipt() *zerodev.UserOperationResult {
	h := hexutil.Bytes(common.HexToHash("0xabc").Bytes())
	return &zerodev.UserOperationResult{Receipt: &zerodev.UserOperationReceipt{TransactionHash: &h}}
}

func workerFixture(t *testing.T, auth *stubAuthorizer, fleet *stubFleet) *ShareWorker {
	t.Helper()
	logger := zerolog.Nop()
	settings := &config.Settings{
		SacdAddress:       "0x3c152B5d96769661008Ff404224d6530FCAC766d",
		VehicleNftAddress: "0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF",
		ChainID:           137,
	}
	w := NewShareWorker(&logger, settings, auth, fleet)
	w.now = func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) }
	return w
}

// river.Job embeds *rivertype.JobRow, and the worker logs job.ID — so the row
// has to be present or every call panics before it does anything.
func job(args ShareArgs) *river.Job[ShareArgs] {
	return &river.Job[ShareArgs]{JobRow: &rivertype.JobRow{ID: 7}, Args: args}
}

func validArgs() ShareArgs {
	return ShareArgs{
		TenantID: "t1", TokenID: 42,
		Grantee: testGrantee.Hex(), DurationDays: 365,
		ActorWallet: "0x3333333333333333333333333333333333333333",
	}
}

// The share is sent FROM the owner's kernel and signed BY the tenant's signer.
// That asymmetry is the entire feature — the owner never signs — so it is
// asserted directly rather than inferred from the call succeeding.
func TestShareWorker_SendsFromOwnerKernelSignedByTenant(t *testing.T) {
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	auth := &stubAuthorizer{owner: testOwner, pk: pk}
	fleet := &stubFleet{result: receipt()}

	require.NoError(t, workerFixture(t, auth, fleet).Work(context.Background(), job(validArgs())))

	require.Equal(t, 1, fleet.calls)
	assert.Equal(t, testOwner, fleet.kernel, "the UserOp is sent from the owner's kernel account")
	assert.Equal(t, pk, fleet.pk, "signed by the tenant's signer, not the owner")
	assert.True(t, fleet.waited, "the worker must wait for a receipt or it cannot report success")
	require.NotNil(t, fleet.msg.To)
	assert.Equal(t, common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d"), *fleet.msg.To,
		"aimed at the SACD contract")
}

// THE POINT OF RE-AUTHORIZING IN THE WORKER. A job can sit in the queue while
// the vehicle is transferred or the owner revokes the signer. Acting on the
// HTTP handler's older answer would send a grant the current owner never
// agreed to, so the worker must refuse and must not call the bundler.
func TestShareWorker_RefusesWhenAuthorizationChangedSinceSubmit(t *testing.T) {
	denied := errors.New("owner account has not authorized this tenant's signer")
	auth := &stubAuthorizer{err: denied}
	fleet := &stubFleet{result: receipt()}

	err := workerFixture(t, auth, fleet).Work(context.Background(), job(validArgs()))

	require.Error(t, err)
	assert.ErrorIs(t, err, denied)
	assert.Zero(t, fleet.calls, "nothing may reach the bundler once authorization fails")
}

// A malformed grantee would pack into calldata granting permissions to an
// address nobody controls. Unreachable through the endpoint, checked anyway
// because this is the last point before the call is built.
func TestShareWorker_RejectsMalformedGranteeBeforeAuthorizing(t *testing.T) {
	auth := &stubAuthorizer{owner: testOwner}
	fleet := &stubFleet{result: receipt()}

	args := validArgs()
	args.Grantee = "not-an-address"
	err := workerFixture(t, auth, fleet).Work(context.Background(), job(args))

	require.Error(t, err)
	assert.Zero(t, auth.calls, "a malformed grantee fails before any upstream work")
	assert.Zero(t, fleet.calls)
}

// waitForReceipt is true, so no receipt means the poll window closed with the
// operation unconfirmed. Reported as failure — but the grant may still land,
// which is exactly why the job does not retry.
func TestShareWorker_MissingReceiptIsFailureNotSilentSuccess(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubAuthorizer{owner: testOwner, pk: pk}
	fleet := &stubFleet{result: &zerodev.UserOperationResult{}}

	err := workerFixture(t, auth, fleet).Work(context.Background(), job(validArgs()))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "may still land")
}

// One attempt. A retry cannot distinguish "never sent" from "sent, receipt poll
// timed out", and the second case re-sends a grant that already exists — which
// could re-grant something the customer has since revoked.
func TestShareArgs_DoesNotRetry(t *testing.T) {
	opts := ShareArgs{}.InsertOpts()
	assert.Equal(t, 1, opts.MaxAttempts, "a share must not be retried automatically")
	assert.Equal(t, QueueName, opts.Queue)
}

// The worker's timeout must exceed the receipt-polling window (5s × 60) or it
// would kill jobs whose UserOp is still in flight, and must stay under the
// rescue window or River would rescue a live job and send the grant twice.
func TestShareWorker_TimeoutSitsBetweenPollingAndRescue(t *testing.T) {
	w := workerFixture(t, &stubAuthorizer{}, &stubFleet{})
	timeout := w.Timeout(nil)

	pollWindow := time.Duration(receiptPollingDelaySeconds*receiptPollingRetries) * time.Second
	assert.Greater(t, timeout, pollWindow, "a job must not be killed while still polling for its receipt")
	assert.Less(t, timeout, rescueStuckJobsAfter, "River must not rescue a job that is still running")
}

// The expiration is anchored to when the share runs, not when it was queued,
// and a zero duration means indefinite.
func TestShareWorker_ExpirationUsesRunTime(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		days int
		want *big.Int
	}{
		"a year":     {365, big.NewInt(now.Add(365 * 24 * time.Hour).Unix())},
		"indefinite": {0, big.NewInt(now.AddDate(40, 0, 0).Unix())},
	} {
		t.Run(name, func(t *testing.T) {
			auth := &stubAuthorizer{owner: testOwner, pk: pk}
			fleet := &stubFleet{result: receipt()}
			w := workerFixture(t, auth, fleet)

			args := validArgs()
			args.DurationDays = tc.days
			require.NoError(t, w.Work(context.Background(), job(args)))

			// Rebuild the expected calldata and compare — the expiration is an
			// ABI-encoded argument, so this checks the value actually sent.
			want, err := BuildSetPermissionsCall(
				common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d"),
				common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF"),
				42, testGrantee, DefaultPermissions(), tc.want, "")
			require.NoError(t, err)
			assert.Equal(t, want.Data, fleet.msg.Data)
		})
	}
}

// Every share carries the default mask — v1 exposes no permission picker, and
// the worker must not quietly widen it to the full set.
func TestShareWorker_UsesDefaultPermissionsNotFull(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	auth := &stubAuthorizer{owner: testOwner, pk: pk}
	fleet := &stubFleet{result: receipt()}
	w := workerFixture(t, auth, fleet)
	require.NoError(t, w.Work(context.Background(), job(validArgs())))

	full, err := BuildSetPermissionsCall(
		common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d"),
		common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF"),
		42, testGrantee, FullPermissions(),
		ExpirationFrom(time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC), 365*24*time.Hour), "")
	require.NoError(t, err)
	assert.NotEqual(t, full.Data, fleet.msg.Data,
		"a customer share is the default mask, never the full one")
}
