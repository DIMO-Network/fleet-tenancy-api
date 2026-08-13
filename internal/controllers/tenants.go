package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// TenantsController serves the operator console's tenant surface.
//
// These are service-to-service routes on /v1, reached by b2b-fleet-mgr-app
// through kaufmann-oracle: b2b holds no DIMO developer licence and cannot
// authenticate here directly, while kaufmann can and already does for
// /v1/authz. kaufmann is responsible for checking that the *user* driving the
// request may manage members before it proxies; this service checks that the
// *caller tenant* may reach the tenant being acted on.
//
// The spec's longer-term home for these is /user/v1, authenticated with the end
// user's own DIMO JWT. That surface stays cheap to add because the work is in
// the service layer — only the wrapper that establishes who is asking differs.
type TenantsController struct {
	logger  *zerolog.Logger
	tenants *service.TenantService
	caller  CallerResolver
}

func NewTenantsController(logger *zerolog.Logger, tenants *service.TenantService,
	caller CallerResolver) *TenantsController {
	return &TenantsController{logger: logger, tenants: tenants, caller: caller}
}

// ListChildren — GET /v1/operators/:operatorId/children
func (c *TenantsController) ListChildren(ctx *fiber.Ctx) error {
	operatorID := ctx.Params("operatorId")
	if err := c.assertScope(ctx, operatorID, "list children"); err != nil {
		return err
	}
	children, err := c.tenants.ListChildren(ctx.Context(), operatorID)
	if err != nil {
		c.logger.Err(err).Str("operator_tenant", operatorID).Msg("list children")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list customers")
	}
	return ctx.JSON(children)
}

// GetTenant — GET /v1/tenants/:tenantId
func (c *TenantsController) GetTenant(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "get tenant"); err != nil {
		return err
	}
	t, err := c.tenants.Get(ctx.Context(), tenantID)
	if err != nil {
		return c.mapError(err, tenantID, "get tenant")
	}
	return ctx.JSON(t)
}

// CreateCustomer — POST /v1/operators/:operatorId/customers
//
// Deliberately nested under the operator rather than a bare POST /v1/tenants.
// The parent is the thing being added to, so it belongs in the path where the
// scope check can see it — and it removes any question of a caller creating a
// tenant somewhere it has no business.
func (c *TenantsController) CreateCustomer(ctx *fiber.Ctx) error {
	operatorID := ctx.Params("operatorId")

	var body models.CreateTenantInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, operatorID, "create customer"); err != nil {
		return err
	}

	t, err := c.tenants.CreateCustomer(ctx.Context(), operatorID, &body, actorWallet(ctx))
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNameTaken):
			return fiber.NewError(fiber.StatusConflict, err.Error())
		case errors.Is(err, service.ErrNotAnOperator):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return c.mapError(err, operatorID, "create customer")
	}
	return ctx.Status(fiber.StatusCreated).JSON(t)
}

// UpdateTenant — PATCH /v1/tenants/:tenantId
func (c *TenantsController) UpdateTenant(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	var body models.UpdateTenantInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "update tenant"); err != nil {
		return err
	}

	t, err := c.tenants.Update(ctx.Context(), tenantID, &body)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNameTaken):
			return fiber.NewError(fiber.StatusConflict, err.Error())
		case errors.Is(err, service.ErrInvalidStatus):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return c.mapError(err, tenantID, "update tenant")
	}
	return ctx.JSON(t)
}

// ListMembers — GET /v1/tenants/:tenantId/members
func (c *TenantsController) ListMembers(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "list members"); err != nil {
		return err
	}
	members, err := c.tenants.ListMembers(ctx.Context(), tenantID)
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("list members")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list members")
	}
	return ctx.JSON(members)
}

func (c *TenantsController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
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
			Msg("/v1: caller acted on a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not act on this tenant")
	}
	return nil
}

// mapError turns a service error into a status. An unknown tenant is 403 rather
// than 404 for the same reason as the read path: a 404 would let a caller probe
// which tenant ids are real.
func (c *TenantsController) mapError(err error, tenantID, op string) error {
	if errors.Is(err, service.ErrTenantNotFound) {
		return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
	}
	c.logger.Err(err).Str("tenant_id", tenantID).Msg(op)
	return fiber.NewError(fiber.StatusInternalServerError, "request failed")
}

// actorWallet is the person the calling app says is driving the request, for
// the audit trail. It is not an authorization input: the caller has already
// been authenticated as a tenant, and kaufmann checks the user's capability
// before proxying. Recorded because operator actions on a customer are the ones
// that get questioned later.
func actorWallet(ctx *fiber.Ctx) string {
	return ctx.Get("X-Actor-Wallet")
}
