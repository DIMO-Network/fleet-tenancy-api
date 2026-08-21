package controllers

import (
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/sharing"
	"github.com/ethereum/go-ethereum/common"
	"github.com/gofiber/fiber/v2"
)

// SharedOperation — POST /v1/tenants/:tenantId/vehicles/:tokenId/shared-ops
//
// The typed shared-operations endpoint (docs/plans/06-signer-key-consolidation.md,
// step 3): one of four named operations, signed with the tenant's signer on
// the vehicle owner's kernel account. The body carries an operation ENUM and
// never calldata — a general "sign this" endpoint would be a signing oracle
// over every kernel account any operator's signer can act for, and the narrow
// interface cannot be widened later without that cost, so it starts narrow.
//
// Returns 202 and a job id, like a share and for the same reason: every op
// waits on a bundler for longer than an HTTP request should.
//
// Unlike ShareVehicle there is no per-member capability check here, and the
// difference is deliberate rather than an omission. The expected caller is
// kaufmann's shared-account workers, whose HTTP boundary already gated the
// human (its access middleware), and which call this from a background job
// that carries no session — the same BFF split as invitations, where the
// calling app owns the human check and this service checks the caller tenant's
// scope. Re-checking a member's capability at this remove would also be an
// execution-time re-read of a request-time property, which ShareArgs's
// ActorWallet comment records as the wrong side of that line. What this
// endpoint enforces itself is everything that must be true NOW: caller scope,
// entitlement, live ownership and live signer authority.
func (c *SharingController) SharedOperation(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	tokenID, err := strconv.ParseInt(ctx.Params("tokenId"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "tokenId must be a number")
	}

	var body models.SharedOpInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "run shared operation"); err != nil {
		return err
	}

	args := sharing.SharedOpArgs{
		TenantID:         tenantID,
		TokenID:          tokenID,
		Op:               sharing.SharedOp(body.Op),
		TargetWallet:     body.TargetWallet,
		SyntheticTokenID: body.SyntheticTokenID,
		ActorWallet:      body.ActorWallet,
	}
	// Shape first, before any upstream call — the op/field matrix needs
	// nothing to check. The same validation runs again in the worker, which is
	// the last point before calldata is built.
	if err := args.Validate(); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	// The full authorization chain runs here so the caller gets a synchronous
	// answer, and again in the worker before the irreversible call. Both are
	// necessary: this one is for the caller, that one is for correctness.
	owner, _, err := c.shares.AuthorizeShare(ctx.Context(), tenantID, tokenID)
	if err != nil {
		return c.shareAuthError(ctx, tenantID, tokenID, err)
	}

	switch args.Op {
	case sharing.OpTransferVehicle:
		// Only applicable once the owner is known, which is why it is not in
		// Validate: transferring a vehicle to its current owner is a no-op
		// that would read as success.
		if common.HexToAddress(args.TargetWallet) == owner {
			return fiber.NewError(fiber.StatusBadRequest, "the vehicle already belongs to targetWallet")
		}
	case sharing.OpGrantSacd:
		// The grant's target is the tenant's own client id; a tenant without
		// one has nothing to grant to, and finding that out synchronously
		// beats a job that can only fail. The transfer op deliberately skips
		// this check — its chained re-share is best-effort, and a tenant
		// without a client id can still transfer.
		if _, err := c.shares.GranteeClientID(ctx.Context(), tenantID); err != nil {
			return c.granteeClientIDError(ctx, tenantID, err)
		}
	}

	jobID, err := c.queue.EnqueueSharedOp(ctx.Context(), args)
	if err != nil {
		if errors.Is(err, sharing.ErrQueueUnavailable) {
			return fiber.NewError(fiber.StatusServiceUnavailable,
				"shared operations are not available in this environment")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Int64("token_id", tokenID).
			Str("op", body.Op).Msg("enqueue shared operation")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to queue the operation")
	}

	return ctx.Status(fiber.StatusAccepted).JSON(models.SharedOpResult{JobID: jobID})
}

// SharedOperationStatus — GET /v1/tenants/:tenantId/vehicles/:tokenId/shared-ops/status?jobId=
//
// Mirrors ShareStatus exactly: same response shape, same tenant scoping, same
// not-found answer for another tenant's job — plus the same answer for a job
// of the other kind, so the two polling surfaces stay distinct despite
// sharing one id sequence.
func (c *SharingController) SharedOperationStatus(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	jobID, err := strconv.ParseInt(ctx.Query("jobId"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "jobId is required and must be a number")
	}
	if err := c.assertScope(ctx, tenantID, "read shared operation status"); err != nil {
		return err
	}

	status, err := c.queue.SharedOpStatus(ctx.Context(), tenantID, jobID)
	if err != nil {
		switch {
		case errors.Is(err, sharing.ErrJobNotFound):
			return fiber.NewError(fiber.StatusNotFound, "no such shared-operation job")
		case errors.Is(err, sharing.ErrQueueUnavailable):
			return fiber.NewError(fiber.StatusServiceUnavailable,
				"shared operations are not available in this environment")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Int64("job_id", jobID).
			Msg("read shared operation status")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read the operation status")
	}
	return ctx.JSON(status)
}

// granteeClientIDError maps a failed grantee resolution: a missing credential
// or client id is the tenant's configuration state (409, an operator can fix
// it), everything else is upstream (500 — this resolution is a local read).
func (c *SharingController) granteeClientIDError(ctx *fiber.Ctx, tenantID string, err error) error {
	switch {
	case errors.Is(err, service.ErrNoClientID), errors.Is(err, service.ErrNoCredential):
		return fiber.NewError(fiber.StatusConflict, "this tenant has no DIMO client id to grant to")
	case errors.Is(err, service.ErrTenantNotFound):
		return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
	}
	c.logger.Err(err).Str("tenant_id", tenantID).Msg("resolve grantee client id")
	return fiber.NewError(fiber.StatusInternalServerError, "failed to resolve the tenant's client id")
}
