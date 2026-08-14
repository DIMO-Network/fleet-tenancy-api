package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// MembershipsController serves the commercial record: what a customer has paid
// for, per vehicle, and until when.
//
// Scope is checked on every route for the same reason it is on entitlements: a
// caller able to write memberships into a tenant outside its own could turn
// another operator's customer's fleet on or off.
type MembershipsController struct {
	logger      *zerolog.Logger
	memberships *service.MembershipService
	tenants     *service.TenantService
	caller      CallerResolver
}

func NewMembershipsController(logger *zerolog.Logger, memberships *service.MembershipService,
	tenants *service.TenantService, caller CallerResolver) *MembershipsController {
	return &MembershipsController{
		logger: logger, memberships: memberships, tenants: tenants, caller: caller,
	}
}

// ListMemberships — GET /v1/tenants/:tenantId/vehicle-memberships
func (c *MembershipsController) ListMemberships(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "list memberships"); err != nil {
		return err
	}
	out, err := c.memberships.List(ctx.Context(), tenantID)
	if err != nil {
		if errors.Is(err, service.ErrTenantNotFound) {
			return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("list memberships")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list memberships")
	}
	return ctx.JSON(out)
}

// ActiveMemberships — GET /v1/tenants/:tenantId/active-vehicle-memberships
//
// The read fleet-lite gates its vehicle list on: the enforcement flag and the
// active token ids, in one response. Deliberately not ListMemberships — that
// ships every row (terms, dates, status) for the console to render, and this
// sits on a per-request hot path that needs only a set of ints to intersect.
//
// `enforced` and `tokenIds` travel together because two calls can straddle a
// toggle, and the failure mode of that is a fleet that briefly renders empty.
func (c *MembershipsController) ActiveMemberships(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "read active memberships"); err != nil {
		return err
	}
	enforced, tokenIDs, err := c.memberships.ActiveTokenIDs(ctx.Context(), tenantID)
	if err != nil {
		if errors.Is(err, service.ErrTenantNotFound) {
			return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("read active memberships")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read active memberships")
	}
	// tokenIds is always a JSON array, never null. The caller's empty-set
	// handling ("enforced with no memberships matches zero vehicles") depends
	// on [] being distinguishable from an absent value, so the envelope
	// enforces it here rather than trusting the service to keep returning
	// []int64{} through future refactors.
	if tokenIDs == nil {
		tokenIDs = []int64{}
	}
	return ctx.JSON(fiber.Map{"enforced": enforced, "tokenIds": tokenIDs})
}

// CreateMembership — POST /v1/tenants/:tenantId/vehicle-memberships
func (c *MembershipsController) CreateMembership(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	var body models.CreateMembershipInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "create membership"); err != nil {
		return err
	}

	m, err := c.memberships.Create(ctx.Context(), tenantID, &body, actorWallet(ctx))
	if err != nil {
		return c.fail(err, tenantID, "create membership")
	}
	return ctx.Status(fiber.StatusCreated).JSON(m)
}

// MoveMembership — POST /v1/tenants/:tenantId/vehicle-memberships/:membershipId/move
//
// An action rather than a PATCH of vehicle_token_id. Move validates differently
// from every other update — the target has to be entitled and free — and this
// service has twice been bitten by tri-state "absent vs empty vs set" bodies on
// general-purpose update endpoints.
func (c *MembershipsController) MoveMembership(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	var body models.MoveMembershipInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "move membership"); err != nil {
		return err
	}

	m, err := c.memberships.Move(ctx.Context(), tenantID, ctx.Params("membershipId"),
		&body, actorWallet(ctx))
	if err != nil {
		return c.fail(err, tenantID, "move membership")
	}
	return ctx.JSON(m)
}

// RenewMembership — POST /v1/tenants/:tenantId/vehicle-memberships/:membershipId/renew
func (c *MembershipsController) RenewMembership(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")

	var body models.RenewMembershipInput
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "renew membership"); err != nil {
		return err
	}

	m, err := c.memberships.Renew(ctx.Context(), tenantID, ctx.Params("membershipId"),
		&body, actorWallet(ctx))
	if err != nil {
		return c.fail(err, tenantID, "renew membership")
	}
	return ctx.JSON(m)
}

// CancelMembership — DELETE /v1/tenants/:tenantId/vehicle-memberships/:membershipId
func (c *MembershipsController) CancelMembership(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "cancel membership"); err != nil {
		return err
	}

	err := c.memberships.Cancel(ctx.Context(), tenantID, ctx.Params("membershipId"))
	if err != nil {
		// Cancelling something already cancelled is the state the caller asked
		// for, so a retry after a timeout is not a failure. Same reasoning as
		// revoking an entitlement twice.
		if errors.Is(err, service.ErrMembershipNotFound) {
			return ctx.SendStatus(fiber.StatusNoContent)
		}
		return c.fail(err, tenantID, "cancel membership")
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// fail maps the service's errors onto statuses the console can act on.
//
// 409 and 422 are distinguished deliberately: 409 means "this vehicle already
// has one, renew or move it instead" and 422 means "that vehicle isn't this
// customer's" — different mistakes with different fixes, and collapsing them
// into 400 would make the console guess.
func (c *MembershipsController) fail(err error, tenantID, op string) error {
	switch {
	case errors.Is(err, service.ErrTenantNotFound):
		return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
	case errors.Is(err, service.ErrMembershipNotFound):
		return fiber.NewError(fiber.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrMembershipExists):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case errors.Is(err, service.ErrVehicleNotEntitled):
		return fiber.NewError(fiber.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, service.ErrNotExplicitMode),
		errors.Is(err, service.ErrInvalidTerm),
		errors.Is(err, service.ErrInvalidStartsAt),
		errors.Is(err, service.ErrSameVehicle):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	c.logger.Err(err).Str("tenant_id", tenantID).Msg(op)
	return fiber.NewError(fiber.StatusInternalServerError, "failed to "+op)
}

func (c *MembershipsController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
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
			Msg("/v1: caller acted on memberships of a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not act on this tenant")
	}
	return nil
}
