// Package controllers holds the HTTP handlers.
package controllers

import (
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

type AuthzController struct {
	logger *zerolog.Logger
	authz  *service.AuthzService
}

func NewAuthzController(logger *zerolog.Logger, authz *service.AuthzService) *AuthzController {
	return &AuthzController{logger: logger, authz: authz}
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

	res, err := c.authz.Authorize(ctx.Context(), tenantID, wallet)
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("authorize")
		return fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	return ctx.JSON(res)
}
