package service

import (
	"context"
	"fmt"
	"time"

	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/lib/pq"
)

// negativeRecheckAfter bounds how long "checked, has no shared account" is
// believed.
//
// Positives are never re-checked — providedSignerAddress cannot be revoked, so
// a stored signer stays true forever (docs/signer-permanence.md). Negatives are
// not the mirror image: a wallet with no shared account today can register one
// tomorrow, and freezing that would permanently hide sharing from anyone looked
// up shortly before their account existed. A day is short enough that the wait
// is a day rather than forever, and long enough that the fleet's unshareable
// majority — most vehicle owners are ordinary wallets — costs one lookup a day
// instead of one per page render.
const negativeRecheckAfter = 24 * time.Hour

// SharedAccountRecord is what this service remembers about one wallet's kernel
// account. SignerAddress empty means "asked, has none".
type SharedAccountRecord struct {
	Wallet        string
	SignerAddress string
	CheckedAt     time.Time
}

// Fresh reports whether the record can answer without asking accounts-api
// again: a positive always can, a negative only until it ages out.
func (r SharedAccountRecord) Fresh(now time.Time) bool {
	if r.SignerAddress != "" {
		return true
	}
	return now.Sub(r.CheckedAt) < negativeRecheckAfter
}

// SharedAccountStore is the durable half of the signer gate — what accounts-api
// has already told us, so a fleet render is one indexed query instead of one
// HTTP call per distinct owner.
type SharedAccountStore struct {
	pdb *db.Store
}

func NewSharedAccountStore(pdb *db.Store) *SharedAccountStore {
	return &SharedAccountStore{pdb: pdb}
}

// Lookup returns what is known about these wallets, keyed by checksummed
// address. Wallets with no row are absent from the result — the caller resolves
// those live and records what it learns.
func (s *SharedAccountStore) Lookup(ctx context.Context, wallets []string) (map[string]SharedAccountRecord, error) {
	out := make(map[string]SharedAccountRecord, len(wallets))
	if len(wallets) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(wallets))
	for _, w := range wallets {
		if common.IsHexAddress(w) {
			keys = append(keys, common.HexToAddress(w).Hex())
		}
	}
	if len(keys) == 0 {
		return out, nil
	}

	rows, err := s.pdb.DBS().Reader.QueryContext(ctx, `
		SELECT wallet, COALESCE(signer_address, ''), checked_at
		  FROM shared_accounts WHERE wallet = ANY($1)`, pq.Array(keys))
	if err != nil {
		return nil, fmt.Errorf("read shared accounts: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var r SharedAccountRecord
		if err := rows.Scan(&r.Wallet, &r.SignerAddress, &r.CheckedAt); err != nil {
			return nil, fmt.Errorf("scan shared account: %w", err)
		}
		out[r.Wallet] = r
	}
	return out, rows.Err()
}

// Record stores what accounts-api said about one wallet.
//
// A POSITIVE IS NEVER OVERWRITTEN BY A NEGATIVE. The write is conditional in
// SQL rather than in Go, so two concurrent renders — one reading a stale
// "no account", one reading the registration — cannot land in an order that
// erases a known signer. There is no path in accounts-api that unregisters one,
// so a negative arriving after a positive is always the older truth.
func (s *SharedAccountStore) Record(ctx context.Context, wallet, signerAddress string) error {
	if !common.IsHexAddress(wallet) {
		return nil
	}
	key := common.HexToAddress(wallet).Hex()
	var signer any
	if signerAddress != "" {
		if !common.IsHexAddress(signerAddress) {
			return fmt.Errorf("record shared account for %s: signer %q is not an address", key, signerAddress)
		}
		signer = common.HexToAddress(signerAddress).Hex()
	}

	_, err := s.pdb.DBS().Writer.ExecContext(ctx, `
		INSERT INTO shared_accounts (wallet, signer_address, checked_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (wallet) DO UPDATE
		   SET signer_address = COALESCE(EXCLUDED.signer_address, shared_accounts.signer_address),
		       checked_at     = NOW(),
		       updated_at     = NOW()`,
		key, signer)
	if err != nil {
		return fmt.Errorf("record shared account for %s: %w", key, err)
	}
	return nil
}
