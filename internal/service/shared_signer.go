package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
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
	// coldBudget is coldLookupBudget, held on the instance so a test can run a
	// batch out of budget in a few hundred milliseconds instead of three
	// seconds. Nothing in production sets it.
	coldBudget time.Duration
	// settings is consulted for exactly one bit: OwnerModeConfigured, which
	// gates whether the tenant's AA wallet counts as a signable owner.
	settings *config.Settings
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
//
// Deliberately still three seconds. The caller gives up at five and the budget
// is not the whole response — the credential resolve, the store lookup and
// serialization sit outside it — so buying a fourth second of lookups spends
// most of the remaining margin against the exact failure this bound exists to
// prevent. The per-owner accounts-api latency (~1-2.5s) is what limits how many
// owners a render can reach, and no setting of this constant fixes that; a
// cold fleet is warmed out of band instead, with `warm-shared-accounts`.
const coldLookupBudget = 3 * time.Second

// recordTimeout bounds ONE durable write of a learned answer.
//
// The writes run on a context detached from the caller's (see record), so they
// need a deadline of their own — without one, a wedged database would pin a
// lookup worker with no caller left to time it out.
const recordTimeout = 5 * time.Second

// warmLookupConcurrency is the fan-out for the out-of-band warm path, which has
// no caller waiting on it and runs one instance at a time. Wider than
// signerLookupConcurrency for that reason and no other: the request-path bound
// is about how much simultaneous load ONE page render is allowed to put on
// accounts-api, multiplied by every render in flight across every replica. A
// single job answers to neither.
const warmLookupConcurrency = 16

func NewSharedSignerService(logger *zerolog.Logger, accounts gateway.AccountsAPI,
	creds credentialProvider, store *SharedAccountStore, settings *config.Settings) *SharedSignerService {
	return &SharedSignerService{
		logger:     logger.With().Str("component", "shared-signer").Logger(),
		accounts:   accounts,
		creds:      creds,
		store:      store,
		settings:   settings,
		coldBudget: coldLookupBudget,
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

// FilterSignable returns the subset of owners this tenant may sign for, and
// the tenant's AA wallet address when one is configured (empty otherwise).
//
// This is the display gate behind fleet-lite's per-vehicle share button. Owners
// are deduplicated before any upstream call because a customer tenant's whole
// fleet typically sits on one kernel account — the list may hold a hundred
// vehicles and one distinct owner.
//
// The AA wallet is a positive with no lookup (docs/plans/08-aa-owner-signing.md):
// it is the tenant's own wallet, proven at config time to be controlled by the
// stored key, so accounts-api has no question to answer about it. It is
// reported positive even when the tenant has no signer at all — owner mode is
// exactly how a tenant without the signer arrangement shares.
//
// An infrastructure failure is fatal to the whole call rather than being
// treated as "not signable" per owner. Degrading to a partial answer would hide
// share buttons during an accounts-api blip and look exactly like the feature
// being switched off.
func (s *SharedSignerService) FilterSignable(ctx context.Context, tenantID string, owners []string) ([]string, []string, string, error) {
	cred, err := s.creds.Effective(ctx, tenantID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("resolve effective credential for %s: %w", tenantID, err)
	}
	var signer, aaWallet string
	if cred.SignerAddress != "" {
		signer = common.HexToAddress(cred.SignerAddress).Hex()
	}
	// Gated on the same switch the authorizer consults, so the display gate
	// cannot light up a wallet the execution path would refuse to sign for.
	// MaySignFor funnels through here too (check), which makes all three
	// surfaces — display, HTTP authorize, worker authorize — read one switch.
	if s.settings.OwnerModeConfigured() && cred.AAWalletAddress != "" {
		aaWallet = common.HexToAddress(cred.AAWalletAddress).Hex()
	}
	if signer == "" && aaWallet == "" {
		// Nothing to compare against and no wallet of our own: a denial for
		// every owner, not an error — the tenant is simply not set up for
		// server-signed operations of either kind.
		return []string{}, nil, "", nil
	}

	seen := map[string]bool{}
	distinct := make([]string, 0, len(owners))
	aaPositive := false
	for _, owner := range owners {
		if !common.IsHexAddress(owner) {
			continue
		}
		key := common.HexToAddress(owner).Hex()
		if !seen[key] {
			seen[key] = true
			if aaWallet != "" && key == aaWallet {
				// The tenant's own wallet: positive by construction, and kept
				// out of the lookup set so a signerless tenant never reaches
				// accounts-api at all.
				aaPositive = true
				continue
			}
			distinct = append(distinct, key)
		}
	}
	if len(distinct) == 0 || signer == "" {
		// Everything left would need the signer arrangement, and either there
		// is nothing left or there is no signer to compare against — the
		// remaining owners are denials, not unknowns.
		out := []string{}
		if aaPositive {
			out = append(out, aaWallet)
		}
		return out, nil, aaWallet, nil
	}

	known, err := s.store.Lookup(ctx, distinct)
	if err != nil {
		return nil, nil, "", err
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
		budget, cancel := context.WithTimeout(ctx, s.coldBudget)
		defer cancel()
		resolved, rerr := s.resolveMany(budget, tenantID, unknown, signerLookupConcurrency)
		if rerr != nil && !errors.Is(rerr, context.DeadlineExceeded) {
			// A real upstream failure is still all-or-nothing. A degraded
			// answer would hide share buttons during an accounts-api blip and
			// be indistinguishable from the feature being switched off.
			return nil, nil, "", rerr
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
				Msg("shared-account lookups ran out of budget; the resolved ones are recorded and the next call resolves more. " +
					"If this repeats with the counts barely moving, warm the fleet out of band: warm-shared-accounts -tenant <uuid>")
		}
	}

	out := []string{}
	if aaPositive {
		out = append(out, aaWallet)
	}
	for _, owner := range distinct {
		if registered[owner] != "" && strings.EqualFold(registered[owner], signer) {
			out = append(out, owner)
		}
	}
	return out, unresolved, aaWallet, nil
}

// resolveMany asks accounts-api about the owners nothing is known about, up to
// concurrency at a time, and records every answer so the next render does not
// ask again.
//
// The developer JWT is minted ONCE for the whole batch rather than per owner.
// It was per owner before, which meant a cold render's cost was multiplied by
// the credential path as well as the accounts-api call.
//
// EACH ANSWER IS RECORDED THE MOMENT IT ARRIVES, in the worker that obtained
// it, rather than in a drain loop after wg.Wait(). Persisting after the fact
// was correct only for calls that finished inside their budget — which are
// exactly the calls where none of this matters. The batches worth remembering
// are the ones the deadline lands in the middle of, and for those the drain ran
// after the budget had already expired. Recording as we go also means the
// answers are durable before the process has any chance to lose them, and it
// costs nothing: one upsert per owner either way, moved off the tail and spread
// across workers that spend ~99% of their time waiting on accounts-api.
func (s *SharedSignerService) resolveMany(ctx context.Context, tenantID string, owners []string, concurrency int) (map[string]string, error) {
	minted, err := s.creds.DeveloperJWT(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("mint developer JWT for %s: %w", tenantID, err)
	}

	// Derived once, here, so every write in this batch shares the caller's
	// values and none of its cancellation. See record.
	keep := context.WithoutCancel(ctx)

	type result struct {
		owner  string
		signer string
		err    error
	}
	jobs := make(chan string)
	results := make(chan result, len(owners))

	workers := concurrency
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
				if lerr == nil {
					s.record(keep, owner, sa)
				}
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
	}
	// A blown budget is not a failure: what was resolved has been recorded and
	// is returned, and the caller reports the rest as unresolved.
	if firstErr != nil && !errors.Is(firstErr, context.DeadlineExceeded) &&
		!errors.Is(firstErr, context.Canceled) {
		return nil, firstErr
	}
	return out, nil
}

// record persists one learned answer on a context detached from the caller's.
//
// THE DETACHMENT IS THE POINT. The lookups run under coldLookupBudget, and the
// answers most worth keeping are the ones obtained just before it expires —
// so writing them through that same context meant that on every call which
// exhausted its budget, every write failed with "context deadline exceeded"
// and the service learned nothing from work it had already paid accounts-api
// for. In prod the Kaufmann tenant's 162 owners resolved 9 on one render and 22
// on the next half an hour later: the same cold lookups, redone and
// rediscarded, with the store never warming and the share button never
// appearing for owners whose authorisation accounts-api was returning happily.
//
// context.WithoutCancel rather than context.Background so the request's logging
// and tracing values survive into the write; the deadline is fresh because the
// write is not part of what the budget is bounding. Note that this only stays
// safe while recording is SYNCHRONOUS within the call: ctx here descends from
// fiber's *fasthttp.RequestCtx, which is recycled once the handler returns, so
// a detached copy must never outlive it. It doesn't — resolveMany's workers are
// all joined before FilterSignable returns.
//
// A failed write is not fatal. The answer is correct; only the remembering
// failed, and the cost is asking again next time.
func (s *SharedSignerService) record(parent context.Context, owner, signer string) {
	// Recorded even when the signer is empty: "asked, has none" is worth
	// remembering for a day, and it is the answer for most wallets.
	ctx, cancel := context.WithTimeout(parent, recordTimeout)
	defer cancel()
	if err := s.store.Record(ctx, owner, signer); err != nil {
		s.logger.Warn().Err(err).Str("owner", owner).Msg("could not record shared-account lookup")
	}
}

// ColdOwners returns the checksummed, deduplicated subset of owners that has no
// usable answer on record and would have to be asked about.
//
// It applies exactly the freshness rule the request path applies — a positive
// is permanent, a negative is re-asked after negativeRecheckAfter — because the
// rule belongs in one place. A warm run is not a reason to re-ask accounts-api
// about a signer that cannot have changed, and a second copy of that rule in a
// SQL predicate somewhere would be free to drift from this one.
//
// Exported so a dry run can report what a real run would cost without spending
// it.
func (s *SharedSignerService) ColdOwners(ctx context.Context, owners []string) ([]string, error) {
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
		return nil, nil
	}

	known, err := s.store.Lookup(ctx, distinct)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cold := make([]string, 0, len(distinct))
	for _, owner := range distinct {
		if rec, ok := known[owner]; ok && rec.Fresh(now) {
			continue
		}
		cold = append(cold, owner)
	}
	return cold, nil
}

// WarmResult is what one warm run learned. Cold is what it actually had to
// ask about; Requested minus Cold is what was already known.
type WarmResult struct {
	Requested int
	Cold      int
	Positive  int
	Negative  int
	Failed    int
}

// Warm resolves owners and records the answers OUTSIDE a request, with no
// budget but the caller's own context.
//
// This is the answer to convergence, and the reason it is not "raise
// coldLookupBudget" or "raise signerLookupConcurrency". Accounts-api answers a
// wallet in roughly one to two and a half seconds, so a render is limited to
// about (budget / latency) x concurrency owners no matter how those two are
// tuned — 8-wide over 3 seconds is the 9 and 22 seen in prod. Reaching 162
// owners in one render needs either a budget past the caller's five-second
// patience or a fan-out that puts fifty-odd simultaneous requests on
// accounts-api per page load, per replica. Both trade a real limit for a worse
// one. A fleet is a fixed set of owners that changes slowly; resolving it is a
// batch job that happens to have been living inside a page render.
//
// So: run this once for a tenant and its whole fleet is warm, permanently for
// every positive (docs/signer-permanence.md). The request path then only ever
// meets the handful of owners that appeared since — a vehicle transferred, a
// fleet extended — which is what its 3-second budget was always sized for.
func (s *SharedSignerService) Warm(ctx context.Context, tenantID string, owners []string, concurrency int) (WarmResult, error) {
	res := WarmResult{Requested: len(owners)}
	if concurrency <= 0 {
		concurrency = warmLookupConcurrency
	}

	cold, err := s.ColdOwners(ctx, owners)
	if err != nil {
		return res, err
	}
	res.Cold = len(cold)
	if len(cold) == 0 {
		return res, nil
	}

	resolved, err := s.resolveMany(ctx, tenantID, cold, concurrency)
	if err != nil {
		// Whatever was resolved before this is already in the table — the
		// recording no longer depends on the batch finishing.
		return res, err
	}
	for _, sa := range resolved {
		if sa == "" {
			res.Negative++
			continue
		}
		res.Positive++
	}
	res.Failed = len(cold) - len(resolved)
	return res, nil
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

func (s *SharedSignerService) check(ctx context.Context, tenantID, ownerAddress string) (bool, error) {
	if !common.IsHexAddress(ownerAddress) {
		return false, nil
	}
	signable, _, _, err := s.FilterSignable(ctx, tenantID, []string{ownerAddress})
	if err != nil {
		return false, err
	}
	return len(signable) == 1, nil
}
