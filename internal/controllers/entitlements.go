package controllers

import (
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// EntitlementsController serves which vehicles a customer may see.
//
// Every write here moves the isolation boundary, so the scope check is not a
// formality: a caller that could assign vehicles into a tenant outside its own
// would be handing another operator's fleet to its own customer.
type EntitlementsController struct {
	logger       *zerolog.Logger
	entitlements *service.EntitlementService
	tenants      *service.TenantService
	caller       CallerResolver
}

func NewEntitlementsController(logger *zerolog.Logger, entitlements *service.EntitlementService,
	tenants *service.TenantService, caller CallerResolver) *EntitlementsController {
	return &EntitlementsController{
		logger: logger, entitlements: entitlements, tenants: tenants, caller: caller,
	}
}

// ListVehicles — GET /v1/tenants/:tenantId/vehicles
func (c *EntitlementsController) ListVehicles(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "list vehicles"); err != nil {
		return err
	}
	out, err := c.entitlements.List(ctx.Context(), tenantID)
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("list entitlements")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list vehicles")
	}
	return ctx.JSON(out)
}

// AssignVehicles — POST /v1/tenants/:tenantId/vehicles
//
// Returns 200 with a per-vehicle account rather than 4xx when some vehicles
// could not be taken. A partial assignment is a real outcome the operator needs
// the detail of, and an error status would throw that detail away.
func (c *EntitlementsController) AssignVehicles(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	var body models.AssignVehiclesInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "assign vehicles"); err != nil {
		return err
	}

	res, err := c.entitlements.Assign(ctx.Context(), tenantID, &body, actorWallet(ctx))
	if err != nil {
		switch {
		case errors.Is(err, service.ErrTenantNotFound):
			return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
		case errors.Is(err, service.ErrNotExplicitMode):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("assign vehicles")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to assign vehicles")
	}
	return ctx.JSON(res)
}

// RevokeVehicle — DELETE /v1/tenants/:tenantId/vehicles/:tokenId
func (c *EntitlementsController) RevokeVehicle(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	tokenID, err := strconv.ParseInt(ctx.Params("tokenId"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "tokenId must be a number")
	}
	if err := c.assertScope(ctx, tenantID, "revoke vehicle"); err != nil {
		return err
	}

	if err := c.entitlements.Revoke(ctx.Context(), tenantID, tokenID); err != nil {
		// Revoking something already revoked is the state the caller asked for,
		// so a retry after a timeout is not a failure.
		if errors.Is(err, service.ErrEntitlementNotFound) {
			return ctx.SendStatus(fiber.StatusNoContent)
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Int64("token_id", tokenID).
			Msg("revoke entitlement")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to revoke vehicle")
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (c *EntitlementsController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
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
			Msg("/v1: caller acted on vehicles of a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not act on this tenant")
	}
	return nil
}
