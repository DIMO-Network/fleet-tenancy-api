package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// MembersController is the write half of the service-to-service surface.
//
// Same three gates as every other /v1 route — trusted-caller key, developer
// licence JWT, caller scope — and scope matters more here than on a read: a
// caller that could write memberships into a tenant outside its own would be
// granting itself access to somebody else's fleet.
type MembersController struct {
	logger  *zerolog.Logger
	members *service.MemberService
	tenants *service.TenantService
	caller  CallerResolver
}

func NewMembersController(logger *zerolog.Logger, members *service.MemberService,
	tenants *service.TenantService, caller CallerResolver) *MembersController {
	return &MembersController{logger: logger, members: members, tenants: tenants, caller: caller}
}

// PutMember — PUT /v1/tenants/:tenantId/members/:wallet
//
// Idempotent by construction: the body is the whole membership, so a repeat of
// the same call is a no-op and a caller retrying after a timeout cannot double
// anything. That matters because the callers write locally and here, and a
// failure between the two is retried.
func (c *MembersController) PutMember(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	wallet := ctx.Params("wallet")
	if tenantID == "" || wallet == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenantId and wallet are required")
	}

	var body models.MemberWrite
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	if err := c.assertScope(ctx, tenantID, "PUT member"); err != nil {
		return err
	}

	if err := c.members.Upsert(ctx.Context(), tenantID, wallet, &body); err != nil {
		if errors.Is(err, service.ErrTenantNotFound) {
			// 403 rather than 404, matching the read path: a caller that got
			// past the scope check named a tenant that does not exist, and a
			// 404 would confirm which uuids are real.
			return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
		}
		// A missing or malformed scopeGroupIds is the caller's mistake, and the
		// one most worth naming precisely — silently defaulting it is how a
		// membership ends up unrestricted by accident.
		if isBadRequest(err) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("upsert membership")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to write membership")
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// DeleteMember — DELETE /v1/tenants/:tenantId/members/:wallet
func (c *MembersController) DeleteMember(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	wallet := ctx.Params("wallet")
	if tenantID == "" || wallet == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenantId and wallet are required")
	}

	if err := c.assertScope(ctx, tenantID, "DELETE member"); err != nil {
		return err
	}

	if err := c.members.Remove(ctx.Context(), tenantID, wallet); err != nil {
		// Removing something already gone is the state the caller asked for.
		// 204 keeps a retry after a timeout from looking like a failure.
		if errors.Is(err, service.ErrMemberNotFound) {
			return ctx.SendStatus(fiber.StatusNoContent)
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("remove membership")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to remove membership")
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// LoginTouch — POST /v1/tenants/:tenantId/members/:wallet/login
//
// Stamps the membership's last_login_at and captures the login email, which is
// telemetry rather than authorization — the sync tiering reads the stamp, and
// nothing gates on it. 204 whether or not the membership was found: a member
// revoked inside the authz cache window can still send this, and failing their
// request over a lost stamp would fail a session that authz already admitted.
func (c *MembersController) LoginTouch(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	wallet := ctx.Params("wallet")
	if tenantID == "" || wallet == "" {
		return fiber.NewError(fiber.StatusBadRequest, "tenantId and wallet are required")
	}

	var body struct {
		Email string `json:"email"`
	}
	// Body is optional; a parse failure just means no email arrived.
	_ = ctx.BodyParser(&body)

	if err := c.assertScope(ctx, tenantID, "login touch"); err != nil {
		return err
	}

	found, err := c.members.TouchLogin(ctx.Context(), tenantID, wallet, body.Email)
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("login touch")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to record login")
	}
	if !found {
		c.logger.Debug().Str("tenant_id", tenantID).Str("wallet", wallet).
			Msg("login touch for a wallet with no membership")
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (c *MembersController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
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
			Msg("/v1: caller tried to write into a tenant outside its scope")
		return fiber.NewError(fiber.StatusForbidden, "caller may not write to this tenant")
	}
	return nil
}

// isBadRequest distinguishes caller error from server fault for the validation
// the service performs. Kept narrow deliberately — a broad string match would
// eventually turn a database failure into a 400 and hide a real outage.
func isBadRequest(err error) bool {
	msg := err.Error()
	return msg == "scopeGroupIds is required (null for unrestricted, [] for no groups)" ||
		msg == "tenantID and wallet are required" ||
		msg == "name is required" ||
		msg == "clientId and apiKey are required" ||
		msg == "ownerWallet is required"
}
