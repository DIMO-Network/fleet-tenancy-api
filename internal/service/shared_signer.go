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
	// store is the durable memory of what accounts-api said. It replaces the
	// one-minute in-process cache: that cache expired faster than a single
	// fleet render took, so a 600-vehicle operator paid hundreds of sequential
	// accounts-api calls on every page load and timed out its caller.
	store *SharedAccountStore
}

// signerLookupConcurrency bounds the fan-out when owners are genuinely unknown.
// Sequential was the original shape and it is what made a cold render take 45
// seconds against a 5-second client timeout; unbounded would turn one page load
// into a few hundred simultaneous requests at accounts-api.
const signerLookupConcurrency = 8

// coldLookupBudget bounds how long ONE call spends resolving owners nothing is
// known about.
//
// The alternative — resolve them all, however long it takes — is what produced
// a 45-second response against a caller that gives up at five, and a caller
// that gives up cancels the context, so the work is thrown away and NOTHING is
// learned. That fleet would never warm up: every render pays the full cost and
// every render is discarded. Bounding the work means each call resolves a
// chunk, records it, and returns; a large fleet converges over a few renders
// instead of failing forever.
const coldLookupBudget = 3 * time.Second

func NewSharedSignerService(logger *zerolog.Logger, accounts gateway.AccountsAPI,
	creds credentialProvider, store *SharedAccountStore) *SharedSignerService {
	return &SharedSignerService{
		logger:   logger.With().Str("component", "shared-signer").Logger(),
		accounts: accounts,
		creds:    creds,
		store:    store,
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
func (s *SharedSignerService) FilterSignable(ctx context.Context, tenantID string, owners []string) ([]string, []string, error) {
	signer, err := s.tenantSigner(ctx, tenantID)
	if err != nil || signer == "" {
		// No signer means nothing to compare against, and no reason to ask
		// accounts-api about a single owner. A denial for every owner, not an
		// error: the tenant is simply not set up for shared signing.
		return []string{}, nil, err
	}

	seen := map[string]bool{}
	distinct := make([]string, 0, len(owners))
	for _, owner := range owners {
		if !common.IsHexAddress(owner) {
			continue
		}
		key := common.HexToAddress(owner).Hex()
		if !seen[key] {
			seen[key] = true
			distinct = append(distinct, key)
		}
	}
	if len(distinct) == 0 {
		return []string{}, nil, nil
	}

	known, err := s.store.Lookup(ctx, distinct)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	registered := make(map[string]string, len(distinct))
	unknown := make([]string, 0)
	for _, owner := range distinct {
		if rec, ok := known[owner]; ok && rec.Fresh(now) {
			registered[owner] = rec.SignerAddress
			continue
		}
		unknown = append(unknown, owner)
	}

	var unresolved []string
	if len(unknown) > 0 {
		budget, cancel := context.WithTimeout(ctx, coldLookupBudget)
		defer cancel()
		resolved, rerr := s.resolveMany(budget, tenantID, unknown)
		if rerr != nil && !errors.Is(rerr, context.DeadlineExceeded) {
			// A real upstream failure is still all-or-nothing. A degraded
			// answer would hide share buttons during an accounts-api blip and
			// be indistinguishable from the feature being switched off.
			return nil, nil, rerr
		}
		for owner, sa := range resolved {
			registered[owner] = sa
		}
		for _, owner := range unknown {
			if _, ok := registered[owner]; !ok {
				unresolved = append(unresolved, owner)
			}
		}
		if len(unresolved) > 0 {
			s.logger.Info().Str("tenant_id", tenantID).
				Int("resolved", len(resolved)).Int("unresolved", len(unresolved)).
				Msg("shared-account lookups ran out of budget; the next call resolves more")
		}
	}

	out := []string{}
	for _, owner := range distinct {
		if registered[owner] != "" && strings.EqualFold(registered[owner], signer) {
			out = append(out, owner)
		}
	}
	return out, unresolved, nil
}

// resolveMany asks accounts-api about the owners nothing is known about, up to
// signerLookupConcurrency at a time, and records every answer so the next
// render does not ask again.
//
// The developer JWT is minted ONCE for the whole batch rather than per owner.
// It was per owner before, which meant a cold render's cost was multiplied by
// the credential path as well as the accounts-api call.
func (s *SharedSignerService) resolveMany(ctx context.Context, tenantID string, owners []string) (map[string]string, error) {
	minted, err := s.creds.DeveloperJWT(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("mint developer JWT for %s: %w", tenantID, err)
	}

	type result struct {
		owner  string
		signer string
		err    error
	}
	jobs := make(chan string)
	results := make(chan result, len(owners))

	workers := signerLookupConcurrency
	if len(owners) < workers {
		workers = len(owners)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for owner := range jobs {
				sa, lerr := s.lookupSigner(ctx, owner, minted.Token)
				results <- result{owner: owner, signer: sa, err: lerr}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, o := range owners {
			select {
			case jobs <- o:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(results)

	out := make(map[string]string, len(owners))
	var firstErr error
	for r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		out[r.owner] = r.signer
		// Recorded even when empty: "asked, has none" is worth remembering for
		// a day, and it is the answer for most wallets.
		if rerr := s.store.Record(ctx, r.owner, r.signer); rerr != nil {
			// Not fatal. The answer is correct; only the remembering failed,
			// and the cost is asking again next time.
			s.logger.Warn().Err(rerr).Str("owner", r.owner).Msg("could not record shared-account lookup")
		}
	}
	// A blown budget is not a failure: what was resolved is recorded and
	// returned, and the caller reports the rest as unresolved.
	if firstErr != nil && !errors.Is(firstErr, context.DeadlineExceeded) &&
		!errors.Is(firstErr, context.Canceled) {
		return nil, firstErr
	}
	return out, nil
}

// lookupSigner returns the signer registered on an owner's kernel account, or
// "" when the owner has no shared account. A wallet accounts-api does not know
// is not an error: plenty of vehicles are owned by addresses that never went
// through it.
func (s *SharedSignerService) lookupSigner(ctx context.Context, owner, token string) (string, error) {
	account, err := s.accounts.GetAccountByWallet(owner, token)
	switch {
	case errors.Is(err, gateway.ErrAccountNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("accounts-api lookup for owner %s: %w", owner, err)
	}
	if account == nil || account.ProvidedSignerAddress == "" {
		return "", nil
	}
	return account.ProvidedSignerAddress, nil
}

// tenantSigner is the tenant's effective signer, resolved ONCE per call. The
// old shape resolved the effective credential inside the per-owner check, so a
// fleet with three hundred distinct owners resolved it three hundred times.
func (s *SharedSignerService) tenantSigner(ctx context.Context, tenantID string) (string, error) {
	cred, err := s.creds.Effective(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("resolve effective credential for %s: %w", tenantID, err)
	}
	if cred.SignerAddress == "" {
		return "", nil
	}
	return common.HexToAddress(cred.SignerAddress).Hex(), nil
}

func (s *SharedSignerService) check(ctx context.Context, tenantID, ownerAddress string) (bool, error) {
	if !common.IsHexAddress(ownerAddress) {
		return false, nil
	}
	signable, _, err := s.FilterSignable(ctx, tenantID, []string{ownerAddress})
	if err != nil {
		return false, err
	}
	return len(signable) == 1, nil
}
