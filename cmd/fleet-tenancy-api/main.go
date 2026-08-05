package main

import (
	"os"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/app"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/rs/zerolog"
)

var commitHash string

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().
		Str("app", "fleet-tenancy-api").Logger()

	settings := &config.Settings{
		Environment: "local",
		APIPort:     "8080",
	}

	// Refuse to start rather than silently encrypt with a known key.
	if err := settings.Validate(); err != nil {
		logger.Fatal().Err(err).Msg("invalid settings")
	}

	server := app.App(settings, &logger, commitHash)
	if err := server.Listen(":" + settings.APIPort); err != nil {
		logger.Fatal().Err(err).Msg("server failed")
	}
}
