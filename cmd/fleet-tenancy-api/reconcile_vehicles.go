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

// reconcileVehiclesCmd refreshes the vehicle roster from identity-api — plan 07
// step 3.
//
// It sweeps every developer licence this service holds, reads each licence's
// privileged vehicles, and brings the `vehicles` table into line with what the
// chain says. The owner column is the point: it is re-read here on a schedule
// rather than written by whoever performed a transfer, which is the difference
// between this table and kaufmann_oracle.vins — where three vehicles have been
// wrong since a transfer with nothing to notice.
//
// Run it on a CronJob. The first production run should be a -dry-run read by a
// human: it reports the owner contradictions it would correct, and those are
// diagnostic before they are routine.
type reconcileVehiclesCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	dryRun bool
}

func (*reconcileVehiclesCmd) Name() string { return "reconcile-vehicles" }
func (*reconcileVehiclesCmd) Synopsis() string {
	return "refresh the vehicle roster (owner, definition, minted_at) from identity-api"
}
func (*reconcileVehiclesCmd) Usage() string {
	return `reconcile-vehicles [-dry-run]:
	Sweep every developer licence held here, read its privileged vehicles from
	identity-api, and reconcile the vehicles table against them. Owner is
	re-read and compared on every run; a change is recorded in
	vehicle_owner_changes as well as applied.

	-dry-run computes and reports the whole sweep without writing. Use it for
	the first run against a populated environment.

	Exits non-zero if any licence could not be swept: a partial sweep must not
	be reported as a clean one, because the vehicles behind a failed licence
	are indistinguishable from vehicles nobody can see any more.
  `
}

func (p *reconcileVehiclesCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&p.dryRun, "dry-run", false, "report what would change without writing")
}

func (p *reconcileVehiclesCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	pdb := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	pdb.WaitForDB(p.logger)

	identity := gateway.NewIdentityAPIService(&p.logger, p.settings.IdentityAPIEndpoint)
	roster := service.NewRosterService(&p.logger, &pdb, identity)

	report, err := roster.Reconcile(ctx, p.dryRun)
	if err != nil {
		p.logger.Err(err).Msg("reconcile vehicles")
		return subcommands.ExitFailure
	}

	// Owner changes are logged one per line, at warn, whether or not this is a
	// dry run. They are the reason the table exists; a count in a summary line
	// is not something anybody reads, and the three that prompted this plan
	// were found by hand precisely because nothing ever said them out loud.
	for _, c := range report.OwnerChanges {
		p.logger.Warn().
			Int64("vehicle_token_id", c.TokenID).
			Str("previous_owner", c.Previous).
			Str("chain_owner", c.New).
			Bool("dry_run", p.dryRun).
			Msg("roster owner corrected from the chain")
	}

	ev := p.logger.Info()
	if len(report.LicensesFailed) > 0 {
		ev = p.logger.Error().Strs("licences_failed", report.LicensesFailed)
	}
	ev.Bool("dry_run", p.dryRun).
		Bool("first_run", report.FirstRun).
		Int("licences", report.LicensesSwept).
		Int("vehicles_seen", report.VehiclesSeen).
		Int("inserted", report.Inserted).
		Int("updated", report.Updated).
		Int("owner_changes", len(report.OwnerChanges)).
		Int("entitled_filled", report.EntitledFilled).
		Int("marked_unseen", report.MarkedUnseen).
		Msg("reconcile-vehicles complete")

	if len(report.LicensesFailed) > 0 {
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}
