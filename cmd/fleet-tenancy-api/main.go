package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/app"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
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
		flag.Parse()
		os.Exit(int(subcommands.Execute(ctx)))
	}

	port := settings.APIPort
	if port == 0 {
		port = 3010
	}
	pdb := db.NewDbConnectionFromSettings(ctx, &settings.DB, true)
	pdb.WaitForDB(logger)

	server := app.App(&settings, &logger, commitHash, &pdb)
	if lerr := server.Listen(":" + strconv.Itoa(port)); lerr != nil {
		logger.Fatal().Err(lerr).Msg("server failed")
	}
}
