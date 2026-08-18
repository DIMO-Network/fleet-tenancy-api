package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// maxShareableOwners bounds one request. A customer tenant's fleet normally
// sits on a single kernel account, so this is far above any real list — it is
// here to stop a caller turning one HTTP request into an unbounded fan-out of
// accounts-api calls, not to constrain legitimate use.
const maxShareableOwners = 200

// SharingController serves the vehicle-sharing surface.
//
// This half is read-only: it answers which owners the tenant may sign for,
// which is what fleet-lite needs to decide whether to render a share button.
// The share itself lands with the worker.
type SharingController struct {
	logger  *zerolog.Logger
	signer  *service.SharedSignerService
	tenants *service.TenantService
	caller  CallerResolver
}

func NewSharingController(logger *zerolog.Logger, signer *service.SharedSignerService,
	tenants *service.TenantService, caller CallerResolver) *SharingController {
	return &SharingController{logger: logger, signer: signer, tenants: tenants, caller: caller}
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

	owners, err := c.signer.FilterSignable(ctx.Context(), tenantID, body.Owners)
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
	return ctx.JSON(models.ShareableOwnersResult{Owners: owners})
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
