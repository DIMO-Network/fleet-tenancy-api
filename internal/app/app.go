// Package app wires the HTTP surface together.
//
// Scaffold: health and version only. The tenancy endpoints — /v1/authz,
// /v1/tenants, /v1/resolve/client-id, the /user/v1 management surface — are
// designed but not implemented. See the design docs referenced in README.md.
package app

import (
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/rs/zerolog"
)

var appCommitHash string

func App(settings *config.Settings, logger *zerolog.Logger, commitHash string) *fiber.App {
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
