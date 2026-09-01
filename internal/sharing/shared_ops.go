package sharing

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/go-transactions/contracts/sdid"
	"github.com/DIMO-Network/go-transactions/contracts/vehicleid"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/riverqueue/river"
	"github.com/rs/zerolog"
)

// SharedOp is one of the four operations this service will sign on a shared
// account's behalf. The enum IS the security boundary — see SharedOpArgs.
type SharedOp string

const (
	// OpTransferVehicle moves the vehicle NFT to a target wallet, then chains
	// the post-transfer re-share: a SACD grant of the tenant's client id on the
	// NEW owner's kernel, so the tenant keeps its data access across the
	// transfer. One job, one signer resolution — see transferVehicle.
	OpTransferVehicle SharedOp = "transfer_vehicle"
	// OpBurnSynthetic burns the vehicle's synthetic device NFT — the on-chain
	// half of a disconnect.
	OpBurnSynthetic SharedOp = "burn_synthetic"
	// OpBurnVehicle burns the vehicle NFT itself — the on-chain half of a
	// delete, run after the synthetic device is gone.
	OpBurnVehicle SharedOp = "burn_vehicle"
	// OpGrantSacd grants the tenant's own client id full SACD permissions on
	// the vehicle, on its current owner's kernel. The standalone form of the
	// re-share that OpTransferVehicle chains — its use is recovery, when a
	// transfer landed but its chained grant did not.
	OpGrantSacd SharedOp = "grant_sacd"
)

// ErrOpInvalid covers a request whose op or fields do not describe one of the
// four known operations. A named error so the HTTP handler can map exactly
// this to a 400 and nothing else.
var ErrOpInvalid = errors.New("invalid shared operation")

// SharedOpArgs is one typed shared-account operation, queued.
//
// The body of the endpoint that enqueues this carries an operation NAME, never
// calldata, and this struct is where that boundary is enforced in the type
// system: there is no bytes field to smuggle a call through. An endpoint that
// accepted caller-supplied calldata would be a signing oracle over every
// kernel account any operator's signer can act for — a leaked caller key would
// then mean "burn any vehicle in the fleet, transfer it anywhere" rather than
// "perform one of four known operations on a vehicle whose owner authorised
// us". The enum is not ergonomics; do not generalise it.
//
// Like ShareArgs, it carries the tenant and the token ids but NOT the owner or
// the signer key. Everything that authorizes the call is re-resolved inside
// Work, because a job can sit in the queue while ownership changes or a signer
// is revoked.
type SharedOpArgs struct {
	TenantID string   `json:"tenantId"`
	TokenID  int64    `json:"tokenId"`
	Op       SharedOp `json:"op"`

	// TargetWallet receives the vehicle. transfer_vehicle only.
	TargetWallet string `json:"targetWallet,omitempty"`

	// SyntheticTokenID names the synthetic device NFT to burn. burn_synthetic
	// only, and caller-supplied deliberately: the caller (kaufmann's workers)
	// owns the vins row that is the live record of which device sits on which
	// vehicle, where this service's roster copy is a nightly reconcile that
	// could refuse a legitimate burn for being fresher than it. What bounds a
	// wrong value is the chain itself — the burn is sent from the vehicle
	// owner's kernel, which can only burn devices it is authorised over.
	SyntheticTokenID int64 `json:"syntheticTokenId,omitempty"`

	// ActorWallet is the member who asked, carried for the audit trail only,
	// exactly as on ShareArgs. It is optional here because the expected caller
	// is another service's worker, which checked its human at its own HTTP
	// boundary and does not carry the wallet into its job — see the endpoint
	// for why that check is not repeated here.
	ActorWallet string `json:"actorWallet,omitempty"`
}

func (SharedOpArgs) Kind() string { return "shared_operation" }

// InsertOpts pins the job to the sharing queue with MaxAttempts 1.
//
// The same reasoning as ShareArgs, and sharper: a retry cannot tell "the
// UserOp never went" from "it landed and the receipt poll timed out", and
// where a re-sent share merely re-grants, a re-sent burn tries to destroy a
// token that no longer exists and a re-sent transfer moves a vehicle its
// caller believes already moved. A retried burn is not idempotent in any
// useful sense. The caller decides whether to try again, knowing it failed.
func (SharedOpArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueName, MaxAttempts: 1}
}

// Validate rejects anything that is not exactly one of the four operations
// with exactly its fields.
//
// Strict about fields that belong to OTHER ops on purpose: a transfer arriving
// with a syntheticTokenId set means the caller has confused two operations,
// and performing the half we understood would be worse than refusing. Errors
// are written for the operator reading a job's error list, not for a
// stack trace.
func (a SharedOpArgs) Validate() error {
	if a.TokenID <= 0 {
		return fmt.Errorf("%w: tokenId must be positive", ErrOpInvalid)
	}
	switch a.Op {
	case OpTransferVehicle:
		if !common.IsHexAddress(a.TargetWallet) {
			return fmt.Errorf("%w: targetWallet %q is not a hex address", ErrOpInvalid, a.TargetWallet)
		}
		if common.HexToAddress(a.TargetWallet) == (common.Address{}) {
			// Transferring to the zero address is a burn wearing a transfer's
			// name. The caller that means burn must say burn_vehicle.
			return fmt.Errorf("%w: targetWallet must not be the zero address", ErrOpInvalid)
		}
		if a.SyntheticTokenID != 0 {
			return fmt.Errorf("%w: syntheticTokenId only applies to burn_synthetic", ErrOpInvalid)
		}
	case OpBurnSynthetic:
		if a.SyntheticTokenID <= 0 {
			return fmt.Errorf("%w: burn_synthetic requires a positive syntheticTokenId", ErrOpInvalid)
		}
		if a.TargetWallet != "" {
			return fmt.Errorf("%w: targetWallet only applies to transfer_vehicle", ErrOpInvalid)
		}
	case OpBurnVehicle, OpGrantSacd:
		if a.TargetWallet != "" {
			return fmt.Errorf("%w: targetWallet only applies to transfer_vehicle", ErrOpInvalid)
		}
		if a.SyntheticTokenID != 0 {
			return fmt.Errorf("%w: syntheticTokenId only applies to burn_synthetic", ErrOpInvalid)
		}
	default:
		return fmt.Errorf("%w: op must be one of transfer_vehicle, burn_synthetic, burn_vehicle, grant_sacd",
			ErrOpInvalid)
	}
	return nil
}

// OpAuthorizer is the authorization surface the shared-op worker needs: the
// same AuthorizeShare chain every share runs (entitlement, live owner, live
// signer authority, key), plus resolving the tenant's own client id — the
// grantee of grant_sacd and of the post-transfer re-share. Implemented by
// service.ShareAuthorizer; an interface here so the worker tests need no
// database.
type OpAuthorizer interface {
	AuthorizeShare(ctx context.Context, tenantID string, tokenID int64) (owner common.Address, signerPK *ecdsa.PrivateKey, ownerMode bool, err error)
	GranteeClientID(ctx context.Context, tenantID string) (common.Address, error)
}

// SignerGate answers whether the tenant may sign for an owner — the check the
// chained re-share needs against the NEW owner, whom AuthorizeShare never saw.
// Implemented by service.SharedSignerService.
type SignerGate interface {
	MaySignFor(ctx context.Context, tenantID, ownerAddress string) error
}

// SharedOpWorker performs one typed shared-account operation.
type SharedOpWorker struct {
	river.WorkerDefaults[SharedOpArgs]

	logger     zerolog.Logger
	settings   *config.Settings
	authorizer OpAuthorizer
	signer     SignerGate
	fleet      fleetCaller
	now        func() time.Time
}

func NewSharedOpWorker(logger *zerolog.Logger, settings *config.Settings,
	authorizer OpAuthorizer, signer SignerGate, fleet fleetCaller) *SharedOpWorker {
	return &SharedOpWorker{
		logger:     logger.With().Str("component", "shared-op-worker").Logger(),
		settings:   settings,
		authorizer: authorizer,
		signer:     signer,
		fleet:      fleet,
		now:        time.Now,
	}
}

// Timeout bounds one attempt above its receipt-polling windows, with margin
// for the authorization round-trips, and below Queue.rescueStuckJobsAfter — a
// rescue of a live job would send an irreversible call twice.
//
// A transfer gets more because it is two UserOps, each waiting up to the 5s ×
// 60 receipt window: the transfer itself, then the chained re-share on the new
// owner's kernel. Note the worst case sits above kaufmann's 10-minute transfer
// poll — recorded in the plan for step 4 to resolve before writing its loop.
func (w *SharedOpWorker) Timeout(job *river.Job[SharedOpArgs]) time.Duration {
	if job != nil && job.Args.Op == OpTransferVehicle {
		return 15 * time.Minute
	}
	return 10 * time.Minute
}

// Work performs the operation. The order is the share worker's: re-authorize,
// build, send — nothing about the job's own row is trusted except which tenant
// asked for what.
func (w *SharedOpWorker) Work(ctx context.Context, job *river.Job[SharedOpArgs]) error {
	args := job.Args
	log := w.logger.With().
		Int64("job_id", job.ID).
		Str("tenant_id", args.TenantID).
		Int64("token_id", args.TokenID).
		Str("op", string(args.Op)).
		Logger()

	// Unreachable through the endpoint, which validates first. Checked again
	// because this is the last point before calldata is built, and the ops
	// below each trust a different subset of these fields.
	if err := args.Validate(); err != nil {
		return err
	}

	owner, signerPK, ownerMode, err := w.authorizer.AuthorizeShare(ctx, args.TenantID, args.TokenID)
	if err != nil {
		// The world changed between submit and run — the vehicle was
		// transferred, or the owner revoked our signer. Fail loudly rather
		// than proceed on the HTTP handler's older answer.
		log.Warn().Err(err).Msg("shared operation not authorized at execution time")
		return fmt.Errorf("authorize %s: %w", args.Op, err)
	}
	if ownerMode {
		// Owner-mode shared operations are plan 08 step 7, not step 2. Refused
		// here rather than sent, because every path below signs through the
		// owner's weighted-ECDSA validator — which a tenant AA wallet does not
		// have — and the transfer op additionally chains a re-share whose
		// signer-gate logic assumes the signer arrangement. The endpoint
		// refuses these up front; this guard is for a job already queued when
		// the vehicle moved onto the AA wallet.
		log.Warn().Msg("shared operation refused: vehicle is owned by the tenant's AA wallet")
		return fmt.Errorf("%s of an AA-wallet-owned vehicle is not supported yet (plan 08 step 7)", args.Op)
	}

	switch args.Op {
	case OpTransferVehicle:
		return w.transferVehicle(ctx, log, args, owner, signerPK)
	case OpBurnSynthetic:
		msg := BuildBurnSyntheticCall(common.HexToAddress(w.settings.SyntheticNftAddress), args.SyntheticTokenID)
		return w.send(ctx, log, "burn_synthetic", owner, signerPK, msg)
	case OpBurnVehicle:
		msg, berr := BuildBurnVehicleCall(common.HexToAddress(w.settings.VehicleNftAddress), args.TokenID)
		if berr != nil {
			return fmt.Errorf("build burn calldata: %w", berr)
		}
		return w.send(ctx, log, "burn_vehicle", owner, signerPK, msg)
	case OpGrantSacd:
		return w.grantToTenant(ctx, log, args, owner, signerPK)
	}
	// Validate above admits exactly the four cases; this is the compiler's
	// backstop, not a reachable state.
	return fmt.Errorf("%w: %q", ErrOpInvalid, args.Op)
}

// transferVehicle moves the NFT, then chains the re-share on the new owner's
// kernel.
//
// The chaining is why the operation is one job rather than two calls from the
// caller: "transfer, then re-grant the tenant's access on the new owner's
// kernel" is one intention, and splitting it would give the sequence two
// signer resolutions with a window between them for the answer to change. It
// is how kaufmann's SharedAccountTransferWorker behaves today, ported whole.
//
// The re-share is best-effort, also matching kaufmann: once the transfer has
// landed, failing the job would record an operation that succeeded as failed —
// and the caller keeps a vins row whose truth is the chain's, so a "failed"
// job over a landed transfer is precisely the chain/vins disagreement the plan
// warns about. A skipped or failed re-share costs the tenant its data access
// until grant_sacd is run for the same vehicle, which is what that op is for.
func (w *SharedOpWorker) transferVehicle(ctx context.Context, log zerolog.Logger,
	args SharedOpArgs, owner common.Address, signerPK *ecdsa.PrivateKey) error {
	target := common.HexToAddress(args.TargetWallet)
	log = log.With().Str("target", target.Hex()).Logger()

	if target == owner {
		// Only reachable through a race: the endpoint refuses target == owner
		// synchronously, so the vehicle moved between submit and run — most
		// plausibly a previous attempt whose receipt poll timed out after the
		// transfer landed. The desired state holds; converge on it by running
		// only the re-share half rather than reporting failure over success.
		log.Info().Msg("vehicle already belongs to the target; skipping the transfer leg")
	} else {
		msg, err := BuildSafeTransferFromCall(common.HexToAddress(w.settings.VehicleNftAddress),
			owner, target, args.TokenID)
		if err != nil {
			return fmt.Errorf("build transfer calldata: %w", err)
		}
		if err := w.send(ctx, log, "transfer_vehicle", owner, signerPK, msg); err != nil {
			return err
		}
	}

	// The chained re-share needs its own authority check: AuthorizeShare above
	// verified the tenant may sign for the OLD owner, and the grant is sent
	// from the NEW owner's kernel. A transfer to an external wallet — one that
	// never registered our signer — is legitimate and simply cannot be
	// re-shared, so any refusal here is a skip, not a failure.
	if err := w.signer.MaySignFor(ctx, args.TenantID, target.Hex()); err != nil {
		log.Info().Err(err).Msg("new owner is not signable; skipping post-transfer re-share")
		return nil
	}
	grantee, err := w.authorizer.GranteeClientID(ctx, args.TenantID)
	if err != nil {
		log.Warn().Err(err).Msg("no usable client id; skipping post-transfer re-share")
		return nil
	}
	msg, err := w.buildTenantGrant(args.TokenID, grantee)
	if err != nil {
		log.Warn().Err(err).Msg("failed to pack re-share calldata; skipping post-transfer re-share")
		return nil
	}
	if err := w.send(ctx, log, "post-transfer re-share", target, signerPK, msg); err != nil {
		log.Warn().Err(err).Msg("post-transfer re-share failed; the transfer itself landed")
	}
	return nil
}

// grantToTenant is the standalone grant_sacd op: the same grant the transfer
// chains, on the current owner's kernel, except that here it IS the requested
// operation, so its failures fail the job instead of being logged past.
func (w *SharedOpWorker) grantToTenant(ctx context.Context, log zerolog.Logger,
	args SharedOpArgs, owner common.Address, signerPK *ecdsa.PrivateKey) error {
	grantee, err := w.authorizer.GranteeClientID(ctx, args.TenantID)
	if err != nil {
		return fmt.Errorf("resolve grantee client id: %w", err)
	}
	msg, err := w.buildTenantGrant(args.TokenID, grantee)
	if err != nil {
		return fmt.Errorf("build setPermissions call: %w", err)
	}
	return w.send(ctx, log, "grant_sacd", owner, signerPK, msg)
}

// buildTenantGrant packs the SACD grant of the tenant's client id: full
// permissions, indefinite. Both halves mirror kaufmann's shareWithTenant
// exactly — the tenant is re-acquiring its own operating access, which is the
// full mask by definition, where a customer-chosen share gets the narrower
// DefaultPermissions.
func (w *SharedOpWorker) buildTenantGrant(tokenID int64, grantee common.Address) (*ethereum.CallMsg, error) {
	return BuildSetPermissionsCall(
		common.HexToAddress(w.settings.SacdAddress),
		common.HexToAddress(w.settings.VehicleNftAddress),
		// No source: this is the tenant re-acquiring its own operating access
		// via its dev license, not a share to a person, so there is no
		// grantee-facing document.
		tokenID, grantee, FullPermissions(), ExpirationFrom(w.now(), 0), "")
}

// send submits one UserOp from the given kernel, signed by the tenant, and
// requires a receipt. Error text reaches the caller through the status
// endpoint's error list, so it names the operation rather than the plumbing.
func (w *SharedOpWorker) send(ctx context.Context, log zerolog.Logger, op string,
	kernel common.Address, signerPK *ecdsa.PrivateKey, msg *ethereum.CallMsg) error {
	result, err := w.fleet.SendCall(ctx, kernel, signerPK, msg, true)
	if err != nil {
		log.Error().Err(err).Str("kernel", kernel.Hex()).Msgf("%s UserOp failed", op)
		return fmt.Errorf("send %s UserOp: %w", op, err)
	}
	if result == nil || result.Receipt == nil {
		// waitForReceipt was true, so no receipt means the poll window closed
		// with the operation unconfirmed. Reported as a failure — but the call
		// may still land afterwards, which is why MaxAttempts is 1.
		return fmt.Errorf("%s UserOp returned no receipt; it may still land on chain", op)
	}
	event := log.Info().Str("kernel", kernel.Hex())
	if result.Receipt.TransactionHash != nil {
		event = event.Str("tx_hash", result.Receipt.TransactionHash.String())
	}
	event.Msgf("%s landed on chain", op)
	return nil
}

// BuildSafeTransferFromCall packs the ERC-721 safeTransferFrom moving vehicle
// `tokenID` from `owner` to `target`, aimed at the VehicleId NFT contract.
// Pure and network-free, like BuildSetPermissionsCall, and for the same
// reason: this is calldata a mistake makes irreversible, so it must be
// unit-testable.
func BuildSafeTransferFromCall(vehicleNft, owner, target common.Address, tokenID int64) (*ethereum.CallMsg, error) {
	callData, err := vehicleid.NewVehicleid().TryPackSafeTransferFrom(owner, target, big.NewInt(tokenID))
	if err != nil {
		return nil, fmt.Errorf("pack safeTransferFrom: %w", err)
	}
	return &ethereum.CallMsg{
		To:    &vehicleNft,
		Value: big.NewInt(0),
		Data:  callData,
	}, nil
}

// BuildBurnVehicleCall packs the vehicle NFT burn, aimed at the VehicleId
// contract.
func BuildBurnVehicleCall(vehicleNft common.Address, tokenID int64) (*ethereum.CallMsg, error) {
	callData, err := vehicleid.NewVehicleid().TryPackBurn(big.NewInt(tokenID))
	if err != nil {
		return nil, fmt.Errorf("pack burn: %w", err)
	}
	return &ethereum.CallMsg{
		To:    &vehicleNft,
		Value: big.NewInt(0),
		Data:  callData,
	}, nil
}

// BuildBurnSyntheticCall packs the synthetic device NFT burn, aimed at the
// SyntheticDeviceId contract. The sdid bindings expose no TryPackBurn; the
// panicking PackBurn is safe here because a uint256 argument cannot fail to
// pack.
func BuildBurnSyntheticCall(syntheticNft common.Address, syntheticTokenID int64) *ethereum.CallMsg {
	callData := sdid.NewSdid().PackBurn(big.NewInt(syntheticTokenID))
	return &ethereum.CallMsg{
		To:    &syntheticNft,
		Value: big.NewInt(0),
		Data:  callData,
	}
}

// riverSharedOpKind keeps the compiler honest, like riverJobKind: the queue
// and the status reader must move together if Kind ever changes.
var _ river.JobArgs = SharedOpArgs{}
