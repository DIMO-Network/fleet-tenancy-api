package controllers

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordedEvent struct {
	invitationID, messageID, status, detail string
	occurredAt                              time.Time
}

type stubEventSink struct {
	events []recordedEvent
	fail   error
}

func (s *stubEventSink) ApplyEmailEvent(_ context.Context, invitationID, messageID, status string, occurredAt time.Time, detail string) error {
	if s.fail != nil {
		return s.fail
	}
	s.events = append(s.events, recordedEvent{invitationID, messageID, status, detail, occurredAt})
	return nil
}

func webhookApp(secret string, sink emailEventSink) *fiber.App {
	logger := zerolog.Nop()
	app := fiber.New()
	wc := NewWebhooksController(&logger, secret, sink)
	app.Post("/webhooks/postmark", wc.HandlePostmark)
	return app
}

func postEvent(t *testing.T, app *fiber.App, password, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if password != "" {
		req.Header.Set("Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte("postmark:"+password)))
	}
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	return resp
}

// The webhook is the one route that authenticates with something other than
// the /v1 chain — Postmark cannot do DIMO JWTs — so its basic-auth gate gets
// its own coverage: the password is the credential, and an unset secret must
// mean the endpoint is off, not open.
func TestPostmarkWebhookAuth(t *testing.T) {
	const delivery = `{"RecordType":"Delivery","MessageID":"pm-1",` +
		`"Metadata":{"invitation_id":"11111111-0000-0000-0000-000000000001"},` +
		`"DeliveredAt":"2026-08-16T12:00:00Z"}`

	t.Run("no credentials is refused", func(t *testing.T) {
		sink := &stubEventSink{}
		resp := postEvent(t, webhookApp("hook-secret", sink), "", delivery)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Empty(t, sink.events)
	})

	t.Run("a wrong password is refused", func(t *testing.T) {
		sink := &stubEventSink{}
		resp := postEvent(t, webhookApp("hook-secret", sink), "wrong", delivery)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Empty(t, sink.events)
	})

	t.Run("an empty configured secret disables the endpoint", func(t *testing.T) {
		sink := &stubEventSink{}
		resp := postEvent(t, webhookApp("", sink), "", delivery)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		// And matching an empty password must not count as authenticated.
		resp = postEvent(t, webhookApp("   ", sink), "   ", delivery)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Empty(t, sink.events)
	})

	t.Run("the right password applies the event", func(t *testing.T) {
		sink := &stubEventSink{}
		resp := postEvent(t, webhookApp("hook-secret", sink), "hook-secret", delivery)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, sink.events, 1)
		ev := sink.events[0]
		assert.Equal(t, "11111111-0000-0000-0000-000000000001", ev.invitationID)
		assert.Equal(t, "pm-1", ev.messageID)
		assert.Equal(t, service.EmailStatusDelivered, ev.status)
		assert.Equal(t, time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), ev.occurredAt.UTC())
	})
}

func TestPostmarkWebhookEvents(t *testing.T) {
	t.Run("bounce carries type and description as detail", func(t *testing.T) {
		sink := &stubEventSink{}
		resp := postEvent(t, webhookApp("s3cr3t-long-enough", sink), "s3cr3t-long-enough",
			`{"RecordType":"Bounce","MessageID":"pm-2","Type":"HardBounce",`+
				`"Description":"address does not exist","BouncedAt":"2026-08-16T13:00:00Z"}`)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, sink.events, 1)
		assert.Equal(t, service.EmailStatusBounced, sink.events[0].status)
		assert.Equal(t, "HardBounce: address does not exist", sink.events[0].detail)
	})

	t.Run("unknown record types are acknowledged and ignored", func(t *testing.T) {
		sink := &stubEventSink{}
		resp := postEvent(t, webhookApp("s3cr3t-long-enough", sink), "s3cr3t-long-enough",
			`{"RecordType":"SpamComplaint","MessageID":"pm-3"}`)
		assert.Equal(t, http.StatusOK, resp.StatusCode,
			"Postmark must not retry events this service will never use")
		assert.Empty(t, sink.events)
	})

	t.Run("unparseable payloads are acknowledged, not retried forever", func(t *testing.T) {
		sink := &stubEventSink{}
		resp := postEvent(t, webhookApp("s3cr3t-long-enough", sink), "s3cr3t-long-enough", `not json`)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, sink.events)
	})

	t.Run("a sink failure answers 500 so Postmark retries", func(t *testing.T) {
		sink := &stubEventSink{fail: assert.AnError}
		resp := postEvent(t, webhookApp("s3cr3t-long-enough", sink), "s3cr3t-long-enough",
			`{"RecordType":"Open","MessageID":"pm-4","ReceivedAt":"2026-08-16T14:00:00Z"}`)
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	})
}
