package sharing

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/go-transactions/contracts/sacd"
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
	owner     common.Address
	pk        *ecdsa.PrivateKey
	ownerMode bool
	err       error
	calls     int
}

func (s *stubAuthorizer) AuthorizeShare(context.Context, string, int64) (common.Address, *ecdsa.PrivateKey, bool, error) {
	s.calls++
	if s.err != nil {
		return common.Address{}, nil, false, s.err
	}
	return s.owner, s.pk, s.ownerMode, nil
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

// stubOwnerCaller mirrors stubFleet for the owner-mode path, so a test can
// assert which of the two signing paths a job took.
type stubOwnerCaller struct {
	calls  int
	wallet common.Address
	pk     *ecdsa.PrivateKey
	msg    *ethereum.CallMsg
	result *zerodev.UserOperationResult
	err    error
}

func (s *stubOwnerCaller) SendOwnerCall(_ context.Context, wallet common.Address, pk *ecdsa.PrivateKey,
	msg *ethereum.CallMsg, _ bool) (*zerodev.UserOperationResult, error) {
	s.calls++
	s.wallet, s.pk, s.msg = wallet, pk, msg
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
	w := NewShareWorker(&logger, settings, auth, fleet, nil)
	w.now = func() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) }
	// No SACD_UPLOAD_URL above, so the real publisher fails every time; the
	// chain read it then falls back to must not dial. Default: the grantee
	// has no record, which is a first share.
	w.readGrant = func(context.Context, int64, common.Address) (sacd.ISacdPermissionRecord, error) {
		return sacd.ISacdPermissionRecord{}, nil
	}
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

// liveGrant is the record an upgrade overwrites: unexpired, the default mask,
// and the document the grantee's glovebox access hangs on.
func liveGrant() sacd.ISacdPermissionRecord {
	return sacd.ISacdPermissionRecord{
		Permissions: DefaultPermissions(),
		Expiration:  big.NewInt(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).Unix()),
		TemplateId:  big.NewInt(0),
		Source:      "ipfs://bafyexisting",
	}
}

// grantReads stubs readGrant and counts calls, so a test can assert whether
// the chain was consulted at all.
type grantReads struct {
	rec   sacd.ISacdPermissionRecord
	err   error
	calls int
}

func (g *grantReads) read(context.Context, int64, common.Address) (sacd.ISacdPermissionRecord, error) {
	g.calls++
	return g.rec, g.err
}

func publishing(source string, err error) func(context.Context, common.Address, common.Address, int64,
	*big.Int, *ecdsa.PrivateKey) (string, error) {
	return func(context.Context, common.Address, common.Address, int64, *big.Int, *ecdsa.PrivateKey) (string, error) {
		return source, err
	}
}

// expectedCall is the calldata a default 365-day share from the fixture sends
// with the given source.
func expectedCall(t *testing.T, source string) []byte {
	t.Helper()
	want, err := BuildSetPermissionsCall(
		common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d"),
		common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF"),
		42, testGrantee, DefaultPermissions(),
		ExpirationFrom(time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC), 365*24*time.Hour), source)
	require.NoError(t, err)
	return want.Data
}

// A first share keeps the best-effort behaviour: an assets.dimo.org blip must
// not turn into "you cannot share your vehicle", so the grant goes out with no
// source.
func TestShareWorker_NewShareSurvivesPublishFailureWithEmptySource(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	fleet := &stubFleet{result: receipt()}
	w := workerFixture(t, &stubAuthorizer{owner: testOwner, pk: pk}, fleet)
	w.publishSource = publishing("", errors.New("SACD upload returned 503"))
	reads := &grantReads{}
	w.readGrant = reads.read

	require.NoError(t, w.Work(context.Background(), job(validArgs())))

	assert.Equal(t, 1, reads.calls, "a failed publish must check what it would overwrite")
	require.Equal(t, 1, fleet.calls)
	assert.Equal(t, expectedCall(t, ""), fleet.msg.Data)
}

// THE BUG THIS GUARDS. setPermissions overwrites the whole record, so an
// upgrade sent with an empty source erases the grantee's document and with it
// their glovebox — while the customer is told the upgrade worked. The job must
// fail instead, before anything reaches the bundler.
func TestShareWorker_RefusesToOverwriteLiveGrantWhenPublishFails(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	fleet := &stubFleet{result: receipt()}
	w := workerFixture(t, &stubAuthorizer{owner: testOwner, pk: pk}, fleet)
	uploadErr := errors.New("SACD upload returned 503")
	w.publishSource = publishing("", uploadErr)
	w.readGrant = (&grantReads{rec: liveGrant()}).read

	err := w.Work(context.Background(), job(validArgs()))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrShareDocumentRequired)
	assert.NotContains(t, err.Error(), uploadErr.Error(), "causes go to the log, not the customer")
	assert.Zero(t, fleet.calls, "the existing grant must be left untouched")
}

// The same guard in owner mode: the refusal is decided before the send path
// is chosen, so neither caller may be reached.
func TestShareWorker_RefusesToOverwriteLiveGrantInOwnerMode(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	fleet := &stubFleet{result: receipt()}
	ownerCli := &stubOwnerCaller{result: receipt()}
	logger := zerolog.Nop()
	w := NewShareWorker(&logger, workerFixture(t, nil, nil).settings,
		&stubAuthorizer{owner: testOwner, pk: pk, ownerMode: true}, fleet, ownerCli)
	w.publishSource = publishing("", errNoUploadURL)
	w.readGrant = (&grantReads{rec: liveGrant()}).read

	err := w.Work(context.Background(), job(validArgs()))

	require.ErrorIs(t, err, ErrShareDocumentRequired)
	assert.Zero(t, fleet.calls)
	assert.Zero(t, ownerCli.calls)
}

// A successful publish is the normal upgrade: the new document replaces the
// old one, and the chain is not read at all.
func TestShareWorker_OverwritesLiveGrantWithNewSource(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	fleet := &stubFleet{result: receipt()}
	w := workerFixture(t, &stubAuthorizer{owner: testOwner, pk: pk}, fleet)
	w.publishSource = publishing("ipfs://bafynew", nil)
	reads := &grantReads{rec: liveGrant()}
	w.readGrant = reads.read

	require.NoError(t, w.Work(context.Background(), job(validArgs())))

	assert.Zero(t, reads.calls, "a published document needs no check of what it replaces")
	require.Equal(t, 1, fleet.calls)
	assert.Equal(t, expectedCall(t, "ipfs://bafynew"), fleet.msg.Data)
}

// Not knowing whether a grant exists is not permission to overwrite it: a
// failed read takes the safe path and fails the job.
func TestShareWorker_FailsSafeWhenGrantReadFails(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	fleet := &stubFleet{result: receipt()}
	w := workerFixture(t, &stubAuthorizer{owner: testOwner, pk: pk}, fleet)
	w.publishSource = publishing("", errors.New("SACD upload returned 503"))
	w.readGrant = (&grantReads{err: errors.New("rpc: connection refused")}).read

	err := w.Work(context.Background(), job(validArgs()))

	require.ErrorIs(t, err, ErrShareDocumentRequired)
	assert.Zero(t, fleet.calls)
}

// The job's error is served to the customer through the status endpoint, and
// a failed RPC call renders its URL — API key included. Neither cause may
// reach the returned error.
func TestShareWorker_RefusalDoesNotLeakRPCURL(t *testing.T) {
	const secretURL = "https://polygon-mainnet.example/v2/SECRET-API-KEY"
	transport := &url.Error{Op: "Post", URL: secretURL, Err: errors.New("dial tcp: i/o timeout")}

	for name, reads := range map[string]*grantReads{
		"read fails": {err: fmt.Errorf("call currentPermissionRecord: %w", transport)},
		"live grant": {rec: liveGrant()},
	} {
		t.Run(name, func(t *testing.T) {
			pk, _ := crypto.GenerateKey()
			fleet := &stubFleet{result: receipt()}
			w := workerFixture(t, &stubAuthorizer{owner: testOwner, pk: pk}, fleet)
			w.publishSource = publishing("", fmt.Errorf("sign SACD document: %w", transport))
			w.readGrant = reads.read

			err := w.Work(context.Background(), job(validArgs()))

			require.ErrorIs(t, err, ErrShareDocumentRequired)
			assert.NotContains(t, err.Error(), "SECRET-API-KEY")
			assert.Zero(t, fleet.calls)
		})
	}
}

// A grant that is revoked or expired has nothing left to lose, so re-sharing
// to that grantee is a new share and keeps the best-effort path.
func TestShareWorker_DeadGrantIsTreatedAsNewShare(t *testing.T) {
	pk, _ := crypto.GenerateKey()
	for name, rec := range map[string]sacd.ISacdPermissionRecord{
		"revoked": {Permissions: NoPermissions(), Expiration: RevokedExpiration(), TemplateId: big.NewInt(0)},
		"expired": {Permissions: DefaultPermissions(), TemplateId: big.NewInt(0), Source: "ipfs://bafyold",
			Expiration: big.NewInt(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix())},
	} {
		t.Run(name, func(t *testing.T) {
			fleet := &stubFleet{result: receipt()}
			w := workerFixture(t, &stubAuthorizer{owner: testOwner, pk: pk}, fleet)
			w.publishSource = publishing("", errors.New("SACD upload returned 503"))
			w.readGrant = (&grantReads{rec: rec}).read

			require.NoError(t, w.Work(context.Background(), job(validArgs())))
			assert.Equal(t, expectedCall(t, ""), fleet.msg.Data)
		})
	}
}
