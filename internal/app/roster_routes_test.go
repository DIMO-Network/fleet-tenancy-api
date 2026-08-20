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

// The vehicle-metadata route must exist and must sit behind the /v1 gate.
//
// It serves owner, VIN and plate for any token id a caller names. There is no
// enumeration here — the caller must already know the tokens — but reachable
// without a caller key it would answer those questions to anything that can
// reach the port, which is the whole fleet's identifying data.
//
// Routing and the first gate only; the answer itself is covered by the
// RosterService tests.
func TestVehicleMetadataRouteIsRegisteredAndGuarded(t *testing.T) {
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	v1 := app.Group("/v1", NewTrustedCallerGuard(map[string]string{"fleet-lite": "secret"}, &logger))

	reached := false
	v1.Post("/tenants/:tenantId/vehicle-metadata", func(c *fiber.Ctx) error {
		reached = true
		return c.SendStatus(fiber.StatusOK)
	})

	const path = "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/vehicle-metadata"
	body := `{"tokenIds":[192379,192400]}`

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

// The reason this endpoint is a POST rather than a GET with a query parameter.
//
// A tenant's fleet is hundreds of token ids; six hundred of them in a query
// string is several kilobytes of request line, which fiber refuses with a 431
// long before anyone suspects the URL. Asserted rather than commented, because
// "make it a GET, it's a read" is the obvious review note and this is the
// answer to it.
func TestVehicleMetadataAsQueryStringWouldNotFit(t *testing.T) {
	logger := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ReadBufferSize: 4096, // fiber's default
		ErrorHandler:   func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &logger) },
	})
	app.Get("/v1/tenants/:tenantId/vehicle-metadata", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	ids := make([]string, 0, 619)
	for i := 0; i < 619; i++ {
		ids = append(ids, "192379")
	}
	req := httptest.NewRequest(http.MethodGet,
		"/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/vehicle-metadata?tokenIds="+
			strings.Join(ids, ","), nil)
	_, err := app.Test(req)
	require.Error(t, err,
		"a real fleet does not fit in a query string — this is why the route is a POST")
	assert.Contains(t, err.Error(), "small read buffer",
		"and it fails while reading the request line, before any handler or gate runs")
}
