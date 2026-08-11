// Package controllers holds the HTTP handlers.
package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// CallerResolver hands the handler the caller the middleware authenticated.
// Injected rather than imported so controllers do not depend on package app,
// which already depends on controllers.
type CallerResolver func(c *fiber.Ctx) *models.CallerTenant

type AuthzController struct {
	logger  *zerolog.Logger
	authz   *service.AuthzService
	tenants *service.TenantService
	caller  CallerResolver
}

func NewAuthzController(logger *zerolog.Logger, authz *service.AuthzService,
	tenants *service.TenantService, caller CallerResolver) *AuthzController {
	return &AuthzController{logger: logger, authz: authz, tenants: tenants, caller: caller}
}

// GetAuthz — GET /v1/authz?wallet=&tenant_id=
//
// The hot path: both fleet-lite-app and kaufmann-oracle call this on every
// request, replacing their own edge checks (fleet-lite's NewTenantMiddleware and
// kaufmann's NewAccessMiddleware).
//
// A wallet with no access is a 200 with via="none", not a 403 — the caller
// decides the status code for its own surface, and a 403 here would be
// indistinguishable from this service rejecting the *caller's* credentials.
func (c *AuthzController) GetAuthz(ctx *fiber.Ctx) error {
	wallet := ctx.Query("wallet")
	tenantID := ctx.Query("tenant_id")
	if wallet == "" || tenantID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "wallet and tenant_id are required")
	}

	// Scope: a caller may only ask about tenants its own credential reaches.
	// Without this, any registered developer license could read any tenant's
	// authorization data — and four of the eight that authenticate today belong
	// to customer tenants owned by outside companies.
	caller := c.caller(ctx)
	ok, err := c.tenants.CallerMayAccess(ctx.Context(), caller, tenantID)
	if err != nil {
		if errors.Is(err, service.ErrInvalidTenantID) {
			return fiber.NewError(fiber.StatusBadRequest, "tenant_id must be a uuid")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("caller scope check")
		return fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	if !ok {
		// 403 rather than 404: the caller is authenticated, it simply has no
		// business with this tenant. A 404 would also let it probe which tenant
		// ids exist.
		c.logger.Warn().Str("caller_tenant", callerID(caller)).Str("subject_tenant", tenantID).
			Msg("/v1/authz: caller asked about a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not query this tenant")
	}

	res, err := c.authz.Authorize(ctx.Context(), tenantID, wallet)
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("authorize")
		return fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	return ctx.JSON(res)
}

// callerID is a logging helper that tolerates an unresolved caller.
func callerID(c *models.CallerTenant) string {
	if c == nil {
		return "<none>"
	}
	return c.TenantID
}
