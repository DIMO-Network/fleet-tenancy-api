package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

type ResolveController struct {
	logger  *zerolog.Logger
	tenants *service.TenantService
}

func NewResolveController(logger *zerolog.Logger, tenants *service.TenantService) *ResolveController {
	return &ResolveController{logger: logger, tenants: tenants}
}

// ResolveClientID — GET /v1/resolve/client-id/:clientId
//
// Developer license → tenant. Replaces kaufmann-oracle's in-app resolver, which
// stops being able to answer this once an operator's license is shared with its
// customers.
//
// A client id with no tenant is a 404, unlike /v1/authz where "no access" is a
// 200. The difference is deliberate: authz asks a question that has a legitimate
// negative answer, while this asks to dereference an identifier that either
// exists or does not.
func (c *ResolveController) ResolveClientID(ctx *fiber.Ctx) error {
	clientID := ctx.Params("clientId")
	if clientID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "clientId is required")
	}

	ref, err := c.tenants.ResolveByClientID(ctx.Context(), clientID)
	if err != nil {
		if errors.Is(err, service.ErrTenantNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "no tenant is registered for this client id")
		}
		c.logger.Err(err).Msg("resolve client id")
		return fiber.NewError(fiber.StatusInternalServerError, "tenant lookup failed")
	}
	return ctx.JSON(ref)
}
