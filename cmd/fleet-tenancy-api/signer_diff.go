package main

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"sort"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/service"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/subcommands"
	"github.com/rs/zerolog"
)

// signerDiffCmd is plan 06 step 1: prove that this service's copy of each
// tenant signer key and kaufmann's copy are the same key.
//
// Everything downstream of this assumes one logical key with two wrappers. If
// a tenant's copies have drifted — a signer regenerated on one side, a partial
// backfill, a master key changed — consolidation would pick a winner silently,
// and the losing key is the one some kernel accounts registered as their
// validator. Those vehicles become permanently unsignable with no error naming
// the cause. Nothing else in plan 06 starts until this reports differ=0 and no
// unexplained missing.
//
// ADDRESSES ONLY. Both copies are decrypted under their own master keys, the
// public address is derived from each, and only the addresses are compared and
// logged. Key material never appears in output, in errors, or in a comparison
// that could leak it — GCM authenticates, so a failed decrypt names the tenant
// and nothing else.
//
// Like every diff here, it walks EVERY tenant before reporting: a diagnostic
// that aborts on the first failure verifies almost nothing.
type signerDiffCmd struct {
	logger   zerolog.Logger
	settings config.Settings
}

func (*signerDiffCmd) Name() string { return "signer-diff" }
func (*signerDiffCmd) Synopsis() string {
	return "compare tenant signer keys here against kaufmann's copies, by derived address"
}
func (*signerDiffCmd) Usage() string {
	return `signer-diff:
	Plan 06 step 1. For every tenant with a signer on either side, decrypt both
	signer_key_enc values under their own master keys, derive the public address
	from each, and compare addresses only. Also cross-checks each derived
	address against the stored signer_address column on both sides, which is
	what everything that has never decrypted the key believes the signer to be.

	Kaufmann connection details come from the environment — the SAME variables
	the tenant backfill used, because they are the same secrets and a second
	name for one secret invites drift:

	  BACKFILL_KAUFMANN_DSN       postgres://... (kaufmann_oracle)
	  BACKFILL_KAUFMANN_ENC_KEY   its TENANT_SECRET_ENC_KEY

	Read-only on both databases. Exits non-zero if any pair differs or any
	ciphertext fails to decrypt; missing copies are reported and counted but do
	not fail the run, because "kaufmann tenant never backfilled" and "self-serve
	tenant with no signer anywhere" are findings, not faults.
`
}
func (*signerDiffCmd) SetFlags(*flag.FlagSet) {}

// signerSide is one database's belief about one tenant's signer: the address
// derived from its decrypted key, and the address column stored beside it.
type signerSide struct {
	name       string
	hasKey     bool
	derived    common.Address
	stored     string
	prefixed0x bool // key stored with a 0x prefix — see the format note below
	decryptErr bool // ciphertext would not open under this side's master key
}

func (p *signerDiffCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	kDSN, kKey := os.Getenv("BACKFILL_KAUFMANN_DSN"), os.Getenv("BACKFILL_KAUFMANN_ENC_KEY")
	if kDSN == "" || kKey == "" {
		p.logger.Error().Msg("BACKFILL_KAUFMANN_DSN and BACKFILL_KAUFMANN_ENC_KEY are required")
		return subcommands.ExitUsageError
	}
	if p.settings.TenantSecretEncKey == "" {
		// sha256("") is a valid AES key, so an empty passphrase would "work"
		// and simply fail to open every real ciphertext, reporting a hundred
		// percent drift that does not exist.
		p.logger.Error().Msg("TENANT_SECRET_ENC_KEY is empty — refusing to diff under sha256(\"\")")
		return subcommands.ExitFailure
	}

	kdb, err := sql.Open("postgres", kDSN)
	if err != nil {
		p.logger.Err(err).Msg("open kaufmann")
		return subcommands.ExitFailure
	}
	defer func() { _ = kdb.Close() }()

	store := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	store.WaitForDB(p.logger)

	local, err := p.readLocal(ctx, store.DBS().Reader)
	if err != nil {
		p.logger.Err(err).Msg("read tenant_credentials")
		return subcommands.ExitFailure
	}
	remote, err := p.readKaufmann(ctx, kdb, kKey)
	if err != nil {
		p.logger.Err(err).Msg("read kaufmann tenants")
		return subcommands.ExitFailure
	}

	// ---- compare, walking every tenant on either side ----

	var agree, differ, missingLocal, missingRemote, noSigner, driftStored, decryptFailed int

	for _, id := range unionKeys(local, remote) {
		l, hasL := local[id]
		r, hasR := remote[id]
		name := l.name
		if name == "" {
			name = r.name
		}
		log := p.logger.With().Str("tenant_id", id).Str("tenant", name).Logger()

		switch {
		case (hasL && l.decryptErr) || (hasR && r.decryptErr):
			// A row that would not open. Counted, never skipped silently, and
			// fatal at the end: an undecryptable ciphertext is either a corrupt
			// row or the wrong master key, and a diff that shrugged at it
			// would report differ=0 while comparing nothing.
			decryptFailed++
			log.Error().Str("verdict", "undecryptable").
				Bool("local", hasL && l.decryptErr).Bool("kaufmann", hasR && r.decryptErr).
				Msg("signer diff — ciphertext did not open under its own master key")
			continue
		case (!hasL || !l.hasKey) && (!hasR || !r.hasKey):
			// A tenant with no signer anywhere — self-serve, by design.
			noSigner++
			continue
		case !hasL || !l.hasKey:
			missingLocal++
			log.Warn().Str("verdict", "missing_local").
				Str("kaufmann_address", r.derived.Hex()).
				Msg("signer diff — kaufmann holds a signer this service does not")
			continue
		case !hasR || !r.hasKey:
			missingRemote++
			log.Warn().Str("verdict", "missing_remote").
				Str("local_address", l.derived.Hex()).
				Msg("signer diff — this service holds a signer kaufmann does not")
			continue
		}

		if l.derived != r.derived {
			differ++
			log.Error().Str("verdict", "differ").
				Str("local_address", l.derived.Hex()).
				Str("kaufmann_address", r.derived.Hex()).
				Msg("signer diff — THE TWO COPIES ARE DIFFERENT KEYS")
			continue
		}
		agree++

		// Same key both sides. Now check what everything that has never
		// decrypted it believes: the stored signer_address columns.
		driftStored += p.checkStored(log, "local", l)
		driftStored += p.checkStored(log, "kaufmann", r)

		// Format note, not a verdict: sharing.go parses the decrypted key
		// WITHOUT trimming a 0x prefix, so a prefixed local copy would sign
		// attestations fine and then fail the first share with a parse error.
		if l.prefixed0x {
			log.Warn().Msg("signer diff — local key is stored 0x-prefixed; the share path's HexToECDSA does not trim")
		}
	}

	p.logger.Info().
		Int("agree", agree).Int("differ", differ).
		Int("missing_local", missingLocal).Int("missing_remote", missingRemote).
		Int("no_signer", noSigner).Int("stored_address_drift", driftStored).
		Int("decrypt_failed", decryptFailed).
		Msg("signer diff complete")

	if differ > 0 || decryptFailed > 0 {
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

// checkStored compares one side's stored signer_address column against the
// address derived from its own key, returning 1 on drift. Stored-address drift
// is not key drift — the key is fine — but it means every reader of the column
// names the wrong signer.
func (p *signerDiffCmd) checkStored(log zerolog.Logger, side string, s signerSide) int {
	if s.stored == "" {
		return 0
	}
	if !strings.EqualFold(s.stored, s.derived.Hex()) {
		log.Error().Str("side", side).
			Str("stored", s.stored).
			Str("derived", s.derived.Hex()).
			Msg("signer diff — stored signer_address does not match the key beside it")
		return 1
	}
	return 0
}

// querier is what readLocal needs from either the shared store's wrapped
// reader or a bare *sql.DB.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (p *signerDiffCmd) readLocal(ctx context.Context, q querier) (map[string]signerSide, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT t.id, t.name, COALESCE(c.signer_address, ''), COALESCE(c.signer_key_enc, '')
		  FROM tenants t
		  LEFT JOIN tenant_credentials c ON c.tenant_id = t.id
		 ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]signerSide)
	for rows.Next() {
		var id, name, stored, keyEnc string
		if err := rows.Scan(&id, &name, &stored, &keyEnc); err != nil {
			return nil, err
		}
		side, err := deriveSide(p.settings.TenantSecretEncKey, name, stored, keyEnc)
		if err != nil {
			// Name the tenant, never the material. Recorded on the side and
			// kept walking — the summary fails the run, but only after every
			// tenant has been looked at.
			p.logger.Err(err).Str("tenant", name).Msg("decrypt local signer key")
			side.decryptErr = true
		}
		out[id] = side
	}
	return out, rows.Err()
}

func (p *signerDiffCmd) readKaufmann(ctx context.Context, kdb *sql.DB, kKey string) (map[string]signerSide, error) {
	rows, err := kdb.QueryContext(ctx, `
		SELECT id, name, COALESCE(signer_address, ''), COALESCE(signer_key_enc, '')
		  FROM kaufmann_oracle.tenants ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]signerSide)
	for rows.Next() {
		var id, name, stored, keyEnc string
		if err := rows.Scan(&id, &name, &stored, &keyEnc); err != nil {
			return nil, err
		}
		side, err := deriveSide(kKey, name, stored, keyEnc)
		if err != nil {
			p.logger.Err(err).Str("tenant", name).Msg("decrypt kaufmann signer key")
			side.decryptErr = true
		}
		out[id] = side
	}
	return out, rows.Err()
}

// deriveSide decrypts one ciphertext and derives its address. The plaintext
// key exists only inside this function; what escapes is the address and a
// format flag.
func deriveSide(passphrase, name, stored, keyEnc string) (signerSide, error) {
	s := signerSide{name: name, stored: stored}
	if keyEnc == "" {
		return s, nil
	}
	plain, err := service.DecryptSecret(passphrase, keyEnc)
	if err != nil {
		return s, err
	}
	s.prefixed0x = strings.HasPrefix(plain, "0x")
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(plain, "0x"))
	if err != nil {
		return s, err
	}
	s.hasKey = true
	s.derived = crypto.PubkeyToAddress(pk.PublicKey)
	return s, nil
}

// unionKeys returns every tenant id present on either side, sorted for a
// stable, comparable log order across runs.
func unionKeys(a, b map[string]signerSide) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for id := range a {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for id := range b {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
