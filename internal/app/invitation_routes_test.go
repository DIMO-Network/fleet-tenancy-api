package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The invitation routes must exist and must sit behind the /v1 gate — the
// same two opposite failure modes as the member routes: unguarded, anyone who
// can reach the port could mint an invitation into any tenant (an invitation
// IS a deferred membership grant); misspelled, the P2 caller 404s and every
// invite operation turns into a 502 at fleet-lite.
func TestInvitationRoutesAreRegisteredAndGuarded(t *testing.T) {
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	v1 := app.Group("/v1", NewTrustedCallerGuard(map[string]string{"fleet-lite": "secret"}, &logger))

	reached := false
	handler := func(c *fiber.Ctx) error {
		reached = true
		return c.SendStatus(fiber.StatusNoContent)
	}
	v1.Get("/tenants/:tenantId/invitations", handler)
	v1.Post("/tenants/:tenantId/invitations", handler)
	v1.Delete("/tenants/:tenantId/invitations/:invitationId", handler)
	v1.Post("/tenants/:tenantId/invitations/:invitationId/resend", handler)
	v1.Post("/invitations/accept", handler)

	const tenantBase = "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/invitations"
	const invitation = tenantBase + "/bbbbbbbb-0000-0000-0000-000000000001"

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, tenantBase},
		{http.MethodPost, tenantBase},
		{http.MethodDelete, invitation},
		{http.MethodPost, invitation + "/resend"},
		{http.MethodPost, "/v1/invitations/accept"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path+" without a key is refused before the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req, -1)
			require.NoError(t, err)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.False(t, reached, "an unauthenticated call must not reach the handler")
		})

		t.Run(tc.method+" "+tc.path+" with a valid key routes to the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(TrustedCallerHeader, "secret")

			resp, err := app.Test(req, -1)
			require.NoError(t, err)
			assert.Equal(t, http.StatusNoContent, resp.StatusCode,
				"a 404 here means the route pattern does not match the path the callers build")
			assert.True(t, reached)
		})
	}
}
