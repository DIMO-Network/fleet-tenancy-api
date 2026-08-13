package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// ProvisionController serves the two endpoints built on the tenant's effective
// credential: on-behalf member provisioning and the developer-token minter.
// They share a controller because they share the sensitive part — both cause a
// decrypted tenant credential to be used inside the service — and the same
// three gates as the rest of /v1, with scope mattering most: a caller that
// could mint another tenant's token would hold its license in all but name.
type ProvisionController struct {
	logger    *zerolog.Logger
	provision *service.ProvisionService
	creds     *service.CredentialService
	tenants   *service.TenantService
	caller    CallerResolver
}

func NewProvisionController(logger *zerolog.Logger, provision *service.ProvisionService,
	creds *service.CredentialService, tenants *service.TenantService, caller CallerResolver) *ProvisionController {
	return &ProvisionController{logger: logger, provision: provision, creds: creds, tenants: tenants, caller: caller}
}

// Provision — POST /v1/tenants/:tenantId/members/provision
//
// {email, role, permissions, scopeGroupIds} → accounts-api lookup-or-create →
// user + membership. Not idempotent in the HTTP sense a PUT is, but a retry
// converges: the lookup finds what a half-failed attempt created, and the
// membership write replaces rather than accumulates.
func (c *ProvisionController) Provision(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if tenantID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenantId is required")
	}

	var body models.ProvisionRequest
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	caller, err := c.assertScope(ctx, tenantID, "provision member")
	if err != nil {
		return err
	}
	// The audit trail records the authenticated caller, never the body.
	body.GrantedByTenantID = caller.TenantID

	res, err := c.provision.Provision(ctx.Context(), tenantID, &body)
	if err != nil {
		return c.mapError(ctx, tenantID, "provision", err)
	}
	return ctx.JSON(res)
}

// GetDimoToken — GET /v1/tenants/:tenantId/dimo-token
//
// A short-lived developer JWT for the tenant's effective credential. This is
// the alternative to credentials ever leaving the service: b2b (or any future
// caller) asks for a token to act as the tenant, rather than for the key.
func (c *ProvisionController) GetDimoToken(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if tenantID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenantId is required")
	}

	if _, err := c.assertScope(ctx, tenantID, "mint dimo token"); err != nil {
		return err
	}

	minted, err := c.creds.DeveloperJWT(ctx.Context(), tenantID)
	if err != nil {
		return c.mapError(ctx, tenantID, "mint", err)
	}
	return ctx.JSON(minted)
}

// assertScope is the same gate the other write controllers apply, returning
// the caller for the audit trail.
func (c *ProvisionController) assertScope(ctx *fiber.Ctx, tenantID, op string) (*models.CallerTenant, error) {
	caller := c.caller(ctx)
	ok, err := c.tenants.CallerMayAccess(ctx.Context(), caller, tenantID)
	if err != nil {
		if errors.Is(err, service.ErrInvalidTenantID) {
			return nil, fiber.NewError(fiber.StatusBadRequest, "tenantId must be a uuid")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("caller scope check")
		return nil, fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	if !ok {
		c.logger.Warn().
			Str("caller_tenant", callerID(caller)).
			Str("subject_tenant", tenantID).
			Str("op", op).
			Msg("/v1: caller tried to act on a tenant outside its scope")
		return nil, fiber.NewError(fiber.StatusForbidden, "caller may not act on this tenant")
	}
	return caller, nil
}

func (c *ProvisionController) mapError(ctx *fiber.Ctx, tenantID, op string, err error) error {
	switch {
	case errors.Is(err, service.ErrTenantNotFound):
		// 403 rather than 404, matching every other route: a 404 would confirm
		// which uuids are real.
		return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
	case errors.Is(err, service.ErrNoCredential):
		// A configuration state, not a caller mistake: the tenant (or its
		// operator) has no usable license yet.
		return fiber.NewError(fiber.StatusConflict, service.ErrNoCredential.Error())
	case errors.Is(err, service.ErrNoSignerAddress):
		return fiber.NewError(fiber.StatusConflict, service.ErrNoSignerAddress.Error())
	case errors.Is(err, service.ErrUpstream):
		// The dependency failed, not this service and not the caller. 502 so a
		// retry looks as reasonable as it is.
		c.logger.Warn().Err(err).Str("tenant_id", tenantID).Str("op", op).Msg("upstream failure")
		return fiber.NewError(fiber.StatusBadGateway, err.Error())
	case isProvisionBadRequest(err):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	c.logger.Err(err).Str("tenant_id", tenantID).Str("op", op).Msg("provision surface")
	return fiber.NewError(fiber.StatusInternalServerError, "request failed")
}

// isProvisionBadRequest mirrors members.go's isBadRequest: exact strings only,
// so a database failure can never be mistaken for a caller mistake.
func isProvisionBadRequest(err error) bool {
	msg := err.Error()
	return msg == "email is required" ||
		msg == "scopeGroupIds is required (null for unrestricted, [] for no groups)"
}
