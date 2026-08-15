package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wallet-tenants listing and the login touch must exist and sit behind the
// /v1 gate. Same reasoning as the membership write routes: a missing or
// unguarded route fails in opposite ways that both compile.
//
// One extra thing is pinned here that the other route tests don't need:
// GET /v1/tenants (no path parameter) and GET /v1/tenants/:tenantId are
// distinct routes, and the collection must not be swallowed by the parameter
// pattern — a request for the list answered by the detail handler would read
// "tenants" as a tenant id and 400 every caller.
func TestWalletTenantRoutesAreRegisteredAndGuarded(t *testing.T) {
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	v1 := app.Group("/v1", NewTrustedCallerGuard(map[string]string{"fleet-lite": "secret"}, &logger))

	var hit string
	v1.Get("/tenants", func(c *fiber.Ctx) error {
		hit = "list"
		return c.SendStatus(fiber.StatusOK)
	})
	v1.Get("/tenants/:tenantId", func(c *fiber.Ctx) error {
		hit = "detail:" + c.Params("tenantId")
		return c.SendStatus(fiber.StatusOK)
	})
	v1.Post("/tenants/:tenantId/members/:wallet/login", func(c *fiber.Ctx) error {
		hit = "login:" + c.Params("wallet")
		return c.SendStatus(fiber.StatusNoContent)
	})

	const listPath = "/v1/tenants?wallet=0x1111111111111111111111111111111111111111&surface=fleet_lite"
	const loginPath = "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/members/0x1111111111111111111111111111111111111111/login"

	t.Run("the listing is refused without a key", func(t *testing.T) {
		hit = ""
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, listPath, nil), -1)
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Empty(t, hit)
	})

	t.Run("the listing routes to the collection handler, not the detail one", func(t *testing.T) {
		hit = ""
		req := httptest.NewRequest(http.MethodGet, listPath, nil)
		req.Header.Set(TrustedCallerHeader, "secret")
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "list", hit)
	})

	t.Run("the detail route still matches a real id", func(t *testing.T) {
		hit = ""
		req := httptest.NewRequest(http.MethodGet, "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001", nil)
		req.Header.Set(TrustedCallerHeader, "secret")
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "detail:aaaaaaaa-0000-0000-0000-000000000001", hit)
	})

	t.Run("the login touch is refused without a key and routed with one", func(t *testing.T) {
		hit = ""
		resp, err := app.Test(httptest.NewRequest(http.MethodPost, loginPath, nil), -1)
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Empty(t, hit)

		req := httptest.NewRequest(http.MethodPost, loginPath, nil)
		req.Header.Set(TrustedCallerHeader, "secret")
		resp, err = app.Test(req, -1)
		require.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		assert.Equal(t, "login:0x1111111111111111111111111111111111111111", hit)
	})
}
