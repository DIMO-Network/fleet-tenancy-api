package controllers

import (
	"context"
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/sharing"
	"github.com/ethereum/go-ethereum/common"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// maxShareableOwners bounds one request. A customer tenant's fleet normally
// sits on a single kernel account, so this is far above any real list — it is
// here to stop a caller turning one HTTP request into an unbounded fan-out of
// accounts-api calls, not to constrain legitimate use.
const maxShareableOwners = 200

// shareQueue is the queue surface the controller needs, narrowed so the
// handlers can be tested without River or a database.
type shareQueue interface {
	Enqueue(ctx context.Context, args sharing.ShareArgs) (int64, error)
	Status(ctx context.Context, tenantID string, jobID int64) (*models.ShareStatus, error)
	EnqueueSharedOp(ctx context.Context, args sharing.SharedOpArgs) (int64, error)
	SharedOpStatus(ctx context.Context, tenantID string, jobID int64) (*models.ShareStatus, error)
}

// SharingController serves the vehicle-sharing surface: the display gate, the
// share itself, and the status of a queued share.
type SharingController struct {
	logger  *zerolog.Logger
	signer  *service.SharedSignerService
	authz   *service.AuthzService
	shares  *service.ShareAuthorizer
	queue   shareQueue
	tenants *service.TenantService
	caller  CallerResolver
}

func NewSharingController(logger *zerolog.Logger, signer *service.SharedSignerService,
	authz *service.AuthzService, shares *service.ShareAuthorizer, queue shareQueue,
	tenants *service.TenantService, caller CallerResolver) *SharingController {
	return &SharingController{
		logger: logger, signer: signer, authz: authz, shares: shares,
		queue: queue, tenants: tenants, caller: caller,
	}
}

// ShareVehicle — POST /v1/tenants/:tenantId/vehicles/:tokenId/share
//
// Returns 202 and a job id. The share waits on a bundler, which regularly takes
// longer than a sane HTTP timeout, and a client-side timeout after the UserOp
// was sent would leave the caller unable to tell whether the grant landed.
//
// The full authorization chain runs here so the customer gets a synchronous
// answer, and runs again in the worker before the irreversible call. Both are
// necessary: this one is for the human, that one is for correctness.
func (c *SharingController) ShareVehicle(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	tokenID, err := strconv.ParseInt(ctx.Params("tokenId"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "tokenId must be a number")
	}

	var body models.ShareVehicleInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "share vehicle"); err != nil {
		return err
	}
	if body.DurationDays < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "durationDays must not be negative")
	}
	// Grantee shape first, before any upstream call: it needs nothing to check
	// and it is the field a caller is most likely to get wrong. The
	// owner-equality half of the rule cannot be applied yet — the owner is not
	// known until the chain runs — so it is re-checked below.
	if err := service.ValidateGrantee(body.Grantee, common.Address{}); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	// The member's capability. This is the gate kaufmann's shared-account
	// routes were missing: the owner's grant of our signer says the TENANT may
	// act, never which of its members may. Checked against this service's own
	// authz rather than trusted from the caller — the calling app checks it too,
	// but a bug there should not become an on-chain grant here.
	if err := c.assertCapability(ctx, tenantID, body.Wallet); err != nil {
		return err
	}

	owner, _, err := c.shares.AuthorizeShare(ctx.Context(), tenantID, tokenID)
	if err != nil {
		return c.shareAuthError(ctx, tenantID, tokenID, err)
	}
	// Now that the owner is known, the rest of the grantee rule applies.
	if err := service.ValidateGrantee(body.Grantee, owner); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	jobID, err := c.queue.Enqueue(ctx.Context(), sharing.ShareArgs{
		TenantID:     tenantID,
		TokenID:      tokenID,
		Grantee:      common.HexToAddress(body.Grantee).Hex(),
		DurationDays: body.DurationDays,
		ActorWallet:  body.Wallet,
	})
	if err != nil {
		if errors.Is(err, sharing.ErrQueueUnavailable) {
			return fiber.NewError(fiber.StatusServiceUnavailable,
				"vehicle sharing is not available in this environment")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Int64("token_id", tokenID).
			Msg("enqueue share")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to queue the share")
	}

	return ctx.Status(fiber.StatusAccepted).JSON(models.ShareVehicleResult{JobID: jobID})
}

// ShareStatus — GET /v1/tenants/:tenantId/vehicles/:tokenId/share/status?jobId=
func (c *SharingController) ShareStatus(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	jobID, err := strconv.ParseInt(ctx.Query("jobId"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "jobId is required and must be a number")
	}
	if err := c.assertScope(ctx, tenantID, "read share status"); err != nil {
		return err
	}

	status, err := c.queue.Status(ctx.Context(), tenantID, jobID)
	if err != nil {
		switch {
		case errors.Is(err, sharing.ErrJobNotFound):
			return fiber.NewError(fiber.StatusNotFound, "no such share job")
		case errors.Is(err, sharing.ErrQueueUnavailable):
			return fiber.NewError(fiber.StatusServiceUnavailable,
				"vehicle sharing is not available in this environment")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Int64("job_id", jobID).
			Msg("read share status")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read share status")
	}
	return ctx.JSON(status)
}

// assertCapability requires manage_vehicles for the acting member.
func (c *SharingController) assertCapability(ctx *fiber.Ctx, tenantID, wallet string) error {
	if wallet == "" {
		return fiber.NewError(fiber.StatusBadRequest, "wallet is required")
	}
	res, err := c.authz.Authorize(ctx.Context(), tenantID, wallet)
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("share capability lookup")
		return fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	for _, p := range res.Permissions {
		if p == models.CapManageVehicles {
			return nil
		}
	}
	c.logger.Warn().
		Str("tenant_id", tenantID).
		Str("wallet", wallet).
		Msg("share refused: member lacks manage_vehicles")
	return fiber.NewError(fiber.StatusForbidden, "manage_vehicles is required to share a vehicle")
}

// shareAuthError maps the authorization chain's failures onto status codes.
//
// The distinction that matters most: a policy denial is a 403 and an upstream
// failure is a 502. Collapsed, an accounts-api outage would tell every customer
// their owner had revoked a signer that was never revoked.
func (c *SharingController) shareAuthError(ctx *fiber.Ctx, tenantID string, tokenID int64, err error) error {
	switch {
	case errors.Is(err, service.ErrNotEntitled):
		// Not 404: the caller knows the vehicle exists, it is simply not
		// theirs. Saying so is not a leak — they supplied the id.
		return fiber.NewError(fiber.StatusForbidden, "vehicle is not in this tenant's fleet")
	case errors.Is(err, service.ErrVehicleUnknown):
		return fiber.NewError(fiber.StatusNotFound, "no such vehicle")
	case errors.Is(err, service.ErrSignerNotAuthorized):
		return fiber.NewError(fiber.StatusForbidden,
			"the vehicle's owner has not authorized this tenant to sign on its behalf")
	case errors.Is(err, service.ErrNoSignerKey), errors.Is(err, service.ErrNoCredential):
		return fiber.NewError(fiber.StatusConflict, "this tenant has no signer configured")
	case errors.Is(err, service.ErrTenantNotFound):
		return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
	}
	c.logger.Err(err).Str("tenant_id", tenantID).Int64("token_id", tokenID).
		Msg("share authorization failed upstream")
	return fiber.NewError(fiber.StatusBadGateway, "could not verify the share is permitted")
}

// ShareableOwners — POST /v1/tenants/:tenantId/shareable-owners
//
// A POST for what is conceptually a read. The input is a list whose length is
// set by the caller's fleet, and a query string is the wrong place for that —
// it would work until a tenant with enough distinct owners silently exceeded a
// URL length limit. Nothing is written.
func (c *SharingController) ShareableOwners(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	var body models.ShareableOwnersInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "resolve shareable owners"); err != nil {
		return err
	}
	if len(body.Owners) > maxShareableOwners {
		return fiber.NewError(fiber.StatusBadRequest, "too many owners in one request")
	}
	if len(body.Owners) == 0 {
		return ctx.JSON(models.ShareableOwnersResult{Owners: []string{}})
	}

	owners, unresolved, err := c.signer.FilterSignable(ctx.Context(), tenantID, body.Owners)
	if err != nil {
		// Deliberately not a 200 with a shorter list. An upstream failure that
		// read as "none of these are shareable" would hide every share button
		// during an accounts-api blip and be indistinguishable from the feature
		// being switched off — the caller must be able to tell the difference.
		if errors.Is(err, service.ErrNoCredential) {
			return fiber.NewError(fiber.StatusConflict, "tenant has no effective credential")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("resolve shareable owners")
		return fiber.NewError(fiber.StatusBadGateway, "could not resolve shareable owners")
	}
	return ctx.JSON(models.ShareableOwnersResult{Owners: owners, Unresolved: unresolved})
}

func (c *SharingController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
	if tenantID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenant id is required")
	}
	caller := c.caller(ctx)
	ok, err := c.tenants.CallerMayAccess(ctx.Context(), caller, tenantID)
	if err != nil {
		if errors.Is(err, service.ErrInvalidTenantID) {
			return fiber.NewError(fiber.StatusBadRequest, "tenant id must be a uuid")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("caller scope check")
		return fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	if !ok {
		c.logger.Warn().
			Str("caller_tenant", callerID(caller)).
			Str("subject_tenant", tenantID).
			Str("op", op).
			Msg("/v1: caller acted on sharing of a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not act on this tenant")
	}
	return nil
}
