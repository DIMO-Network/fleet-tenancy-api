package main

import (
	"context"
	"flag"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/google/subcommands"
	"github.com/rs/zerolog"
)

// warmSharedAccountsCmd resolves vehicle owners' shared accounts ahead of any
// page render, so a large fleet does not have to converge one budget at a time.
//
// The display gate (SharedSignerService.FilterSignable) resolves owners nothing
// is known about inside the request, 8-wide under a 3-second budget, because
// the caller gives up at five seconds. Accounts-api answers a wallet in roughly
// one to two and a half seconds, which caps a render at a couple of dozen
// owners however those two numbers are tuned. For the Kaufmann operator tenant
// — 162 distinct owners — that is a fleet that needs the better part of a dozen
// renders before its share buttons all appear, and until 2026-08-22 it was
// worse than that: the answers were being resolved and then discarded, so the
// fleet never converged at all (see SharedSignerService.record).
//
// The persistence bug is fixed, but the convergence shape is still wrong for a
// batch of that size. Resolving a fleet's owners is a batch job that happened
// to be living inside a page render. This is that job.
//
// Cheap to re-run and safe to run repeatedly: it skips every owner already
// known on exactly the freshness rule the request path uses, so a second run
// asks accounts-api only about owners that appeared since, or whose "no shared
// account" answer has aged past negativeRecheckAfter. A positive is never
// re-asked at all — providedSignerAddress cannot be revoked
// (docs/signer-permanence.md).
type warmSharedAccountsCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	tenantID    string
	limit       int
	concurrency int
	dryRun      bool
}

func (*warmSharedAccountsCmd) Name() string { return "warm-shared-accounts" }
func (*warmSharedAccountsCmd) Synopsis() string {
	return "resolve and record vehicle owners' shared accounts ahead of a page render"
}
func (*warmSharedAccountsCmd) Usage() string {
	return `warm-shared-accounts -tenant <uuid> [-limit N] [-concurrency N] [-dry-run]:
	Asks accounts-api about every vehicle owner this service knows of that has
	no fresh answer in shared_accounts, and records what it hears. Run it once
	after a fleet is onboarded or reconciled; the display gate then answers from
	one indexed query instead of a few hundred HTTP calls spread over a dozen
	renders.

	-tenant names the tenant whose developer JWT authenticates the lookups. It
	is required and it is ONLY that: a by-wallet lookup is not scoped to the
	caller, so one tenant's credential warms every owner in the table.

	The owner set is deliberately NOT scoped to that tenant's fleet. Resolving
	"this tenant's vehicles" means the entitlement rules, and those differ by
	entitlement_mode — explicit tenants have vehicle_entitlements rows, operator
	and self-serve tenants resolve their fleet from the license's privileged set
	and have none. Scoping by entitlement would therefore warm nothing at all
	for exactly the operator tenants this command exists for. The distinct
	owners in ` + "`vehicles`" + ` are the superset the gate can be asked about, which is
	what makes a completed run mean something.

	-limit bounds one run for a first pass or a smoke test. It caps the owners
	still needing an answer, not the owners read, so successive limited runs
	walk forward instead of re-reading the same head of the list — each run's
	answers are recorded, and recorded owners drop out of the next run's set.

	Exits non-zero if any lookup failed against accounts-api, but only after
	walking everything — like every batch here, a run that aborted on the first
	failure would verify almost nothing. Answers obtained before a failure are
	still recorded.
`
}

func (p *warmSharedAccountsCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.tenantID, "tenant", "", "tenant uuid whose developer JWT authenticates the lookups")
	f.IntVar(&p.limit, "limit", 0, "resolve at most N owners this run (0 = no limit)")
	f.IntVar(&p.concurrency, "concurrency", 0, "simultaneous accounts-api lookups (0 = default)")
	f.BoolVar(&p.dryRun, "dry-run", false, "report how many owners are cold; ask accounts-api nothing")
}

func (p *warmSharedAccountsCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if p.tenantID == "" {
		p.logger.Error().Msg("-tenant is required: the lookups need a developer JWT to authenticate with")
		return subcommands.ExitUsageError
	}

	store := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	store.WaitForDB(p.logger)

	owners, err := p.readOwners(ctx, &store)
	if err != nil {
		p.logger.Err(err).Msg("read vehicle owners")
		return subcommands.ExitFailure
	}
	if len(owners) == 0 {
		p.logger.Info().Msg("no vehicle owners on record — nothing to warm")
		return subcommands.ExitSuccess
	}

	l := p.logger
	creds := service.NewCredentialService(&l, &store, &p.settings,
		gateway.NewIdentityAPIService(&l, p.settings.IdentityAPIEndpoint))
	signer := service.NewSharedSignerService(&l,
		gateway.NewAccountsAPIService(&l, p.settings.AccountsAPIEndpoint), creds,
		service.NewSharedAccountStore(&store), &p.settings)

	// The cold set is computed before the limit is applied, not after — and
	// that ordering is the whole reason -limit is usable more than once. Taking
	// the first N owners in SQL would take the SAME N on every run, so a second
	// limited run would find them all warm and do nothing. Truncating the cold
	// set instead means each run consumes N owners that still need asking about
	// and the next one picks up where it left off, with no cursor to keep.
	cold, err := signer.ColdOwners(ctx, owners)
	if err != nil {
		p.logger.Err(err).Msg("read what is already known")
		return subcommands.ExitFailure
	}
	if p.limit > 0 && len(cold) > p.limit {
		cold = cold[:p.limit]
	}

	if p.dryRun {
		p.logger.Info().Int("owners", len(owners)).Int("cold", len(cold)).
			Msg("dry run — would ask accounts-api about the cold owners and record the answers")
		return subcommands.ExitSuccess
	}
	if len(cold) == 0 {
		p.logger.Info().Int("owners", len(owners)).
			Msg("every known owner already has a fresh answer — nothing to warm")
		return subcommands.ExitSuccess
	}

	started := time.Now()
	res, err := signer.Warm(ctx, p.tenantID, cold, p.concurrency)
	log := p.logger.With().Str("tenant_id", p.tenantID).
		Int("owners", len(owners)).Int("cold", res.Cold).
		Int("positive", res.Positive).Int("negative", res.Negative).
		Int("failed", res.Failed).Dur("took", time.Since(started)).Logger()
	if err != nil {
		// Whatever resolved before the failure is already in the table; each
		// answer is recorded as it arrives, not at the end of the batch.
		log.Err(err).Msg("warm shared accounts — accounts-api failed; what was resolved first is recorded")
		return subcommands.ExitFailure
	}
	log.Info().Msg("warm shared accounts complete")
	if res.Failed > 0 {
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

// readOwners returns every distinct vehicle owner on record, in a stable order.
//
// No join to shared_accounts and no LIMIT: the freshness rule lives in
// SharedSignerService.ColdOwners, which is the same one the request path
// applies, and a second copy of "a positive is permanent, a negative ages out"
// in a SQL predicate here would be free to drift from it. -limit is applied to
// what comes back cold, for the reason given at the call site.
func (p *warmSharedAccountsCmd) readOwners(ctx context.Context, store *db.Store) ([]string, error) {
	rows, err := store.DBS().Reader.QueryContext(ctx, `
		SELECT DISTINCT owner FROM vehicles
		 WHERE owner IS NOT NULL AND owner <> ''
		 ORDER BY owner`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}
