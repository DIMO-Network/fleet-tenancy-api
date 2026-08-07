// Package app wires the HTTP surface together.
//
// Implemented: health, version, GET /v1/authz — the hot path both apps call on
// every request — and GET /v1/resolve/client-id/{clientId}, both behind
// developer-license authentication. Still to come: /v1/tenants, the DIMO token
// minter, and the /user/v1 management surface. See the design docs referenced in
// README.md.
//
// OPEN DECISION, worth settling before more of /v1 lands: authentication
// identifies *which* tenant is calling, but no handler restricts a caller to
// asking about its own tenant. Any holder of a registered developer license can
// currently ask /v1/authz about any tenant id. That is tolerable only because
// the surface is cluster-internal with no ingress. Whether to enforce
// caller == subject depends on which license the callers actually present —
// fleet-lite holds per-tenant credentials and could present the subject
// tenant's, which would make enforcement natural. CallerFrom exposes the caller
// so a handler can log or enforce it without further plumbing.
package app

import (
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/controllers"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	jwtware "github.com/gofiber/contrib/jwt"
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
	resolveCtrl := controllers.NewResolveController(logger, service.NewTenantService(logger, pdb))

	// Service-to-service surface. Callers are fleet-lite-app, kaufmann-oracle and
	// the b2b proxy, authenticating with a DIMO developer-license JWT verified
	// against the DIMO JWKS and resolved to a tenant via
	// tenant_credentials.dimo_client_id.
	//
	// Order matters and is not cosmetic: jwtware must run first so the resolver
	// only ever reads claims from a signature-verified token. Reversed, the
	// ethereum_address claim would be attacker-supplied and the resolver would
	// authenticate anyone.
	//
	// This surface is cluster-internal by design — the chart publishes no
	// ingress for it. The JWT is authentication in depth, not the only wall.
	jwtAuth := jwtware.New(jwtware.Config{
		JWKSetURLs: []string{settings.JwtKeySetURL.String()},
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			logger.Debug().Err(err).Str("path", c.Path()).Msg("/v1: JWT rejected")
			return fiber.NewError(fiber.StatusUnauthorized, "invalid or missing JWT")
		},
	})
	v1 := app.Group("/v1", jwtAuth, NewDeveloperLicenseTenantResolver(pdb, logger))

	v1.Get("/authz", authzCtrl.GetAuthz)
	v1.Get("/resolve/client-id/:clientId", resolveCtrl.ResolveClientID)

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
