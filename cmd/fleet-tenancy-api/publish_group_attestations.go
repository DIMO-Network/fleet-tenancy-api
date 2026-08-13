package main

import (
	"context"
	"flag"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/google/subcommands"
	"github.com/rs/zerolog"
)

// publishGroupAttestationsCmd runs one publisher scan — P4's single publisher
// of dimo.document.vehicle.groups, run as a CronJob rather than in the API
// process (the plan's R5 note: heavy background work must not share the fate
// of /v1/authz).
//
// The pod must be meshed: minting a developer JWT resolves the redirect URI
// through identity-api, which 403s unmeshed callers. The chart's CronJob
// wrapper owns the linkerd proxy shutdown, or the job never terminates.
type publishGroupAttestationsCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	tenantID string
	dryRun   bool
}

func (*publishGroupAttestationsCmd) Name() string { return "publish-group-attestations" }
func (*publishGroupAttestationsCmd) Synopsis() string {
	return "publish vehicle group attestations for every vehicle whose groups changed"
}
func (*publishGroupAttestationsCmd) Usage() string {
	return `publish-group-attestations [-tenant-id <uuid>] [-dry-run]:
	Scans fleet_groups / vehicle_fleet_groups against
	vehicle_group_attestation_state and publishes a
	dimo.document.vehicle.groups CloudEvent for every vehicle whose current
	group set (ids, names, colours) no longer matches what was last published.
	A vehicle removed from its last group gets an explicit empty-set
	retraction, exactly once.

	Scan-based on purpose: a per-write queue is what once coalesced a rename
	into completed jobs and published nothing (kaufmann-oracle#192). Each run
	converges on whatever is true now; a missed run is caught by the next.

	Events are signed with the tenant's effective developer license — its own,
	or its operator's for a managed customer. Tenants with no usable
	credential are skipped and counted; that is a configuration state, not a
	failure. Any delivery failure makes the run exit non-zero, because a
	partial publish is not a success.

	-dry-run reports what would be published and writes nothing.
  `
}

func (p *publishGroupAttestationsCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.tenantID, "tenant-id", "", "only this tenant")
	f.BoolVar(&p.dryRun, "dry-run", false, "report only; publish nothing")
}

func (p *publishGroupAttestationsCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if p.settings.AttestAPIURL.String() == "" && !p.dryRun {
		p.logger.Error().Msg("ATTEST_API_URL is not configured")
		return subcommands.ExitUsageError
	}
	if p.settings.ChainID == 0 {
		p.logger.Error().Msg("CHAIN_ID is not configured")
		return subcommands.ExitUsageError
	}

	store := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	store.WaitForDB(p.logger)

	credentials := service.NewCredentialService(&p.logger, &store, &p.settings,
		gateway.NewIdentityAPIService(&p.logger, p.settings.IdentityAPIEndpoint))
	attest := gateway.NewAttestAPIService(&p.logger, p.settings.AttestAPIURL.String())
	publisher := service.NewGroupPublisher(&p.logger, &store, &p.settings, credentials, attest)

	res, err := publisher.Run(ctx, p.tenantID, p.dryRun)
	if err != nil {
		p.logger.Err(err).Msg("publisher scan failed")
		return subcommands.ExitFailure
	}

	p.logger.Info().
		Int("checked", res.Checked).
		Int("published", res.Published).
		Int("retracted", res.Retracted).
		Int("unchanged", res.Unchanged).
		Int("failed", res.Failed).
		Int("skipped_tenants", res.SkippedTenants).
		Bool("dry_run", p.dryRun).
		Msg("group attestation publish complete")

	if res.Failed > 0 {
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}
