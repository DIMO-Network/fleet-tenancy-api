package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/app"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/sharing"
	"github.com/DIMO-Network/shared/pkg/db"
	ssettings "github.com/DIMO-Network/shared/pkg/settings"
	"github.com/google/subcommands"
	_ "github.com/lib/pq"
	"github.com/riverqueue/river"
	"github.com/rs/zerolog"
)

var commitHash string

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().
		Str("app", "fleet-tenancy-api").Logger()

	settings, err := ssettings.LoadConfig[config.Settings]("settings.yaml")
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load settings")
	}

	if settings.LogLevel != "" {
		lvl, lerr := zerolog.ParseLevel(settings.LogLevel)
		if lerr != nil {
			logger.Fatal().Err(lerr).Msg("could not parse log level")
		}
		zerolog.SetGlobalLevel(lvl)
		logger = logger.Level(lvl)
	}

	// Refuse to start rather than silently encrypt credentials under sha256("").
	// Checked before subcommands so CLI paths are covered too.
	if verr := settings.Validate(); verr != nil {
		logger.Fatal().Err(verr).Msg("invalid settings")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 {
		subcommands.Register(subcommands.HelpCommand(), "")
		subcommands.Register(subcommands.FlagsCommand(), "")
		subcommands.Register(subcommands.CommandsCommand(), "")
		subcommands.Register(&migrateDBCmd{logger: logger, settings: settings}, "database")
		subcommands.Register(&backfillCmd{logger: logger, settings: settings}, "database")
		subcommands.Register(&backfillGroupsCmd{logger: logger, settings: settings}, "database")
		subcommands.Register(&backfillInvitationsCmd{logger: logger, settings: settings}, "database")
		subcommands.Register(&publishGroupAttestationsCmd{logger: logger, settings: settings}, "attestations")
		subcommands.Register(&pushPostmarkTemplatesCmd{logger: logger, settings: settings}, "email")
		flag.Parse()
		os.Exit(int(subcommands.Execute(ctx)))
	}

	port := settings.APIPort
	if port == 0 {
		port = 3010
	}
	pdb := db.NewDbConnectionFromSettings(ctx, &settings.DB, true)
	pdb.WaitForDB(logger)

	// The public webhook surface listens separately from /v1 — see
	// app.WebhookApp for why it is a second listener rather than a path rule.
	// A bind failure is fatal and immediate: Listen returns straight away when
	// the port cannot be taken, and a service that silently stopped receiving
	// Postmark's delivery events would look exactly like email working fine.
	webhookPort := settings.WebhookPort
	if webhookPort == 0 {
		webhookPort = 8087
	}
	webhooks := app.WebhookApp(&settings, &logger, &pdb)
	go func() {
		logger.Info().Int("port", webhookPort).Msg("starting webhook listener (public surface)")
		if lerr := webhooks.Listen(":" + strconv.Itoa(webhookPort)); lerr != nil {
			logger.Fatal().Err(lerr).Msg("webhook listener failed")
		}
	}()

	// The vehicle-sharing job queue (docs/HANDOFF.md, "Vehicle sharing"). Nil and
	// silent when unconfigured, which is every environment until the SACD and
	// bundler settings are in place — a service two apps fail closed on does not
	// get to refuse to boot over a feature neither of them calls yet.
	//
	// A queue that cannot be built when it IS configured is fatal, though: that
	// means the settings are present but wrong, and starting anyway would serve
	// share requests into a queue nothing drains.
	shareQueue, qerr := sharing.NewQueue(ctx, &logger, &settings, shareWorkers(ctx, &logger, &settings, &pdb))
	if qerr != nil {
		logger.Fatal().Err(qerr).Msg("failed to create vehicle-sharing job queue")
	}
	if serr := shareQueue.Start(ctx); serr != nil {
		logger.Fatal().Err(serr).Msg("failed to start vehicle-sharing job queue")
	}
	// Drained on SIGTERM from a goroutine rather than a defer. server.Listen
	// below blocks until the process is killed and logger.Fatal calls os.Exit,
	// so a deferred Stop would never run on any path that matters — the queue
	// would be hard-killed on every deploy with a UserOp possibly in flight.
	//
	// This is deliberately not wired to an HTTP-server shutdown: the API's
	// blocking Listen is left exactly as it was. Kubernetes cancels ctx at
	// SIGTERM and waits out terminationGracePeriodSeconds before SIGKILL, which
	// is the window this drain runs in.
	go func() {
		<-ctx.Done()
		// A fresh context, not the cancelled one: Stop waits for running jobs,
		// and passing an already-cancelled context would abandon a share
		// mid-flight — the grant lands on-chain with the job recorded as
		// failed. Bounded below the default 30s grace period so the drain
		// finishes before SIGKILL rather than being cut off by it.
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := shareQueue.Stop(stopCtx); err != nil {
			logger.Error().Err(err).Msg("vehicle-sharing job queue did not stop cleanly")
		}
	}()

	server := app.App(&settings, &logger, commitHash, &pdb, shareQueue)
	if lerr := server.Listen(":" + strconv.Itoa(port)); lerr != nil {
		logger.Fatal().Err(lerr).Msg("server failed")
	}
}

// shareWorkers builds the vehicle-sharing worker bundle, or nil when sharing is
// unconfigured — which is what keeps the queue unbuilt and the service booting
// normally in environments that have no bundler.
//
// The worker's dependencies are constructed here rather than being shared with
// app.App's, and the duplication is deliberate on two counts. It keeps App's
// signature to the queue alone; and the worker is the half the plan expects to
// move into its own deployment if bundler latency ever crowds the API, at which
// point it will need to build these anyway. The only shared state either copy
// holds is a cache, and a worker checking authorization against its own fresh
// cache is the behaviour we want at the moment it spends gas.
func shareWorkers(ctx context.Context, logger *zerolog.Logger, settings *config.Settings, pdb *db.Store) *river.Workers {
	if !settings.SharingConfigured() {
		return nil
	}

	fleetClient, err := sharing.NewFleetClient(settings)
	if err != nil {
		logger.Fatal().Err(err).Msg("sharing is configured but its fleet client could not be built")
	}

	credSvc := service.NewCredentialService(logger, pdb, settings,
		gateway.NewIdentityAPIService(logger, settings.IdentityAPIEndpoint))
	signerSvc := service.NewSharedSignerService(logger,
		gateway.NewAccountsAPIService(logger, settings.AccountsAPIEndpoint), credSvc)
	authorizer := service.NewShareAuthorizer(logger, pdb,
		gateway.NewIdentityAPIService(logger, settings.IdentityAPIEndpoint),
		signerSvc, credSvc, settings)

	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, sharing.NewShareWorker(logger, settings, authorizer, fleetClient)); err != nil {
		logger.Fatal().Err(err).Msg("failed to register the vehicle-share worker")
	}
	return workers
}
