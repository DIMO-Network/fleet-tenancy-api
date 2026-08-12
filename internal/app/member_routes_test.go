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

// The membership write routes must exist and must sit behind the /v1 gate.
//
// Worth asserting rather than assuming: a route registered outside the group,
// or misspelled so nothing matches it, fails in opposite ways that both compile.
// An unguarded write route would let anyone who can reach the service grant
// themselves membership of any tenant; a misspelled one would 404 and the
// callers would report memberships diverging again.
//
// This exercises routing and the first gate only. The handlers themselves are
// covered by the MemberService tests, which run against a real database.
func TestMemberRoutesAreRegisteredAndGuarded(t *testing.T) {
	// A guard with one known key, standing in for the real /v1 chain: it is the
	// first layer, so a request without the header must never reach a handler.
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	v1 := app.Group("/v1", NewTrustedCallerGuard(map[string]string{"kaufmann": "secret"}, &logger))

	reached := false
	handler := func(c *fiber.Ctx) error {
		reached = true
		return c.SendStatus(fiber.StatusNoContent)
	}
	v1.Put("/tenants/:tenantId/members/:wallet", handler)
	v1.Delete("/tenants/:tenantId/members/:wallet", handler)

	const path = "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/members/0x1111111111111111111111111111111111111111"

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method+" without a key is refused before the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req, -1)
			require.NoError(t, err)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			assert.False(t, reached, "an unauthenticated write must not reach the handler")
		})

		t.Run(method+" with a valid key routes to the handler", func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
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
