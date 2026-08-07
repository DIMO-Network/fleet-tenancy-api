package app

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These cover the paths that reject before any database access. The resolver
// dereferences pdb only after the claim checks pass, so a nil store is safe
// here and keeps the test honest about which paths are actually exercised —
// the lookup itself is covered by TestResolveByClientID against a real schema.
func newResolverApp(t *testing.T, locals func(c *fiber.Ctx)) *fiber.App {
	t.Helper()
	l := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &l) },
	})
	app.Use(func(c *fiber.Ctx) error {
		if locals != nil {
			locals(c)
		}
		return c.Next()
	})
	app.Get("/v1/thing", NewDeveloperLicenseTenantResolver(nil, &l), func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"ok": true})
	})
	return app
}

func statusAndBody(t *testing.T, app *fiber.App) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", "/v1/thing", nil), -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func TestResolverRejectsBeforeTouchingTheDatabase(t *testing.T) {
	t.Run("no JWT in locals is 401", func(t *testing.T) {
		code, body := statusAndBody(t, newResolverApp(t, nil))
		assert.Equal(t, fiber.StatusUnauthorized, code)
		assert.Contains(t, body, "missing JWT")
	})

	t.Run("a non-token value in locals is 401, not a panic", func(t *testing.T) {
		app := newResolverApp(t, func(c *fiber.Ctx) { c.Locals("user", "not-a-token") })
		code, body := statusAndBody(t, app)
		assert.Equal(t, fiber.StatusUnauthorized, code)
		assert.Contains(t, body, "missing JWT")
	})

	t.Run("token without an ethereum_address claim is 401", func(t *testing.T) {
		app := newResolverApp(t, func(c *fiber.Ctx) {
			c.Locals("user", &jwt.Token{Claims: jwt.MapClaims{"sub": "someone"}})
		})
		code, body := statusAndBody(t, app)
		assert.Equal(t, fiber.StatusUnauthorized, code)
		assert.Contains(t, body, "ethereum_address")
	})

	t.Run("empty ethereum_address is 401", func(t *testing.T) {
		app := newResolverApp(t, func(c *fiber.Ctx) {
			c.Locals("user", &jwt.Token{Claims: jwt.MapClaims{"ethereum_address": ""}})
		})
		code, _ := statusAndBody(t, app)
		assert.Equal(t, fiber.StatusUnauthorized, code)
	})

	t.Run("non-string ethereum_address is 401 rather than a type panic", func(t *testing.T) {
		app := newResolverApp(t, func(c *fiber.Ctx) {
			c.Locals("user", &jwt.Token{Claims: jwt.MapClaims{"ethereum_address": 12345}})
		})
		code, _ := statusAndBody(t, app)
		assert.Equal(t, fiber.StatusUnauthorized, code)
	})
}

// The whole point of the middleware is that an unauthenticated request cannot
// reach a handler. If this ever passes a request through, /v1 is open again.
func TestResolverDoesNotFallThroughToTheHandler(t *testing.T) {
	resp, err := newResolverApp(t, nil).Test(httptest.NewRequest("GET", "/v1/thing", nil), -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)

	var payload map[string]any
	_ = json.Unmarshal(b, &payload)
	assert.NotEqual(t, true, payload["ok"], "handler ran without an authenticated caller")
}

func TestCallerFromIsNilWhenUnset(t *testing.T) {
	app := fiber.New()
	var got any = "sentinel"
	app.Get("/x", func(c *fiber.Ctx) error {
		got = CallerFrom(c)
		return c.SendStatus(fiber.StatusOK)
	})
	_, err := app.Test(httptest.NewRequest("GET", "/x", nil), -1)
	require.NoError(t, err)
	assert.Nil(t, got, "a handler must be able to tell that no caller was resolved")
}
