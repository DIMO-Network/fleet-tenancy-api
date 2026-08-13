package controllers

import (
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// GroupsController serves fleet groups: the organising structure the two apps
// previously each owned a copy of, now owned here. Same three gates as the
// rest of /v1, and the scope check matters on reads as much as writes — a
// group's member list names which vehicles a tenant organises, which is
// exactly the kind of cross-tenant fact this service exists to contain.
type GroupsController struct {
	logger  *zerolog.Logger
	groups  *service.GroupService
	tenants *service.TenantService
	caller  CallerResolver
}

func NewGroupsController(logger *zerolog.Logger, groups *service.GroupService,
	tenants *service.TenantService, caller CallerResolver) *GroupsController {
	return &GroupsController{logger: logger, groups: groups, tenants: tenants, caller: caller}
}

// ListGroups — GET /v1/tenants/:tenantId/groups
func (c *GroupsController) ListGroups(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "list groups"); err != nil {
		return err
	}
	out, err := c.groups.List(ctx.Context(), tenantID)
	if err != nil {
		return c.mapError(ctx, tenantID, "list groups", err)
	}
	return ctx.JSON(out)
}

// CreateGroup — POST /v1/tenants/:tenantId/groups
func (c *GroupsController) CreateGroup(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "create group"); err != nil {
		return err
	}
	var body models.CreateGroupInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	out, err := c.groups.Create(ctx.Context(), tenantID, &body)
	if err != nil {
		return c.mapError(ctx, tenantID, "create group", err)
	}
	return ctx.Status(fiber.StatusCreated).JSON(out)
}

// UpdateGroup — PATCH /v1/tenants/:tenantId/groups/:groupId
func (c *GroupsController) UpdateGroup(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "update group"); err != nil {
		return err
	}
	var body models.UpdateGroupInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	out, err := c.groups.Update(ctx.Context(), tenantID, groupIDParam(ctx), &body)
	if err != nil {
		return c.mapError(ctx, tenantID, "update group", err)
	}
	return ctx.JSON(out)
}

// DeleteGroup — DELETE /v1/tenants/:tenantId/groups/:groupId
func (c *GroupsController) DeleteGroup(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "delete group"); err != nil {
		return err
	}
	if err := c.groups.Delete(ctx.Context(), tenantID, groupIDParam(ctx)); err != nil {
		return c.mapError(ctx, tenantID, "delete group", err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// ListGroupVehicles — GET /v1/tenants/:tenantId/groups/:groupId/vehicles
func (c *GroupsController) ListGroupVehicles(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "list group vehicles"); err != nil {
		return err
	}
	out, err := c.groups.ListVehicles(ctx.Context(), tenantID, groupIDParam(ctx))
	if err != nil {
		return c.mapError(ctx, tenantID, "list group vehicles", err)
	}
	return ctx.JSON(fiber.Map{"tokenIds": out})
}

// AddGroupVehicles — POST /v1/tenants/:tenantId/groups/:groupId/vehicles
func (c *GroupsController) AddGroupVehicles(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "add group vehicles"); err != nil {
		return err
	}
	var body models.GroupVehiclesInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.groups.AddVehicles(ctx.Context(), tenantID, groupIDParam(ctx), body.TokenIDs); err != nil {
		return c.mapError(ctx, tenantID, "add group vehicles", err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// RemoveGroupVehicle — DELETE /v1/tenants/:tenantId/groups/:groupId/vehicles/:tokenId
func (c *GroupsController) RemoveGroupVehicle(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "remove group vehicle"); err != nil {
		return err
	}
	tokenID, err := strconv.ParseInt(ctx.Params("tokenId"), 10, 64)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "tokenId must be an integer")
	}
	if err := c.groups.RemoveVehicle(ctx.Context(), tenantID, groupIDParam(ctx), tokenID); err != nil {
		return c.mapError(ctx, tenantID, "remove group vehicle", err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// groupIDParam decodes the group id from the path. Ids contain the tenant uuid
// and a slug, so the raw param is already url-safe; Params applies fiber's
// decoding, which is all that is needed.
func groupIDParam(ctx *fiber.Ctx) string { return ctx.Params("groupId") }

func (c *GroupsController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
	if tenantID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenantId is required")
	}
	caller := c.caller(ctx)
	ok, err := c.tenants.CallerMayAccess(ctx.Context(), caller, tenantID)
	if err != nil {
		if errors.Is(err, service.ErrInvalidTenantID) {
			return fiber.NewError(fiber.StatusBadRequest, "tenantId must be a uuid")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("caller scope check")
		return fiber.NewError(fiber.StatusInternalServerError, "authorization lookup failed")
	}
	if !ok {
		c.logger.Warn().
			Str("caller_tenant", callerID(caller)).
			Str("subject_tenant", tenantID).
			Str("op", op).
			Msg("/v1: caller tried to act on a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not act on this tenant")
	}
	return nil
}

func (c *GroupsController) mapError(ctx *fiber.Ctx, tenantID, op string, err error) error {
	switch {
	case errors.Is(err, service.ErrTenantNotFound):
		// 403 rather than 404, matching every other route: a 404 would confirm
		// which uuids are real.
		return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
	case errors.Is(err, service.ErrGroupNotFound):
		// A group id is not guessable capital — it names a group inside a
		// tenant the caller already reached — so a plain 404 is fine here.
		return fiber.NewError(fiber.StatusNotFound, service.ErrGroupNotFound.Error())
	case errors.Is(err, service.ErrGroupNameTaken):
		return fiber.NewError(fiber.StatusConflict, service.ErrGroupNameTaken.Error())
	case errors.Is(err, service.ErrInvalidGroupInput):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	c.logger.Err(err).Str("tenant_id", tenantID).Str("op", op).Msg("groups surface")
	return fiber.NewError(fiber.StatusInternalServerError, "request failed")
}
