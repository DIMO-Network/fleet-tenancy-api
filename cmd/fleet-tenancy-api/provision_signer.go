package main

import (
	"context"
	"errors"
	"flag"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/google/subcommands"
	"github.com/rs/zerolog"
)

// provisionSignerCmd is plan 06 step 2: give ONE named tenant a signer it does
// not have. Deliberately not a backfill over every tenant missing one, unlike
// the kaufmann command it ports from — in this service a missing signer is
// usually a DECISION: self-serve tenants have none, which is what keeps
// sharing off for them, and that rule is not to be patched quietly.
type provisionSignerCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	tenantID      string
	forceCustomer bool
	dryRun        bool
}

func (*provisionSignerCmd) Name() string { return "provision-signer" }
func (*provisionSignerCmd) Synopsis() string {
	return "create a signer for one named tenant that has none (create-if-absent, never overwrite)"
}
func (*provisionSignerCmd) Usage() string {
	return `provision-signer -tenant <uuid> [-force-customer] [-dry-run]:
	Generates a keypair, encrypts the private key under TENANT_SECRET_ENC_KEY
	and writes it to the tenant's credential row — only if no signer exists.
	The condition is in the SQL, not in Go, so two racing runs cannot both
	write. A tenant that already has a signer is refused, not overwritten:
	the existing key may be registered as kernel-account validators, and
	replacing it orphans those accounts silently. There is no rotate — see
	docs/signer-permanence.md.

	-force-customer is required for kind=customer tenants. Giving a self-serve
	customer a signer TURNS SHARING ON for its fleet, which is a product
	decision, not an ops task. Managed customers inherit the operator's signer
	and normally need none of their own.
`
}

func (p *provisionSignerCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.tenantID, "tenant", "", "tenant uuid to provision")
	f.BoolVar(&p.forceCustomer, "force-customer", false, "allow provisioning a kind=customer tenant")
	f.BoolVar(&p.dryRun, "dry-run", false, "report what would happen; write nothing")
}

func (p *provisionSignerCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	if p.tenantID == "" {
		p.logger.Error().Msg("-tenant is required")
		return subcommands.ExitUsageError
	}
	if p.settings.TenantSecretEncKey == "" {
		p.logger.Error().Msg("TENANT_SECRET_ENC_KEY is empty — refusing to encrypt under sha256(\"\")")
		return subcommands.ExitFailure
	}

	store := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	store.WaitForDB(p.logger)

	var kind, name string
	var hasSigner bool
	err := store.DBS().Reader.QueryRowContext(ctx, `
		SELECT t.kind, t.name,
		       COALESCE(c.signer_key_enc, '') <> ''
		  FROM tenants t
		  LEFT JOIN tenant_credentials c ON c.tenant_id = t.id
		 WHERE t.id = $1::uuid`, p.tenantID).Scan(&kind, &name, &hasSigner)
	if err != nil {
		p.logger.Err(err).Str("tenant_id", p.tenantID).Msg("load tenant")
		return subcommands.ExitFailure
	}
	if hasSigner {
		p.logger.Error().Str("tenant", name).Msg("tenant already has a signer — refusing; there is no overwrite and no rotate")
		return subcommands.ExitFailure
	}
	if kind == "customer" && !p.forceCustomer {
		p.logger.Error().Str("tenant", name).
			Msg("kind=customer needs -force-customer: a self-serve customer gaining a signer gains SHARING, which is a product decision")
		return subcommands.ExitFailure
	}
	if p.dryRun {
		p.logger.Info().Str("tenant", name).Str("kind", kind).Msg("dry run — would provision a signer")
		return subcommands.ExitSuccess
	}

	l := p.logger
	creds := service.NewCredentialService(&l, &store, &p.settings, nil)
	res, err := creds.ProvisionSigner(ctx, p.tenantID)
	if err != nil {
		if errors.Is(err, service.ErrSignerExists) {
			// Lost the race to another run — which is the guard working.
			p.logger.Error().Str("tenant", name).Msg("a signer appeared between check and write; nothing overwritten")
			return subcommands.ExitFailure
		}
		p.logger.Err(err).Str("tenant", name).Msg("provision signer")
		return subcommands.ExitFailure
	}
	if res.EffectiveUnused {
		p.logger.Warn().Str("tenant", name).
			Msg("row holds no dimo_client_id, so the effective credential will not serve this signer until one exists")
	}
	p.logger.Info().Str("tenant", name).Str("signer_address", res.SignerAddress).Msg("signer provisioned")
	return subcommands.ExitSuccess
}
