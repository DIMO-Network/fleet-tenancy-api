package sharing

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	zerodev "github.com/DIMO-Network/go-zerodev"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/riverqueue/river"
	"github.com/rs/zerolog"
)

// ShareArgs is one vehicle share, queued.
//
// It carries the tenant and the token id but NOT the owner, the signer key or
// the resolved permissions. Everything that authorizes the call is re-resolved
// inside Work: a job row can sit in the queue while ownership changes or a
// signer is revoked, and a serialised decision would let the worker act on an
// answer that was true when the HTTP request was served and is false now.
type ShareArgs struct {
	TenantID string `json:"tenantId"`
	TokenID  int64  `json:"tokenId"`
	Grantee  string `json:"grantee"`

	// DurationDays is 0 for an indefinite share. Stored as the customer's
	// intent rather than a computed timestamp so the expiration is anchored to
	// when the share actually lands, not to when it was queued.
	DurationDays int `json:"durationDays"`

	// ActorWallet is the member who asked, carried for the audit trail only.
	// It is not re-checked here: capability is a property of the request, and
	// re-reading it at execution time would let a membership change between
	// submit and run silently cancel work the member was entitled to start.
	ActorWallet string `json:"actorWallet"`
}

func (ShareArgs) Kind() string { return "vehicle_share" }

// InsertOpts pins the job to the sharing queue with MaxAttempts 1.
//
// One attempt, deliberately. A retry cannot tell "the UserOp never went" from
// "the UserOp landed and the receipt poll timed out", and the second case
// re-sends a grant that already exists. Overwriting an identical grant is
// harmless on-chain, but a retry after a partial failure could also re-grant
// something the customer has since revoked. A failed share the customer can
// retry deliberately is the better failure.
func (ShareArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueName, MaxAttempts: 1}
}

// Authorizer re-checks, at execution time, everything that permits a share.
//
// The interface is here rather than in service so the worker can be tested
// without a database: the implementation lives in internal/service, which owns
// the entitlement rows and the accounts-api client.
type Authorizer interface {
	// AuthorizeShare returns the vehicle's current owner, the key to sign
	// with, and whether to sign in OWNER MODE — the owner is the tenant's own
	// AA wallet and the key is its root key, sent through the kernel's sudo
	// validator — or an error if the share must not proceed. ownerMode false
	// means the key is the tenant's signer and the op goes through the owner's
	// secondary weighted-ECDSA validator, as always.
	AuthorizeShare(ctx context.Context, tenantID string, tokenID int64) (owner common.Address, signerPK *ecdsa.PrivateKey, ownerMode bool, err error)
}

// fleetCaller is the slice of go-zerodev's fleet client the worker uses, named
// so tests can substitute one and assert on the call without a bundler.
type fleetCaller interface {
	SendCall(ctx context.Context, kernel common.Address, fleetPK *ecdsa.PrivateKey,
		msg *ethereum.CallMsg, waitForReceipt bool) (*zerodev.UserOperationResult, error)
}

// ShareWorker sends the SACD grant.
type ShareWorker struct {
	river.WorkerDefaults[ShareArgs]

	logger     zerolog.Logger
	settings   *config.Settings
	authorizer Authorizer
	fleet      fleetCaller
	// owner sends owner-mode UserOps (docs/plans/08-aa-owner-signing.md), nil
	// when AA_BUNDLER_URL is unconfigured — in which case the authorizer never
	// selects owner mode and the guard in Work is unreachable belt.
	owner OwnerCaller
	now   func() time.Time

	// rpc is used only to read the kernel's EIP-712 domain when signing a
	// SACD document as the grantor. Dialled lazily and reused: shares are
	// infrequent, and a dial per job would be wasteful for a value that never
	// changes. nil until the first document is signed.
	rpcMu  sync.Mutex
	rpcCli *rpc.Client
}

// kernelRPC returns the shared RPC client, dialling on first use.
func (w *ShareWorker) kernelRPC() (*rpc.Client, error) {
	w.rpcMu.Lock()
	defer w.rpcMu.Unlock()
	if w.rpcCli != nil {
		return w.rpcCli, nil
	}
	cli, err := rpc.Dial(w.settings.RPCURL.String())
	if err != nil {
		return nil, fmt.Errorf("dial RPC for kernel signing: %w", err)
	}
	w.rpcCli = cli
	return cli, nil
}

func NewShareWorker(logger *zerolog.Logger, settings *config.Settings,
	authorizer Authorizer, fleet fleetCaller, owner OwnerCaller) *ShareWorker {
	return &ShareWorker{
		logger:     logger.With().Str("component", "share-worker").Logger(),
		settings:   settings,
		authorizer: authorizer,
		fleet:      fleet,
		owner:      owner,
		now:        time.Now,
	}
}

// Timeout bounds one attempt above the receipt-polling window (5s × 60 = 5
// minutes), with margin for the authorization round-trips in front of it.
//
// It must stay below Queue.rescueStuckJobsAfter, or River could rescue a job
// that is still legitimately running and send the grant twice.
func (w *ShareWorker) Timeout(*river.Job[ShareArgs]) time.Duration { return 10 * time.Minute }

// Work performs the share.
//
// The order is: re-authorize, build, send. Nothing about the job's own row is
// trusted except which tenant asked for what — the grantee and token id are
// the customer's request, and everything that says the request is permitted is
// resolved fresh here.
func (w *ShareWorker) Work(ctx context.Context, job *river.Job[ShareArgs]) error {
	args := job.Args
	log := w.logger.With().
		Int64("job_id", job.ID).
		Str("tenant_id", args.TenantID).
		Int64("token_id", args.TokenID).
		Str("grantee", args.Grantee).
		Logger()

	if !common.IsHexAddress(args.Grantee) {
		// Unreachable through the endpoint, which validates first. Checked
		// again because this is the last point before calldata is built, and a
		// malformed grantee packs into a call that grants permissions to an
		// address nobody controls.
		return fmt.Errorf("grantee %q is not a hex address", args.Grantee)
	}
	grantee := common.HexToAddress(args.Grantee)

	owner, signerPK, ownerMode, err := w.authorizer.AuthorizeShare(ctx, args.TenantID, args.TokenID)
	if err != nil {
		// Not wrapped in anything softer: an authorization failure at this
		// point means the world changed between submit and run — the vehicle
		// was transferred, or the owner revoked our signer — and the job must
		// fail loudly rather than proceed on the HTTP handler's older answer.
		log.Warn().Err(err).Msg("share not authorized at execution time")
		return fmt.Errorf("authorize share: %w", err)
	}
	log = log.With().Str("mode", shareModeName(ownerMode)).Logger()

	expiration := ExpirationFrom(w.now(), time.Duration(args.DurationDays)*24*time.Hour)

	// Publish the SACD document and point the grant at it. Without this the
	// grantee gets telemetry and no documents — permission bits say nothing
	// about the glovebox; the cloudevent agreements in this document do.
	//
	// Best-effort on purpose. A failed upload degrades to the empty source we
	// shipped before, which is a share that works for everything except
	// documents. Failing the whole job instead would turn an assets.dimo.org
	// blip into "you cannot share your vehicle at all".
	source := w.sacdSource(ctx, log, owner, grantee, args.TokenID, expiration, signerPK)

	msg, err := BuildSetPermissionsCall(
		common.HexToAddress(w.settings.SacdAddress),
		common.HexToAddress(w.settings.VehicleNftAddress),
		args.TokenID, grantee, DefaultPermissions(), expiration, source)
	if err != nil {
		return fmt.Errorf("build setPermissions call: %w", err)
	}

	// Signer mode: sent FROM the owner's kernel account, signed BY the
	// tenant's signer — that asymmetry is the original feature; the owner
	// never signs. Owner mode: the owner IS the tenant's AA wallet and the
	// key is its root, so the op goes through the sudo validator instead —
	// same call, different validator, and mixing the two envelopes is the
	// failure both callers exist to prevent.
	result, err := w.send(ctx, owner, signerPK, ownerMode, msg)
	if err != nil {
		log.Error().Err(err).Str("owner", owner.Hex()).Msg("share UserOp failed")
		return fmt.Errorf("send share UserOp: %w", err)
	}
	if result == nil || result.Receipt == nil {
		// waitForReceipt was true, so no receipt means the poll window closed
		// with the operation unconfirmed. Reported as a failure because that
		// is what the customer should be told — but note the grant may still
		// land afterwards, which is why MaxAttempts is 1.
		return fmt.Errorf("share UserOp returned no receipt; it may still land on chain")
	}

	event := log.Info().Str("owner", owner.Hex()).Str("expiration", expiration.String())
	if result.Receipt.TransactionHash != nil {
		event = event.Str("tx_hash", result.Receipt.TransactionHash.String())
	}
	event.Msg("vehicle share granted on chain")
	return nil
}

// shareModeName renders the mode for logs: "owner" or "signer". A word rather
// than a boolean so the log line reads as the decision it records.
func shareModeName(ownerMode bool) string {
	if ownerMode {
		return "owner"
	}
	return "signer"
}

// send dispatches to the caller the authorized mode requires.
func (w *ShareWorker) send(ctx context.Context, owner common.Address, pk *ecdsa.PrivateKey,
	ownerMode bool, msg *ethereum.CallMsg) (*zerodev.UserOperationResult, error) {
	return sendByMode(ctx, w.fleet, w.owner, owner, pk, ownerMode, msg)
}

// sendByMode routes one call to the right signing path. The guard exists for a
// state the authorizer should make unreachable — owner mode selected while the
// owner client is nil — because the alternative to failing here is signing
// with the wrong validator and burning the attempt.
func sendByMode(ctx context.Context, fleet fleetCaller, ownerCli OwnerCaller,
	owner common.Address, pk *ecdsa.PrivateKey, ownerMode bool,
	msg *ethereum.CallMsg) (*zerodev.UserOperationResult, error) {
	if !ownerMode {
		return fleet.SendCall(ctx, owner, pk, msg, true)
	}
	if ownerCli == nil {
		return nil, ErrOwnerModeNotConfigured
	}
	return ownerCli.SendOwnerCall(ctx, owner, pk, msg, true)
}

// sacdSource publishes the SACD document for a share and returns the
// `ipfs://<cid>` URI to record on chain, or "" when it cannot.
//
// Every failure path returns "" rather than an error. That is the pre-existing
// behaviour — a share with no source — so the worst case is the share we shipped
// yesterday, never a share that does not happen. Each failure is logged at warn
// with the reason, because a silent slide back to "no documents" is exactly the
// bug this method exists to fix.
func (w *ShareWorker) sacdSource(
	ctx context.Context,
	log zerolog.Logger,
	owner, grantee common.Address,
	tokenID int64,
	expiration *big.Int,
	signerPK *ecdsa.PrivateKey,
) string {
	uploadURL := w.settings.SacdUploadURL
	if uploadURL == "" {
		log.Warn().Msg("no SACD upload URL configured; sharing without document access")
		return ""
	}

	asset := VehicleAssetDID(w.settings.ChainID, common.HexToAddress(w.settings.VehicleNftAddress), tokenID)
	doc := BuildSACDDocument(owner, grantee, asset, defaultPermissionList(), w.now(), expiration, true)

	rpcCli, err := w.kernelRPC()
	if err != nil {
		log.Warn().Err(err).Msg("no RPC for kernel signing; sharing without document access")
		return ""
	}

	// Signed as the owner's kernel — the grantor token-exchange verifies
	// against — using the tenant's registered signer via ERC-1271.
	signed, err := SignSACDDocument(ctx, rpcCli, doc, owner, signerPK)
	if err != nil {
		log.Warn().Err(err).Msg("could not sign SACD document; sharing without document access")
		return ""
	}

	cid, err := UploadSACDDocument(ctx, uploadURL, signed)
	if err != nil {
		log.Warn().Err(err).Msg("could not upload SACD document; sharing without document access")
		return ""
	}

	log.Info().Str("cid", cid).Str("asset", asset).Msg("SACD document published")
	return SourceURI(cid)
}
