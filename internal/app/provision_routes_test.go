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

// The provisioning and token-minter routes must sit behind the /v1 gate — the
// same property the member-routes test pins, and worth more here: an unguarded
// dimo-token route would mint any tenant's developer JWT for anyone who can
// reach the port, which is the credential leaving the service in all but name.
//
// This exercises routing and the first gate only; the handlers are covered by
// the service tests.
func TestProvisionRoutesAreRegisteredAndGuarded(t *testing.T) {
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	v1 := app.Group("/v1", NewTrustedCallerGuard(map[string]string{"b2b": "secret"}, &logger))

	reached := false
	handler := func(c *fiber.Ctx) error {
		reached = true
		return c.SendStatus(fiber.StatusOK)
	}
	v1.Post("/tenants/:tenantId/members/provision", handler)
	v1.Get("/tenants/:tenantId/dimo-token", handler)

	routes := map[string]string{
		http.MethodPost: "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/members/provision",
		http.MethodGet:  "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/dimo-token",
	}

	for method, path := range routes {
		t.Run(method+" without a key is refused before the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req, -1)
			require.NoError(t, err)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.False(t, reached, "an unauthenticated request must not reach the handler")
		})

		t.Run(method+" with a valid key routes to the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(TrustedCallerHeader, "secret")

			resp, err := app.Test(req, -1)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.True(t, reached)
		})
	}
}
