package app

import (
	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/controllers"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/gofiber/fiber/v2"
	fiberrecover "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/rs/zerolog"
)

// WebhookApp is the ONLY publicly reachable surface this service has, and it
// is a separate Fiber app on a separate port for exactly that reason.
//
// Postmark posts delivery, open and bounce events for invitation emails from
// the public internet, so something has to be reachable. Everything else here
// — /v1, the token minter, provisioning — is cluster-internal by design, and
// the chart's ingress therefore targets THIS listener's port and nothing else.
//
// WHY A SECOND LISTENER RATHER THAN A PATH-RESTRICTED INGRESS ON THE MAIN ONE.
// Both would work today. The difference is what happens when someone later
// edits the ingress: a path rule widened by accident, a wildcard added, a
// second host block copied from another chart. Against the shared listener
// that mistake exposes /v1 to the internet; against this one it exposes
// nothing, because /v1 is not registered here. It is the difference between
// "internal because the config says so" and "internal because the process
// cannot serve it", and this programme has been bitten enough times by the
// former (the empty encryption key, the missing ExternalSecret ref) to prefer
// the latter.
//
// Two consequences worth keeping true:
//   - Never register a /v1 route here, and never add this port to a Service
//     that something internal already reaches on the main port.
//   - The handler still authenticates. The listener split is defence in depth,
//     not the defence: POST /webhooks/postmark checks basic auth against
//     POSTMARK_WEBHOOK_SECRET in constant time, and an unset secret disables
//     the endpoint rather than opening it.
func WebhookApp(settings *config.Settings, logger *zerolog.Logger, pdb *db.Store) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			return ErrorHandler(c, err, logger)
		},
		DisableStartupMessage: true,
	})
	app.Use(fiberrecover.New(fiberrecover.Config{EnableStackTrace: true}))

	// The ingress needs something to health-check that is not the webhook
	// itself — a probe must not have to present the webhook credential, and
	// must not be counted as a malformed event.
	app.Get("/health", healthCheck)

	// Fully constructed even though this app only ever calls ApplyEmailEvent:
	// a service built with a nil sender is a panic waiting for whoever adds
	// the next route here, and a Postmark client costs nothing to hold.
	invitationSvc := service.NewInvitationService(logger, pdb, settings,
		gateway.NewPostmarkAPI(*logger, settings.PostmarkServerToken))
	webhooksCtrl := controllers.NewWebhooksController(logger, settings.PostmarkWebhookSecret, invitationSvc)
	app.Post("/webhooks/postmark", webhooksCtrl.HandlePostmark)

	if settings.PostmarkWebhookSecret == "" {
		// Worth a line at boot: the endpoint answers 403 to everything in this
		// state, which looks identical to Postmark being misconfigured.
		logger.Warn().Msg("POSTMARK_WEBHOOK_SECRET is empty — /webhooks/postmark refuses every request")
	}
	return app
}
