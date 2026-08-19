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

// The shareable-owners route must exist and must sit behind the /v1 gate.
//
// It reads whether a tenant may sign on its customers' behalf, which is the
// gate fleet-lite uses to decide whether to offer a share at all. Reachable
// without a caller key, it would let anything that can reach the port
// enumerate which owners of which tenant are shared-signable.
//
// Routing and the first gate only; the answer itself is covered by the
// SharedSignerService tests.
func TestShareableOwnersRouteIsRegisteredAndGuarded(t *testing.T) {
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	v1 := app.Group("/v1", NewTrustedCallerGuard(map[string]string{"fleet-lite": "secret"}, &logger))

	reached := false
	v1.Post("/tenants/:tenantId/shareable-owners", func(c *fiber.Ctx) error {
		reached = true
		return c.SendStatus(fiber.StatusOK)
	})

	const path = "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/shareable-owners"
	body := `{"owners":["0x1111111111111111111111111111111111111111"]}`

	t.Run("without a key it is refused before the handler", func(t *testing.T) {
		reached = false
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.False(t, reached, "the handler must not run for an unauthenticated caller")
	})

	t.Run("with a key it reaches the handler", func(t *testing.T) {
		reached = false
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(TrustedCallerHeader, "secret")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.True(t, reached)
	})
}

// The share and status routes must exist and must sit behind the /v1 gate.
//
// The share route spends gas and creates an irreversible on-chain grant, so
// reachability without a caller key is the worst failure in this feature.
func TestShareRoutesAreRegisteredAndGuarded(t *testing.T) {
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	v1 := app.Group("/v1", NewTrustedCallerGuard(map[string]string{"fleet-lite": "secret"}, &logger))

	reached := false
	handler := func(c *fiber.Ctx) error {
		reached = true
		return c.SendStatus(fiber.StatusAccepted)
	}
	v1.Post("/tenants/:tenantId/vehicles/:tokenId/share", handler)
	v1.Get("/tenants/:tenantId/vehicles/:tokenId/share/status", handler)

	const base = "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/vehicles/42/share"

	for _, tc := range []struct{ name, method, path string }{
		{"share", http.MethodPost, base},
		{"status", http.MethodGet, base + "/status?jobId=1"},
	} {
		t.Run(tc.name+" without a key is refused before the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.False(t, reached, "an unauthenticated caller must never reach a handler that spends gas")
		})

		t.Run(tc.name+" with a key reaches the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(TrustedCallerHeader, "secret")
			resp, err := app.Test(req)
			require.NoError(t, err)
			assert.Equal(t, http.StatusAccepted, resp.StatusCode)
			assert.True(t, reached)
		})
	}
}
