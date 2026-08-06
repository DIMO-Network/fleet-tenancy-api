// Package app wires the HTTP surface together.
//
// Implemented: health, version, and GET /v1/authz — the hot path both apps call
// on every request. Still to come: /v1/tenants, /v1/resolve/client-id, the DIMO
// token minter, and the /user/v1 management surface. See the design docs
// referenced in README.md.
package app

import (
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/controllers"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/rs/zerolog"
)

var appCommitHash string

func App(settings *config.Settings, logger *zerolog.Logger, commitHash string, pdb *db.Store) *fiber.App {
	appCommitHash = commitHash

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return ErrorHandler(c, err, logger)
		},
		DisableStartupMessage: true,
	})
	app.Use(fiberrecover.New(fiberrecover.Config{EnableStackTrace: true}))

	app.Get("/health", healthCheck)
	app.Get("/version", getVersion)

	authzCtrl := controllers.NewAuthzController(logger, service.NewAuthzService(logger, pdb))

	// Service-to-service surface. Callers are fleet-lite-app, kaufmann-oracle and
	// the b2b proxy, authenticating with a DIMO developer-license JWT resolved
	// against tenant_credentials.dimo_client_id — the same pattern as
	// kaufmann-oracle's NewDeveloperLicenseTenantResolver. That middleware is not
	// wired yet; the route is registered so the contract is visible and testable.
	v1 := app.Group("/v1")
	v1.Get("/authz", authzCtrl.GetAuthz)

	return app
}

func healthCheck(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "up"})
}

func getVersion(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"commit": appCommitHash})
}

// ErrorHandler logs the recovered error and returns JSON rather than a string.
func ErrorHandler(c *fiber.Ctx, err error, logger *zerolog.Logger) error {
	code := fiber.StatusInternalServerError
	var e *fiber.Error
	if errors.As(err, &e) {
		code = e.Code
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	if code != fiber.StatusNotFound {
		logger.Err(err).
			Str("httpStatusCode", strconv.Itoa(code)).
			Str("httpMethod", c.Method()).
			Str("httpPath", c.Path()).
			Msg("caught an error from http request")
	}
	return c.Status(code).JSON(ErrorRes{Code: code, Message: err.Error()})
}

type ErrorRes struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
