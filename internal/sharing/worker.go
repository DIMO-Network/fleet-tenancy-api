package sharing

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	zerodev "github.com/DIMO-Network/go-zerodev"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
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
	// AuthorizeShare returns the vehicle's current owner and the tenant's
	// signer key, or an error if the share must not proceed.
	AuthorizeShare(ctx context.Context, tenantID string, tokenID int64) (owner common.Address, signerPK *ecdsa.PrivateKey, err error)
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
	now        func() time.Time
}

func NewShareWorker(logger *zerolog.Logger, settings *config.Settings,
	authorizer Authorizer, fleet fleetCaller) *ShareWorker {
	return &ShareWorker{
		logger:     logger.With().Str("component", "share-worker").Logger(),
		settings:   settings,
		authorizer: authorizer,
		fleet:      fleet,
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

	owner, signerPK, err := w.authorizer.AuthorizeShare(ctx, args.TenantID, args.TokenID)
	if err != nil {
		// Not wrapped in anything softer: an authorization failure at this
		// point means the world changed between submit and run — the vehicle
		// was transferred, or the owner revoked our signer — and the job must
		// fail loudly rather than proceed on the HTTP handler's older answer.
		log.Warn().Err(err).Msg("share not authorized at execution time")
		return fmt.Errorf("authorize share: %w", err)
	}

	expiration := ExpirationFrom(w.now(), time.Duration(args.DurationDays)*24*time.Hour)
	msg, err := BuildSetPermissionsCall(
		common.HexToAddress(w.settings.SacdAddress),
		common.HexToAddress(w.settings.VehicleNftAddress),
		args.TokenID, grantee, DefaultPermissions(), expiration)
	if err != nil {
		return fmt.Errorf("build setPermissions call: %w", err)
	}

	// Sent FROM the owner's kernel account, signed BY the tenant's signer.
	// That asymmetry is the whole feature: the owner never signs.
	result, err := w.fleet.SendCall(ctx, owner, signerPK, msg, true)
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
