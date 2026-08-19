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
	"github.com/DIMO-Network/fleet-tenancy-api/internal/sharing"
	"github.com/DIMO-Network/shared/pkg/db"
	ssettings "github.com/DIMO-Network/shared/pkg/settings"
	"github.com/google/subcommands"
	_ "github.com/lib/pq"
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

	// The vehicle-sharing job queue (docs/plans/05-vehicle-sharing.md). Nil and
	// silent when unconfigured, which is every environment until the SACD and
	// bundler settings are in place — a service two apps fail closed on does not
	// get to refuse to boot over a feature neither of them calls yet.
	//
	// A queue that cannot be built when it IS configured is fatal, though: that
	// means the settings are present but wrong, and starting anyway would serve
	// share requests into a queue nothing drains.
	//
	// nil workers: the share worker lands in step 2. River will not start with
	// an empty bundle, so the queue stays unbuilt until there is something to
	// run rather than failing startup.
	shareQueue, qerr := sharing.NewQueue(ctx, &logger, &settings, nil)
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

	server := app.App(&settings, &logger, commitHash, &pdb)
	if lerr := server.Listen(":" + strconv.Itoa(port)); lerr != nil {
		logger.Fatal().Err(lerr).Msg("server failed")
	}
}
