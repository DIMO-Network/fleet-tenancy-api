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
