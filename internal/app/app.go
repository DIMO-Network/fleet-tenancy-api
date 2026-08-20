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
	"github.com/DIMO-Network/fleet-tenancy-api/internal/sharing"
	"github.com/DIMO-Network/shared/pkg/db"
	jwtware "github.com/gofiber/contrib/jwt"
	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/rs/zerolog"
)

var appCommitHash string

// App builds the /v1 surface.
//
// shareQueue may be nil: vehicle sharing is off in environments without the
// SACD and bundler settings, and this service must serve /v1/authz regardless —
// both apps fail closed on it.
func App(settings *config.Settings, logger *zerolog.Logger, commitHash string, pdb *db.Store,
	shareQueue *sharing.Queue) *fiber.App {
	appCommitHash = commitHash

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return ErrorHandler(c, err, logger)
		},
		DisableStartupMessage: true,
	})
	app.Use(fiberrecover.New(fiberrecover.Config{EnableStackTrace: true}))
	// Registered before every route, including /health, so a probe that starts
	// failing is visible in the same place as everything else. See metrics.go
	// for why the label is the route pattern and never the URL.
	app.Use(NewMetricsMiddleware())

	app.Get("/health", healthCheck)
	app.Get("/version", getVersion)

	tenantSvc := service.NewTenantService(logger, pdb)
	memberSvc := service.NewMemberService(logger, pdb)
	authzSvc := service.NewAuthzService(logger, pdb)
	authzCtrl := controllers.NewAuthzController(logger, authzSvc, tenantSvc, CallerFrom)
	resolveCtrl := controllers.NewResolveController(logger, tenantSvc, CallerFrom)
	membersCtrl := controllers.NewMembersController(logger, memberSvc, tenantSvc, CallerFrom)
	entitlementsCtrl := controllers.NewEntitlementsController(logger,
		service.NewEntitlementService(logger, pdb), tenantSvc, CallerFrom)
	membershipsCtrl := controllers.NewMembershipsController(logger,
		service.NewMembershipService(logger, pdb), tenantSvc, CallerFrom)

	// The effective-credential surface: the token minter and on-behalf
	// provisioning. The credential service is the only code that decrypts a
	// tenant key at runtime; everything else moves tokens, not keys.
	credSvc := service.NewCredentialService(logger, pdb, settings,
		gateway.NewIdentityAPIService(logger, settings.IdentityAPIEndpoint))
	// Self-serve creation and credential writes share the credential service
	// so validation warms the exact minter the stored credential will use.
	selfServeSvc := service.NewSelfServeService(logger, pdb, settings, tenantSvc, credSvc)
	tenantsCtrl := controllers.NewTenantsController(logger, tenantSvc, selfServeSvc, CallerFrom)
	provisionSvc := service.NewProvisionService(logger, pdb, memberSvc, credSvc,
		gateway.NewAccountsAPIService(logger, settings.AccountsAPIEndpoint))
	// The one email this service sends: "you've been given access", on
	// provisioning. Unconfigured (no Postmark token) it is a no-op and every
	// provision reports emailSent=false.
	provisionSvc.UseAccessEmail(service.NewAccessEmailService(logger, settings))
	provisionCtrl := controllers.NewProvisionController(logger, provisionSvc, credSvc, tenantSvc, CallerFrom)
	groupsCtrl := controllers.NewGroupsController(logger, service.NewGroupService(logger, pdb), tenantSvc, CallerFrom)

	// The minted-vehicle roster (docs/plans/07-vehicle-roster.md, step 4). The
	// same service the reconcile command drives, here serving reads from the
	// table it maintains; the identity client it carries is the writer's, and
	// no read path touches it.
	rosterCtrl := controllers.NewRosterController(logger,
		service.NewRosterService(logger, pdb,
			gateway.NewIdentityAPIService(logger, settings.IdentityAPIEndpoint)),
		tenantSvc, CallerFrom)

	// Vehicle sharing (docs/HANDOFF.md, "Vehicle sharing").
	//
	// The signer gate asks accounts-api live rather than reading
	// users.shared_account_signer_address, which is empty for every owner whose
	// account kaufmann-oracle created — see SharedSignerService.
	//
	// shareQueue is nil when sharing is unconfigured. The routes are registered
	// either way: an unconfigured environment answers 503, which tells the
	// caller the feature is off, where a 404 would look like a version skew.
	sharedSignerSvc := service.NewSharedSignerService(logger,
		gateway.NewAccountsAPIService(logger, settings.AccountsAPIEndpoint), credSvc)
	shareAuthorizer := service.NewShareAuthorizer(logger, pdb,
		gateway.NewIdentityAPIService(logger, settings.IdentityAPIEndpoint),
		sharedSignerSvc, credSvc, settings)
	sharingCtrl := controllers.NewSharingController(logger, sharedSignerSvc, authzSvc,
		shareAuthorizer, shareQueue, tenantSvc, CallerFrom)

	// Email invitations (docs/plans/04-invitations-into-tenancy.md, P1): the
	// records and the dispatch both live here so the plaintext token exists in
	// exactly one service's memory. Unconfigured Postmark means invitations
	// are recorded and report emailSent=false, like the provisioning email.
	//
	// Postmark's delivery webhook is deliberately NOT registered on this app:
	// it is the one surface that must be publicly reachable, so it lives on
	// its own listener and its own port. See WebhookApp.
	invitationSvc := service.NewInvitationService(logger, pdb, settings,
		gateway.NewPostmarkAPI(*logger, settings.PostmarkServerToken))
	invitationsCtrl := controllers.NewInvitationsController(logger, invitationSvc, tenantSvc, CallerFrom)

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

	// The one read with no tenant in the path: which tenants a wallet belongs
	// to. This is what fleet-lite asks at login, before it has a Tenant-Id to
	// send. Scope is enforced inside the query — each row must pass the same
	// expression CallerMayAccess runs — rather than by assertScope, which needs
	// a single subject.
	v1.Get("/tenants", tenantsCtrl.ListWalletTenants)

	// Membership writes. Callers read their authorization answers from /authz
	// above, so this is where the grant that produces those answers has to
	// land — otherwise a caller writes a membership into its own table, asks
	// here whether that member may act, and is told no.
	v1.Put("/tenants/:tenantId/members/:wallet", membersCtrl.PutMember)
	v1.Delete("/tenants/:tenantId/members/:wallet", membersCtrl.DeleteMember)
	// Login telemetry: last_login_at drives the callers' sync tiering, and the
	// membership row here is the only one a managed tenant's member has.
	v1.Post("/tenants/:tenantId/members/:wallet/login", membersCtrl.LoginTouch)

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
	// Self-serve creation, the last un-cutover write: fleet-lite's POST
	// /tenants writes here FIRST and materialises its local row under the id
	// this service mints. Service callers only — an unparented tenant is in
	// nobody's scope, so the ordinary caller rule has nothing to check.
	v1.Post("/tenants", tenantsCtrl.CreateSelfServeTenant)
	v1.Get("/tenants/:tenantId", tenantsCtrl.GetTenant)
	v1.Patch("/tenants/:tenantId", tenantsCtrl.UpdateTenant)
	// A tenant's own license: set at self-serve creation, replaced from
	// fleet-lite's Settings, or granted to a managed customer as its
	// graduation path — at which point effective-credential resolution stops
	// falling through to its operator, with no other change anywhere.
	v1.Put("/tenants/:tenantId/credentials", tenantsCtrl.SetCredentials)
	v1.Get("/tenants/:tenantId/members", tenantsCtrl.ListMembers)

	// Which vehicles a customer may see. This is the isolation boundary: under
	// D2/D5 it is not enforced by the chain, so these rows are what keeps one
	// customer's telemetry away from another.
	v1.Get("/tenants/:tenantId/vehicles", entitlementsCtrl.ListVehicles)
	v1.Post("/tenants/:tenantId/vehicles", entitlementsCtrl.AssignVehicles)
	v1.Delete("/tenants/:tenantId/vehicles/:tokenId", entitlementsCtrl.RevokeVehicle)

	// Vehicle memberships — the commercial record, one per vehicle
	// (docs/plans/02-vehicle-memberships.md). Distinct from the entitlements
	// above: those decide whether a customer may SEE a vehicle, these decide
	// whether it is PAID FOR. Named vehicle-memberships on the wire because
	// "memberships" already means users-in-tenants throughout this service.
	//
	// Move and renew are actions rather than a PATCH: they validate
	// differently, and a tri-state update body is the shape that has bitten
	// this service before.
	v1.Get("/tenants/:tenantId/vehicle-memberships", membershipsCtrl.ListMemberships)
	// The gate read, separate from the list above: fleet-lite intersects its
	// vehicle queries with this on every request, and needs only the flag and
	// a set of ints — not every row the console renders.
	v1.Get("/tenants/:tenantId/active-vehicle-memberships", membershipsCtrl.ActiveMemberships)
	v1.Post("/tenants/:tenantId/vehicle-memberships", membershipsCtrl.CreateMembership)
	v1.Post("/tenants/:tenantId/vehicle-memberships/:membershipId/move", membershipsCtrl.MoveMembership)
	v1.Post("/tenants/:tenantId/vehicle-memberships/:membershipId/renew", membershipsCtrl.RenewMembership)
	v1.Delete("/tenants/:tenantId/vehicle-memberships/:membershipId", membershipsCtrl.CancelMembership)

	// Fleet groups — P1 of the groups move (docs/plans/01-groups-into-tenancy.md):
	// endpoints served, no caller yet. Both apps still own their local copies;
	// P3's backfill and flagged reads are what start pointing them here.
	// Email invitations — P1 of the invitations move: surface served, no
	// caller yet. CRUD is tenant-scoped like everything else; the calling app
	// owns the human's manage_members check (the BFF split), this service
	// checks the caller tenant's scope.
	v1.Get("/tenants/:tenantId/invitations", invitationsCtrl.List)
	v1.Post("/tenants/:tenantId/invitations", invitationsCtrl.Create)
	v1.Delete("/tenants/:tenantId/invitations/:invitationId", invitationsCtrl.Revoke)
	// Resend is an action, not a PATCH: it mints a fresh token and the old
	// link dies — a side effect no field update should imply.
	v1.Post("/tenants/:tenantId/invitations/:invitationId/resend", invitationsCtrl.Resend)
	// The one write with no tenant in the path: the token resolves it. The
	// token authorizes, the trusted caller asserts the wallet, and caller
	// scope is checked against the resolved tenant inside the service.
	v1.Post("/invitations/accept", invitationsCtrl.Accept)

	// Which of the caller's vehicle owners this tenant may sign for — the
	// per-vehicle share button's gate. A POST because the input is a list whose
	// length is the caller's fleet, not a query parameter; nothing is written.
	v1.Post("/tenants/:tenantId/shareable-owners", sharingCtrl.ShareableOwners)
	// The share itself: 202 and a job id, because it waits on a bundler for
	// longer than an HTTP request should. Status is a sibling GET rather than a
	// path under the job id — the tenant scope check needs the tenant, and the
	// job id alone is a sequential integer anyone could walk.
	v1.Post("/tenants/:tenantId/vehicles/:tokenId/share", sharingCtrl.ShareVehicle)
	v1.Get("/tenants/:tenantId/vehicles/:tokenId/share/status", sharingCtrl.ShareStatus)

	// What the vehicles in a resolved set ARE — owner, definition, VIN, plate —
	// read from the roster this service reconciles against the chain nightly.
	//
	// The set is NOT resolved here: the caller has already intersected
	// entitlements, active memberships and group scope, all three answered
	// above, and this is the metadata join over the result. Registered before
	// the parameterised /vehicles/:tokenId routes for the same reason the
	// provision route is: nothing collides today, but "vehicle-metadata" as a
	// sibling of "vehicles" keeps it that way.
	v1.Post("/tenants/:tenantId/vehicle-metadata", rosterCtrl.VehicleMetadata)

	v1.Get("/tenants/:tenantId/groups", groupsCtrl.ListGroups)
	v1.Get("/tenants/:tenantId/vehicle-groups", groupsCtrl.ListVehicleGroups)
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
