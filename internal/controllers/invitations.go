package controllers

import (
	"errors"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// InvitationsController serves the invitation lifecycle on the
// service-to-service surface. CRUD sits behind the same three gates as every
// other tenant-scoped route; the human-level capability check (manage_members)
// belongs to the calling app — kaufmann's BFF pattern for the console,
// fleet-lite's own gate for customers — while this service checks the caller
// TENANT's scope, the same split as member provisioning (see
// TenantsController's header comment).
//
// Accept is the exception with no tenant in its path: the token resolves the
// tenant, so the scope check runs after resolution, inside the service, via
// the authorize callback.
type InvitationsController struct {
	logger      *zerolog.Logger
	invitations *service.InvitationService
	tenants     *service.TenantService
	caller      CallerResolver
}

func NewInvitationsController(logger *zerolog.Logger, invitations *service.InvitationService,
	tenants *service.TenantService, caller CallerResolver) *InvitationsController {
	return &InvitationsController{logger: logger, invitations: invitations, tenants: tenants, caller: caller}
}

// List — GET /v1/tenants/:tenantId/invitations
func (c *InvitationsController) List(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "list invitations"); err != nil {
		return err
	}
	rows, err := c.invitations.List(ctx.Context(), tenantID)
	if err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("list invitations")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list invitations")
	}
	return ctx.JSON(fiber.Map{"invitations": rows})
}

// Create — POST /v1/tenants/:tenantId/invitations
//
// 201 with emailSent=false is a partial success, not a failure: the record is
// authoritative and the email is courtesy, so a Postmark outage leaves a
// resendable invitation rather than a 5xx.
func (c *InvitationsController) Create(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	var body models.InvitationCreate
	if err := ctx.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := c.assertScope(ctx, tenantID, "create invitation"); err != nil {
		return err
	}
	// The audit column records the *authenticated* caller, never the body —
	// and only when it is genuinely another tenant issuing the invite (the
	// operator console); a tenant inviting its own member carries NULL.
	if caller := c.caller(ctx); caller != nil && caller.TenantID != tenantID {
		body.CreatedByTenantID = caller.TenantID
	}

	inv, err := c.invitations.Create(ctx.Context(), tenantID, &body)
	if err != nil {
		if inv != nil && errors.Is(err, service.ErrEmailNotSent) {
			c.logger.Warn().Err(err).Str("tenant_id", tenantID).
				Msg("invitation created but email not sent")
			return ctx.Status(fiber.StatusCreated).JSON(withEmailSent(inv, false))
		}
		if errors.Is(err, service.ErrTenantNotFound) {
			return fiber.NewError(fiber.StatusForbidden, "unknown tenant")
		}
		if isInvitationBadRequest(err) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("create invitation")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to create invitation")
	}
	return ctx.Status(fiber.StatusCreated).JSON(withEmailSent(inv, true))
}

// Revoke — DELETE /v1/tenants/:tenantId/invitations/:invitationId
//
// 204 whether or not anything was pending: revoking what is already dead is
// the state the caller asked for, and a retry after a timeout must not read
// as failure.
func (c *InvitationsController) Revoke(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "revoke invitation"); err != nil {
		return err
	}
	if err := c.invitations.Revoke(ctx.Context(), tenantID, ctx.Params("invitationId")); err != nil {
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("revoke invitation")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to revoke invitation")
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

type resendInvitationRequest struct {
	// ActorWallet is who pressed resend in the calling app, for the email's
	// "invited by" line; empty falls back to the original inviter.
	ActorWallet string `json:"actorWallet"`
	Locale      string `json:"locale"`
}

// Resend — POST /v1/tenants/:tenantId/invitations/:invitationId/resend
//
// An action, not a PATCH: it mints a fresh token and kills the old link, which
// is a side effect no field update should imply.
func (c *InvitationsController) Resend(ctx *fiber.Ctx) error {
	tenantID := ctx.Params("tenantId")
	if err := c.assertScope(ctx, tenantID, "resend invitation"); err != nil {
		return err
	}
	// Body is optional; an unparseable or empty body falls back to defaults.
	var body resendInvitationRequest
	_ = ctx.BodyParser(&body)

	inv, err := c.invitations.Resend(ctx.Context(), tenantID, ctx.Params("invitationId"),
		body.ActorWallet, body.Locale)
	if err != nil {
		if errors.Is(err, service.ErrInviteInvalid) {
			return fiber.NewError(fiber.StatusNotFound, "no pending invitation to resend")
		}
		if inv != nil && errors.Is(err, service.ErrEmailNotSent) {
			c.logger.Warn().Err(err).Str("tenant_id", tenantID).
				Msg("invitation token refreshed but email not sent")
			return ctx.JSON(withEmailSent(inv, false))
		}
		c.logger.Err(err).Str("tenant_id", tenantID).Msg("resend invitation")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to resend invitation")
	}
	return ctx.JSON(withEmailSent(inv, true))
}

// Accept — POST /v1/invitations/accept
//
// No tenant in the path: the token resolves it, exactly as in the flow this
// ports. The token authorizes the accept; the trusted service caller asserts
// the wallet — the same trust the membership write-through already extends,
// since that caller could PUT the membership directly. Caller scope for the
// resolved tenant is still checked, inside the service once the tenant is
// known, so a caller cannot consume an invitation into a tenant it could not
// otherwise touch.
//
// 410 for a dead token (unknown, superseded, used, expired, revoked) — the
// caller shows "this link is no longer valid" and nothing distinguishes which,
// on purpose.
func (c *InvitationsController) Accept(ctx *fiber.Ctx) error {
	var body models.InvitationAccept
	if err := ctx.BodyParser(&body); err != nil || body.Token == "" || body.Wallet == "" {
		return fiber.NewError(fiber.StatusBadRequest, "token and wallet are required")
	}

	inv, err := c.invitations.Accept(ctx.Context(), &body, func(tenantID string) error {
		return c.assertScope(ctx, tenantID, "accept invitation")
	})
	if err != nil {
		var fe *fiber.Error
		if errors.As(err, &fe) {
			// The authorize callback already chose the status.
			return err
		}
		if errors.Is(err, service.ErrInviteInvalid) {
			return fiber.NewError(fiber.StatusGone, err.Error())
		}
		if isInvitationBadRequest(err) {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		c.logger.Err(err).Msg("accept invitation")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to accept invitation")
	}
	return ctx.JSON(inv)
}

func (c *InvitationsController) assertScope(ctx *fiber.Ctx, tenantID, op string) error {
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

// withEmailSent stamps the delivery flag create/resend responses carry.
func withEmailSent(inv *models.Invitation, sent bool) *models.Invitation {
	inv.EmailSent = &sent
	return inv
}

// isInvitationBadRequest distinguishes caller error from server fault. Kept
// narrow deliberately, like the member controller's — a broad match would
// eventually turn a database failure into a 400 and hide a real outage.
func isInvitationBadRequest(err error) bool {
	msg := err.Error()
	return msg == "email is required" ||
		msg == "invitedByWallet is required" ||
		msg == "scopeGroupIds is required (null for unrestricted, [] for no groups)" ||
		msg == "one or more group ids do not exist in this tenant" ||
		msg == "token and wallet are required"
}
