// Package app wires the HTTP surface together.
//
// Implemented: health, version, GET /v1/authz — the hot path both apps call on
// every request — and GET /v1/resolve/client-id/{clientId}, both behind
// developer-license authentication. Still to come: /v1/tenants, the DIMO token
// minter, and the /user/v1 management surface. See the design docs referenced in
// README.md.
//
// Callers are scoped, not merely authenticated: TenantService.CallerMayAccess
// bounds every /v1 handler to tenants whose effective credential is the
// caller's — itself, a child holding no license of its own, or a tenant it holds
// a delegation over — with an explicit service-caller flag for a shared proxy.
//
// The rule mirrors the architecture's credential resolution rule on purpose. The
// tempting alternative, "caller must equal subject", would pass today while
// every tenant is unparented and break on the first operator-managed customer,
// since such a customer holds no license and is reached with its operator's.
package app

import (
	"errors"
	"strconv"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/controllers"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
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

	tenantSvc := service.NewTenantService(logger, pdb)
	memberSvc := service.NewMemberService(logger, pdb)
	authzCtrl := controllers.NewAuthzController(logger, service.NewAuthzService(logger, pdb), tenantSvc, CallerFrom)
	resolveCtrl := controllers.NewResolveController(logger, tenantSvc, CallerFrom)
	membersCtrl := controllers.NewMembersController(logger, memberSvc, tenantSvc, CallerFrom)
	tenantsCtrl := controllers.NewTenantsController(logger, tenantSvc, CallerFrom)
	entitlementsCtrl := controllers.NewEntitlementsController(logger,
		service.NewEntitlementService(logger, pdb), tenantSvc, CallerFrom)

	// The effective-credential surface: the token minter and on-behalf
	// provisioning. The credential service is the only code that decrypts a
	// tenant key at runtime; everything else moves tokens, not keys.
	credSvc := service.NewCredentialService(logger, pdb, settings,
		gateway.NewIdentityAPIService(logger, settings.IdentityAPIEndpoint))
	provisionSvc := service.NewProvisionService(logger, pdb, memberSvc, credSvc,
		gateway.NewAccountsAPIService(logger, settings.AccountsAPIEndpoint))
	provisionCtrl := controllers.NewProvisionController(logger, provisionSvc, credSvc, tenantSvc, CallerFrom)
	groupsCtrl := controllers.NewGroupsController(logger, service.NewGroupService(logger, pdb), tenantSvc, CallerFrom)

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
	// Three layers, answering three different questions:
	//   NewTrustedCallerGuard          is this a trusted application?
	//   jwtware + resolver             which tenant is it acting as?
	//   TenantService.CallerMayAccess  may that tenant see the one being asked about?
	//
	// The surface is also cluster-internal by design — the chart publishes no
	// ingress for it.
	jwtAuth := jwtware.New(jwtware.Config{
		JWKSetURLs: []string{settings.JwtKeySetURL.String()},
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			logger.Debug().Err(err).Str("path", c.Path()).Msg("/v1: JWT rejected")
			return fiber.NewError(fiber.StatusUnauthorized, "invalid or missing JWT")
		},
	})
	// Settings.Validate has already refused to boot outside local if this is
	// empty, so a nil map here can only mean local development.
	trustedKeys, kerr := settings.ParsedTrustedCallerKeys()
	if kerr != nil {
		logger.Fatal().Err(kerr).Msg("TRUSTED_CALLER_KEYS is invalid")
	}
	logger.Info().Int("trusted_callers", len(trustedKeys)).Msg("/v1 gate configured")

	v1 := app.Group("/v1",
		NewTrustedCallerGuard(trustedKeys, logger),
		jwtAuth,
		NewDeveloperLicenseTenantResolver(pdb, logger))

	v1.Get("/authz", authzCtrl.GetAuthz)
	v1.Get("/resolve/client-id/:clientId", resolveCtrl.ResolveClientID)

	// Membership writes. Callers read their authorization answers from /authz
	// above, so this is where the grant that produces those answers has to
	// land — otherwise a caller writes a membership into its own table, asks
	// here whether that member may act, and is told no.
	v1.Put("/tenants/:tenantId/members/:wallet", membersCtrl.PutMember)
	v1.Delete("/tenants/:tenantId/members/:wallet", membersCtrl.DeleteMember)

	// On-behalf provisioning and the token minter. Registered before the
	// parameterised member routes matter-of-factly — fiber matches "provision"
	// as a :wallet value otherwise, and PUT/POST differ so they cannot collide
	// today, but the explicit route must exist before anyone adds a POST to
	// the parameterised path.
	v1.Post("/tenants/:tenantId/members/provision", provisionCtrl.Provision)
	v1.Get("/tenants/:tenantId/dimo-token", provisionCtrl.GetDimoToken)

	// The operator console's tenant surface, reached by b2b through kaufmann.
	// Creating is nested under the operator so the parent is in the path where
	// the scope check can see it.
	v1.Get("/operators/:operatorId/children", tenantsCtrl.ListChildren)
	v1.Post("/operators/:operatorId/customers", tenantsCtrl.CreateCustomer)
	v1.Get("/tenants/:tenantId", tenantsCtrl.GetTenant)
	v1.Patch("/tenants/:tenantId", tenantsCtrl.UpdateTenant)
	v1.Get("/tenants/:tenantId/members", tenantsCtrl.ListMembers)

	// Which vehicles a customer may see. This is the isolation boundary: under
	// D2/D5 it is not enforced by the chain, so these rows are what keeps one
	// customer's telemetry away from another.
	v1.Get("/tenants/:tenantId/vehicles", entitlementsCtrl.ListVehicles)
	v1.Post("/tenants/:tenantId/vehicles", entitlementsCtrl.AssignVehicles)
	v1.Delete("/tenants/:tenantId/vehicles/:tokenId", entitlementsCtrl.RevokeVehicle)

	// Fleet groups — P1 of the groups move (docs/plans/01-groups-into-tenancy.md):
	// endpoints served, no caller yet. Both apps still own their local copies;
	// P3's backfill and flagged reads are what start pointing them here.
	v1.Get("/tenants/:tenantId/groups", groupsCtrl.ListGroups)
	v1.Post("/tenants/:tenantId/groups", groupsCtrl.CreateGroup)
	v1.Patch("/tenants/:tenantId/groups/:groupId", groupsCtrl.UpdateGroup)
	v1.Delete("/tenants/:tenantId/groups/:groupId", groupsCtrl.DeleteGroup)
	v1.Get("/tenants/:tenantId/groups/:groupId/vehicles", groupsCtrl.ListGroupVehicles)
	v1.Post("/tenants/:tenantId/groups/:groupId/vehicles", groupsCtrl.AddGroupVehicles)
	v1.Delete("/tenants/:tenantId/groups/:groupId/vehicles/:tokenId", groupsCtrl.RemoveGroupVehicle)

	return app
}

func healthCheck(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "up"})
}

func getVersion(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"commit": appCommitHash})
}

// ErrorHandler logs the recovered error and returns JSON rather than a string.
//
// Client errors log at warn, server errors at error. The level is the whole
// point: every rejected /v1 call is a 401 or 403, and this service rejects by
// design — an unauthenticated probe, a caller whose key is stale, a tenant
// asking about one it may not see. Logging those at error level makes routine
// enforcement indistinguishable from the service being broken, and feeds any
// error-rate alerting built on this stream.
//
// A rejection is still recorded, at a level that says "this happened" rather
// than "something is wrong". The security-relevant detail is logged separately
// and deliberately by the layer that made the decision — see
// NewTrustedCallerGuard and CallerMayAccess.
func ErrorHandler(c *fiber.Ctx, err error, logger *zerolog.Logger) error {
	code := fiber.StatusInternalServerError
	var e *fiber.Error
	if errors.As(err, &e) {
		code = e.Code
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	// 404 stays silent: an unrouted path is neither a fault nor worth a line
	// per scan.
	if code != fiber.StatusNotFound {
		ev := logger.Warn()
		if code >= fiber.StatusInternalServerError {
			ev = logger.Error()
		}
		ev.Err(err).
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
