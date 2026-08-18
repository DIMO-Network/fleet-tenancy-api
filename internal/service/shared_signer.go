package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog"
)

// ErrSignerNotAuthorized means the owner's kernel account did not register this
// tenant's signer, so the tenant cannot act on the owner's behalf. It is a
// policy answer, not a failure — most wallets on earth will produce it — and
// callers must keep it distinct from an infrastructure error: this one is a
// 403, an accounts-api outage is a 5xx.
var ErrSignerNotAuthorized = errors.New("owner account has not authorized this tenant's signer")

// signerCacheTTL bounds how stale a display gate may be.
//
// The window is what separates the button fleet-lite renders from the check
// made when the share is submitted. Too long and a revoked signer keeps
// offering a share that then 403s; too short and every vehicle-list render
// fans out to accounts-api. A minute keeps the disagreement to something a
// user would read as "I clicked too fast" rather than as a broken feature.
const signerCacheTTL = time.Minute

// SharedSignerService answers one question: may this tenant sign for this
// owner's kernel account?
//
// It asks accounts-api every time (modulo the cache), rather than reading
// users.shared_account_signer_address. That column is deliberately unused here.
// It has one writer — ProvisionService, and only when this service created the
// account — so it is empty for every owner whose account kaufmann-oracle
// created, which is precisely the population vehicle sharing targets. Worse, a
// cached column can disagree with accounts-api in both directions: it can offer
// a share that will be refused, and it can hide one that would work with
// nothing to repair it. Resolving live makes the display gate and the execution
// gate the same question against the same source.
type SharedSignerService struct {
	logger   zerolog.Logger
	accounts gateway.AccountsAPI
	// The narrowed CredentialService, as provisioning uses: minting reaches
	// auth and identity, and this service's own logic is worth testing without
	// either.
	creds credentialProvider

	mu    sync.Mutex
	cache map[string]signerCacheEntry
}

type signerCacheEntry struct {
	authorized bool
	at         time.Time
}

func NewSharedSignerService(logger *zerolog.Logger, accounts gateway.AccountsAPI, creds credentialProvider) *SharedSignerService {
	return &SharedSignerService{
		logger:   logger.With().Str("component", "shared-signer").Logger(),
		accounts: accounts,
		creds:    creds,
		cache:    map[string]signerCacheEntry{},
	}
}

// MaySignFor reports whether the tenant's effective signer is registered on the
// owner's kernel account.
//
// Returns ErrSignerNotAuthorized for a policy denial, or a wrapped error for an
// infrastructure failure. The distinction is the caller's whole basis for
// choosing 403 over 503, so it must never collapse — an accounts-api outage
// that read as "not authorized" would silently turn every share into a
// permission error and send people looking in the wrong place.
//
// A wallet with no DIMO account at all is a denial, not an error: plenty of
// vehicles are owned by addresses that never went through accounts-api.
func (s *SharedSignerService) MaySignFor(ctx context.Context, tenantID, ownerAddress string) error {
	authorized, err := s.check(ctx, tenantID, ownerAddress)
	if err != nil {
		return err
	}
	if !authorized {
		return ErrSignerNotAuthorized
	}
	return nil
}

// FilterSignable returns the subset of owners this tenant may sign for.
//
// This is the display gate behind fleet-lite's per-vehicle share button. Owners
// are deduplicated before any upstream call because a customer tenant's whole
// fleet typically sits on one kernel account — the list may hold a hundred
// vehicles and one distinct owner.
//
// An infrastructure failure is fatal to the whole call rather than being
// treated as "not signable" per owner. Degrading to a partial answer would hide
// share buttons during an accounts-api blip and look exactly like the feature
// being switched off.
func (s *SharedSignerService) FilterSignable(ctx context.Context, tenantID string, owners []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, owner := range owners {
		if !common.IsHexAddress(owner) {
			continue
		}
		key := common.HexToAddress(owner).Hex()
		if seen[key] {
			continue
		}
		seen[key] = true

		authorized, err := s.check(ctx, tenantID, key)
		if err != nil {
			return nil, err
		}
		if authorized {
			out = append(out, key)
		}
	}
	return out, nil
}

func (s *SharedSignerService) check(ctx context.Context, tenantID, ownerAddress string) (bool, error) {
	if !common.IsHexAddress(ownerAddress) {
		return false, nil
	}
	// Checksummed before it is used as a cache key or compared, so the same
	// owner arriving lowercased from one caller and checksummed from another is
	// one entry and one answer.
	owner := common.HexToAddress(ownerAddress).Hex()

	cred, err := s.creds.Effective(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("resolve effective credential for %s: %w", tenantID, err)
	}
	if cred.SignerAddress == "" {
		// The tenant has a license but no signer, so there is nothing it could
		// sign with. A denial rather than an error: the tenant is simply not
		// set up for shared signing, which is a configuration state an operator
		// can fix, not a fault in this request.
		return false, nil
	}
	signer := common.HexToAddress(cred.SignerAddress).Hex()

	key := tenantID + "|" + owner + "|" + signer
	if entry, ok := s.cached(key); ok {
		return entry, nil
	}

	// Minted from the tenant's effective credential: accounts-api only echoes
	// the extended account shape to allowlisted developer licenses, and without
	// it ProvidedSignerAddress comes back empty and every owner reads as
	// unauthorized.
	minted, err := s.creds.DeveloperJWT(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("mint developer JWT for %s: %w", tenantID, err)
	}

	account, err := s.accounts.GetAccountByWallet(owner, minted.Token)
	switch {
	case errors.Is(err, gateway.ErrAccountNotFound):
		s.store(key, false)
		return false, nil
	case err != nil:
		return false, fmt.Errorf("accounts-api lookup for owner %s: %w", owner, err)
	}

	authorized := account != nil &&
		account.ProvidedSignerAddress != "" &&
		strings.EqualFold(account.ProvidedSignerAddress, signer)

	s.store(key, authorized)
	return authorized, nil
}

func (s *SharedSignerService) cached(key string) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || time.Since(entry.at) > signerCacheTTL {
		return false, false
	}
	return entry.authorized, true
}

func (s *SharedSignerService) store(key string, authorized bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = signerCacheEntry{authorized: authorized, at: time.Now()}
}
