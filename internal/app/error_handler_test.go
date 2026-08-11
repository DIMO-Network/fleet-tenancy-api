package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The level matters more than the message. Every rejected /v1 call is a 401 or
// 403 and this service rejects by design, so logging those at error level makes
// routine enforcement indistinguishable from the service being broken — and
// feeds any error-rate alerting built on this stream.
func TestErrorHandlerLogLevels(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		message   string
		wantLevel string
		wantLog   bool
	}{
		{"unauthorized is a warning", fiber.StatusUnauthorized, "invalid X-Tenancy-Key", "warn", true},
		{"forbidden is a warning", fiber.StatusForbidden, "caller may not query this tenant", "warn", true},
		{"bad request is a warning", fiber.StatusBadRequest, "wallet and tenant_id are required", "warn", true},
		{"server error stays an error", fiber.StatusInternalServerError, "authorization lookup failed", "error", true},
		{"bad gateway stays an error", fiber.StatusBadGateway, "upstream", "error", true},
		// An unrouted path is neither a fault nor worth a line per scan.
		{"not found is silent", fiber.StatusNotFound, "nope", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := zerolog.New(&buf)

			app := fiber.New(fiber.Config{
				ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &l) },
			})
			app.Get("/boom", func(_ *fiber.Ctx) error {
				return fiber.NewError(tc.status, tc.message)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
			require.NoError(t, err)
			assert.Equal(t, tc.status, resp.StatusCode)

			if !tc.wantLog {
				assert.Empty(t, buf.String(), "expected no log line")
				return
			}

			var entry map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), "log line: %s", buf.String())
			assert.Equal(t, tc.wantLevel, entry["level"])
			assert.Equal(t, tc.message, entry["error"])
			assert.Equal(t, "GET", entry["httpMethod"])
			assert.Equal(t, "/boom", entry["httpPath"])
		})
	}
}

// The response body is the caller's only diagnostic, and the tenancy clients
// classify rejections by matching on its message — so the shape has to hold
// regardless of what the log did.
func TestErrorHandlerResponseBody(t *testing.T) {
	l := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &l) },
	})
	app.Get("/boom", func(_ *fiber.Ctx) error {
		return fiber.NewError(fiber.StatusUnauthorized, "invalid "+TrustedCallerHeader)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	require.NoError(t, err)

	var body ErrorRes
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, fiber.StatusUnauthorized, body.Code)
	assert.Contains(t, body.Message, TrustedCallerHeader)
	assert.Equal(t, fiber.MIMEApplicationJSON, resp.Header.Get(fiber.HeaderContentType))
}
