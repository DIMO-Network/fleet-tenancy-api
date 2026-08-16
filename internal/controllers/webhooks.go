package controllers

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// emailEventSink is the slice of InvitationService the webhook needs — an
// interface so the auth and parsing paths are testable without a database.
type emailEventSink interface {
	ApplyEmailEvent(ctx context.Context, invitationID, messageID, status string, occurredAt time.Time, detail string) error
}

// WebhooksController receives Postmark's delivery/open/bounce events for
// invitation emails and feeds them into the email-tracking columns. Ported
// from fleet-lite, which retires its receiver in P4 of the invitations move —
// tracking upgrades rows, and the rows live here now.
//
// Deployment note: this service deliberately publishes no ingress, so Postmark
// cannot reach this endpoint until the chart exposes exactly this path. That
// is P2's repoint work; until then the route exists and is exercised only by
// tests.
type WebhooksController struct {
	logger *zerolog.Logger
	// secret is POSTMARK_WEBHOOK_SECRET. Empty disables the endpoint.
	secret      string
	invitations emailEventSink
}

func NewWebhooksController(logger *zerolog.Logger, secret string, invitations emailEventSink) *WebhooksController {
	return &WebhooksController{logger: logger, secret: strings.TrimSpace(secret), invitations: invitations}
}

// postmarkEvent is the superset of the Delivery/Bounce/Open webhook payload
// fields consumed here. RecordType discriminates; unknown types are ignored.
type postmarkEvent struct {
	RecordType  string            `json:"RecordType"`
	MessageID   string            `json:"MessageID"`
	Metadata    map[string]string `json:"Metadata"`
	DeliveredAt string            `json:"DeliveredAt"` // Delivery
	BouncedAt   string            `json:"BouncedAt"`   // Bounce
	ReceivedAt  string            `json:"ReceivedAt"`  // Open
	Type        string            `json:"Type"`        // Bounce, e.g. HardBounce
	Description string            `json:"Description"` // Bounce
}

// HandlePostmark — POST /webhooks/postmark. Postmark cannot do DIMO JWTs, so
// this authenticates with basic auth whose password is the webhook's own
// secret. Bad credentials get 403, which also tells Postmark to stop
// retrying; everything else gets 200 — the handler is idempotent and
// unmatched events are deliberately swallowed (both receivers tolerate
// unknown message ids silently, which is what makes the P2 repoint safe to
// coordinate loosely).
func (wc *WebhooksController) HandlePostmark(c *fiber.Ctx) error {
	if !wc.authorized(c) {
		return fiber.NewError(fiber.StatusForbidden, "invalid webhook credentials")
	}

	var ev postmarkEvent
	if err := c.BodyParser(&ev); err != nil {
		wc.logger.Warn().Err(err).Msg("invite flow: unparseable postmark webhook payload")
		return c.JSON(fiber.Map{"ok": true})
	}

	var status string
	var occurredAt time.Time
	var detail string
	switch ev.RecordType {
	case "Delivery":
		status = service.EmailStatusDelivered
		occurredAt = parsePostmarkTime(ev.DeliveredAt)
	case "Open":
		status = service.EmailStatusOpened
		occurredAt = parsePostmarkTime(ev.ReceivedAt)
	case "Bounce":
		status = service.EmailStatusBounced
		occurredAt = parsePostmarkTime(ev.BouncedAt)
		detail = strings.TrimSpace(strings.TrimSuffix(ev.Type+": "+ev.Description, ": "))
	default:
		// SpamComplaint, Click, SubscriptionChange, … — not tracked.
		return c.JSON(fiber.Map{"ok": true})
	}

	if err := wc.invitations.ApplyEmailEvent(c.Context(),
		ev.Metadata["invitation_id"], ev.MessageID, status, occurredAt, detail); err != nil {
		// 500 so Postmark retries — the event matched an invitation but a
		// transient failure kept it from being recorded.
		wc.logger.Err(err).Str("recordType", ev.RecordType).Str("messageId", ev.MessageID).
			Msg("invite flow: could not apply postmark email event")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to record event")
	}
	return c.JSON(fiber.Map{"ok": true})
}

// authorized checks the basic-auth password against the configured secret in
// constant time. The username is ignored — the password is the credential. An
// empty configured secret disables the endpoint entirely.
func (wc *WebhooksController) authorized(c *fiber.Ctx) bool {
	if wc.secret == "" {
		return false
	}
	header := c.Get(fiber.HeaderAuthorization)
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	_, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(wc.secret)) == 1
}

// parsePostmarkTime parses Postmark's ISO 8601 timestamps; zero time on
// failure (the service falls back to now()).
func parsePostmarkTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
