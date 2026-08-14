package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/google/subcommands"
	"github.com/rs/zerolog"
)

// pushPostmarkTemplatesCmd syncs the repo-stored Postmark templates to the
// Postmark server the configured token belongs to. Ported from fleet-lite-app,
// which established the pattern: templates live in the repo as the source of
// truth and are pushed by alias, so "the template exists in Postmark" is a
// rerunnable command rather than someone's memory of clicking in a UI.
//
// Run it once per environment before the first provision expects to email:
//
//	POSTMARK_SERVER_TOKEN=... ./fleet-tenancy-api push-postmark-templates
type pushPostmarkTemplatesCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	dir string
}

func (*pushPostmarkTemplatesCmd) Name() string { return "push-postmark-templates" }
func (*pushPostmarkTemplatesCmd) Synopsis() string {
	return "upsert the repo's Postmark templates to the configured Postmark server"
}
func (*pushPostmarkTemplatesCmd) Usage() string {
	return `push-postmark-templates [-dir templates/postmark]:
	Reads manifest.json + body files and upserts each template to Postmark by alias.
  `
}

func (p *pushPostmarkTemplatesCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.dir, "dir", "templates/postmark", "directory holding manifest.json + template bodies")
}

type templateManifest struct {
	Templates []struct {
		Alias    string `json:"alias"`
		Name     string `json:"name"`
		Subject  string `json:"subject"`
		HTMLFile string `json:"htmlFile"`
		TextFile string `json:"textFile"`
	} `json:"templates"`
}

func (p *pushPostmarkTemplatesCmd) Execute(_ context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	postmark := gateway.NewPostmarkAPI(p.logger, p.settings.PostmarkServerToken)
	if !postmark.Enabled() {
		p.logger.Error().Msg("POSTMARK_SERVER_TOKEN is not set; cannot push templates")
		return subcommands.ExitFailure
	}

	raw, err := os.ReadFile(filepath.Join(p.dir, "manifest.json"))
	if err != nil {
		p.logger.Error().Err(err).Str("dir", p.dir).Msg("read manifest")
		return subcommands.ExitFailure
	}
	var manifest templateManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		p.logger.Error().Err(err).Msg("parse manifest")
		return subcommands.ExitFailure
	}
	if len(manifest.Templates) == 0 {
		p.logger.Error().Msg("manifest has no templates")
		return subcommands.ExitFailure
	}

	for _, t := range manifest.Templates {
		htmlBody, err := os.ReadFile(filepath.Join(p.dir, t.HTMLFile))
		if err != nil {
			p.logger.Error().Err(err).Str("alias", t.Alias).Msg("read html body")
			return subcommands.ExitFailure
		}
		var textBody []byte
		if t.TextFile != "" {
			if textBody, err = os.ReadFile(filepath.Join(p.dir, t.TextFile)); err != nil {
				p.logger.Error().Err(err).Str("alias", t.Alias).Msg("read text body")
				return subcommands.ExitFailure
			}
		}
		if err := postmark.UpsertTemplate(t.Alias, t.Name, t.Subject, string(htmlBody), string(textBody)); err != nil {
			p.logger.Error().Err(err).Str("alias", t.Alias).Msg("upsert template")
			return subcommands.ExitFailure
		}
		p.logger.Info().Str("alias", t.Alias).Msg("template pushed")
	}
	return subcommands.ExitSuccess
}
