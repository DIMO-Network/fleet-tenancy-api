package app

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fleetLiteKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	kaufmannKey  = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func guardApp(t *testing.T) *fiber.App {
	t.Helper()
	l := zerolog.Nop()
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &l) },
	})
	keys := map[string]string{"fleet-lite-app": fleetLiteKey, "kaufmann-oracle": kaufmannKey}
	app.Get("/v1/thing", NewTrustedCallerGuard(keys, &l), func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"ok": true, "caller": TrustedCallerFrom(c)})
	})
	return app
}

func callWithKey(t *testing.T, app *fiber.App, key string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/thing", nil)
	if key != "" {
		req.Header.Set(TrustedCallerHeader, key)
	}
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func TestTrustedCallerGuard(t *testing.T) {
	app := guardApp(t)

	t.Run("a known key is admitted and names its caller", func(t *testing.T) {
		code, body := callWithKey(t, app, fleetLiteKey)
		assert.Equal(t, fiber.StatusOK, code)
		assert.Contains(t, body, `"caller":"fleet-lite-app"`)
	})

	t.Run("each caller is identified by its own key", func(t *testing.T) {
		code, body := callWithKey(t, app, kaufmannKey)
		assert.Equal(t, fiber.StatusOK, code)
		assert.Contains(t, body, `"caller":"kaufmann-oracle"`)
	})

	t.Run("no header is rejected", func(t *testing.T) {
		code, body := callWithKey(t, app, "")
		assert.Equal(t, fiber.StatusUnauthorized, code)
		assert.Contains(t, body, TrustedCallerHeader)
	})

	t.Run("an unknown key is rejected", func(t *testing.T) {
		code, _ := callWithKey(t, app, strings.Repeat("z", 64))
		assert.Equal(t, fiber.StatusUnauthorized, code)
	})

	// Prefixes and near-misses must fail: a comparison that stopped early would
	// leak how much of a key was right.
	t.Run("a prefix of a valid key is rejected", func(t *testing.T) {
		code, _ := callWithKey(t, app, fleetLiteKey[:len(fleetLiteKey)-1])
		assert.Equal(t, fiber.StatusUnauthorized, code)
	})

	// HTTP strips leading and trailing whitespace from header values, so this
	// arrives already trimmed and must succeed. The config parser trims to
	// match, so a key stored with a stray newline still authenticates rather
	// than failing in a way that looks like the wrong key entirely.
	t.Run("surrounding whitespace is stripped by the transport and still works", func(t *testing.T) {
		code, body := callWithKey(t, app, fleetLiteKey+" ")
		assert.Equal(t, fiber.StatusOK, code)
		assert.Contains(t, body, `"caller":"fleet-lite-app"`)
	})

	t.Run("case matters", func(t *testing.T) {
		code, _ := callWithKey(t, app, strings.ToUpper(fleetLiteKey))
		assert.Equal(t, fiber.StatusUnauthorized, code)
	})

	// The failure that would matter most: an empty key set must close the door,
	// not hold it open. Settings.Validate already refuses to boot on this
	// outside local, so this is the second line of defence.
	t.Run("no configured keys admits nobody", func(t *testing.T) {
		l := zerolog.Nop()
		empty := fiber.New(fiber.Config{
			ErrorHandler: func(c *fiber.Ctx, err error) error { return ErrorHandler(c, err, &l) },
		})
		empty.Get("/v1/thing", NewTrustedCallerGuard(map[string]string{}, &l), func(c *fiber.Ctx) error {
			return c.JSON(fiber.Map{"ok": true})
		})
		code, _ := callWithKey(t, empty, fleetLiteKey)
		assert.Equal(t, fiber.StatusUnauthorized, code)

		code, _ = callWithKey(t, empty, "")
		assert.Equal(t, fiber.StatusUnauthorized, code)
	})

	t.Run("the rejection body never echoes the presented key", func(t *testing.T) {
		secretish := strings.Repeat("q", 64)
		_, body := callWithKey(t, app, secretish)
		assert.NotContains(t, body, secretish, "an echoed key is a key in the caller's logs")
	})
}

func TestTrustedCallerFromIsEmptyWhenGuardDidNotRun(t *testing.T) {
	app := fiber.New()
	got := "unset"
	app.Get("/x", func(c *fiber.Ctx) error {
		got = TrustedCallerFrom(c)
		return c.SendStatus(fiber.StatusOK)
	})
	_, err := app.Test(httptest.NewRequest("GET", "/x", nil), -1)
	require.NoError(t, err)
	assert.Equal(t, "", got)
}
