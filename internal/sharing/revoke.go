package sharing

import (
	"context"
	"fmt"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/ethereum/go-ethereum/common"
	"github.com/riverqueue/river"
	"github.com/rs/zerolog"
)

// RevokeArgs is one revocation of a vehicle share, queued.
//
// It carries no duration, which is the whole difference from ShareArgs: a
// revocation has nothing to express but which grant to end. Like a share, it
// carries neither the owner nor the signer key — everything that authorizes the
// call is re-resolved inside Work, for the same reason.
//
// A SEPARATE JOB KIND RATHER THAN A FLAG ON ShareArgs. The two write the same
// contract function with different arguments, so a boolean would have worked
// and been fewer lines. It is separate because the audit trail is the point:
// "who un-shared this vehicle, and when" is a question someone will ask, and
// the answer should be a row you can select by kind rather than a field you
// have to remember to filter on. It also keeps the branch out of the function
// that spends gas.
type RevokeArgs struct {
	TenantID string `json:"tenantId"`
	TokenID  int64  `json:"tokenId"`
	Grantee  string `json:"grantee"`

	// ActorWallet is the member who asked, carried for the audit trail only,
	// on the same reasoning as ShareArgs.ActorWallet.
	ActorWallet string `json:"actorWallet"`
}

func (RevokeArgs) Kind() string { return "vehicle_share_revoke" }

// InsertOpts pins the job to the sharing queue with MaxAttempts 1.
//
// ONE ATTEMPT, AND THE OBVIOUS ARGUMENT FOR MORE IS WRONG. Revoking is
// idempotent on-chain — writing a zeroed record over a zeroed record changes
// nothing — so a retry looks free, and a failed revocation is worse than a
// failed grant because the permission stays live. Both true, and still not
// enough.
//
// The failure mode a retry introduces: the revocation lands, the receipt poll
// times out, the customer re-shares that vehicle within the retry window, and
// the retry then zeroes the NEW grant. The customer sees a share they made
// successfully disappear with nothing recorded against it. That is a worse
// failure than a revocation the customer can see failed and repeat — which is
// exactly the trade ShareArgs.InsertOpts makes, for the same reason, in the
// other direction.
func (RevokeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueName, MaxAttempts: 1}
}

// RevokeWorker writes the zeroed SACD record that ends a share.
type RevokeWorker struct {
	river.WorkerDefaults[RevokeArgs]

	logger     zerolog.Logger
	settings   *config.Settings
	authorizer Authorizer
	fleet      fleetCaller
}

func NewRevokeWorker(logger *zerolog.Logger, settings *config.Settings,
	authorizer Authorizer, fleet fleetCaller) *RevokeWorker {
	return &RevokeWorker{
		logger:     logger.With().Str("component", "revoke-worker").Logger(),
		settings:   settings,
		authorizer: authorizer,
		fleet:      fleet,
	}
}

// Timeout matches the share worker's, and for the same reasons: it must sit
// above the receipt-polling window (5s × 60 = 5 minutes) with margin for the
// authorization round-trips, and below Queue.rescueStuckJobsAfter so River
// cannot rescue a job that is still legitimately running.
func (w *RevokeWorker) Timeout(*river.Job[RevokeArgs]) time.Duration { return 10 * time.Minute }

// Work performs the revocation.
//
// Same order as a share — re-authorize, build, send — and deliberately the same
// authorization chain. That is worth stating plainly because the instinct is to
// relax it: taking access away feels safer than granting it, so why re-check
// entitlement and the signer?
//
// Because the call is still a UserOperation sent from the owner's kernel
// account and signed with the tenant's key. If the vehicle has left the
// tenant's fleet, or the owner has revoked this tenant's signer, then this
// service has no standing to write to that vehicle's SACD record AT ALL — not
// to grant, and not to revoke. The check is about who may act, not about which
// direction they are acting in.
func (w *RevokeWorker) Work(ctx context.Context, job *river.Job[RevokeArgs]) error {
	args := job.Args
	log := w.logger.With().
		Int64("job_id", job.ID).
		Str("tenant_id", args.TenantID).
		Int64("token_id", args.TokenID).
		Str("grantee", args.Grantee).
		Logger()

	if !common.IsHexAddress(args.Grantee) {
		// Unreachable through the endpoint. Checked again for the same reason
		// the share worker checks it: this is the last point before calldata is
		// built, and a malformed grantee here writes a zeroed record against an
		// address nobody holds while leaving the real grant untouched — a
		// revocation that reports success and revokes nothing.
		return fmt.Errorf("grantee %q is not a hex address", args.Grantee)
	}
	grantee := common.HexToAddress(args.Grantee)

	owner, signerPK, err := w.authorizer.AuthorizeShare(ctx, args.TenantID, args.TokenID)
	if err != nil {
		log.Warn().Err(err).Msg("revoke not authorized at execution time")
		return fmt.Errorf("authorize revoke: %w", err)
	}

	msg, err := BuildSetPermissionsCall(
		common.HexToAddress(w.settings.SacdAddress),
		common.HexToAddress(w.settings.VehicleNftAddress),
		// No source: a revocation grants nothing, so there is no document to
		// point at. Leaving the previous grant's URI in place would advertise
		// agreements that no longer hold.
		args.TokenID, grantee, NoPermissions(), RevokedExpiration(), "")
	if err != nil {
		return fmt.Errorf("build setPermissions call: %w", err)
	}

	result, err := w.fleet.SendCall(ctx, owner, signerPK, msg, true)
	if err != nil {
		log.Error().Err(err).Str("owner", owner.Hex()).Msg("revoke UserOp failed")
		return fmt.Errorf("send revoke UserOp: %w", err)
	}
	if result == nil || result.Receipt == nil {
		// Same shape as the share worker's no-receipt case and the same
		// meaning: unknown, not failed. Here the stake is inverted — the
		// customer believes access is gone when it may not be — so the message
		// says so rather than leaving them to infer it.
		return fmt.Errorf("revoke UserOp returned no receipt; the share may or may not still be live — check the chain")
	}

	event := log.Info().Str("owner", owner.Hex())
	if result.Receipt.TransactionHash != nil {
		event = event.Str("tx_hash", result.Receipt.TransactionHash.String())
	}
	event.Msg("vehicle share revoked on chain")
	return nil
}

// revokeKind exists to keep the compiler honest, exactly as riverJobKind does
// for ShareArgs: if RevokeArgs.Kind ever changes, the queue and the status
// reader must move together.
var _ river.JobArgs = RevokeArgs{}
