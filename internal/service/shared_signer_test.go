package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lower(s string) string { return strings.ToLower(s) }

// EIP-55 checksummed, because FilterSignable returns checksummed addresses and
// the caller compares them as strings. Getting these wrong in a test is the
// same mistake that would hide a share button in production.
const (
	tenantSigner = "0xAAaaAa0000000000000000000000000000000001"
	ownerWallet  = "0xBbbBBB0000000000000000000000000000000002"
	otherSigner  = "0xccCCcc0000000000000000000000000000000003"
)

func signerFixture(t *testing.T, accounts *fakeAccounts, signer string) *SharedSignerService {
	t.Helper()
	logger := zerolog.Nop()
	creds := &fakeCreds{
		minted:    &models.MintedToken{Token: "jwt", ClientID: "0xclient"},
		effective: &EffectiveCredential{TenantID: "t1", ClientID: "0xclient", SignerAddress: signer},
	}
	// The store is durable now, so each test must start from "nothing is
	// known" — otherwise an earlier test's learned answer silently satisfies a
	// later one and the accounts-api behaviour under test is never exercised.
	store := testStore(t)
	clear := func() {
		// lower() on both sides: rows are stored EIP-55 checksummed, so an
		// exact match against a differently-cased constant silently deletes
		// nothing and the next test inherits this one's answer.
		_, _ = store.DBS().Writer.Exec(
			`DELETE FROM shared_accounts WHERE lower(wallet) = ANY($1)`,
			pq.Array([]string{strings.ToLower(ownerWallet), strings.ToLower(tenantSigner), strings.ToLower(otherSigner)}))
	}
	clear()
	t.Cleanup(clear)
	return NewSharedSignerService(&logger, accounts, creds, NewSharedAccountStore(store))
}

// The happy path: the owner's kernel registered this tenant's signer, so the
// tenant may share on its behalf without the owner's passkey.
func TestMaySignFor_AuthorizedWhenSignerMatches(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: tenantSigner},
	}}
	svc := signerFixture(t, accounts, tenantSigner)

	assert.NoError(t, svc.MaySignFor(context.Background(), "t1", ownerWallet))
}

// Accounts-api is the authority and it says no. This is the ordinary answer for
// any vehicle whose owner did not come through the tenant's account creation.
func TestMaySignFor_DeniedWhenSignerDiffers(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: otherSigner},
	}}
	svc := signerFixture(t, accounts, tenantSigner)

	assert.ErrorIs(t, svc.MaySignFor(context.Background(), "t1", ownerWallet), ErrSignerNotAuthorized)
}

// A vehicle can be owned by any address, including one that never went through
// accounts-api. That is a denial, not a fault — it must not surface as a 5xx.
func TestMaySignFor_UnknownAccountIsDenialNotError(t *testing.T) {
	svc := signerFixture(t, &fakeAccounts{byWallet: map[string]*gateway.Account{}}, tenantSigner)

	err := svc.MaySignFor(context.Background(), "t1", ownerWallet)
	assert.ErrorIs(t, err, ErrSignerNotAuthorized)
}

// An account with no signer at all is a denial for the same reason.
func TestMaySignFor_EmptyProvidedSignerIsDenial(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: ""},
	}}
	svc := signerFixture(t, accounts, tenantSigner)

	assert.ErrorIs(t, svc.MaySignFor(context.Background(), "t1", ownerWallet), ErrSignerNotAuthorized)
}

// THE DISTINCTION THIS SERVICE EXISTS TO KEEP. An accounts-api outage must not
// read as "not authorized". Collapsed, every share during an upstream blip
// would surface as a permission error and send people looking for a revoked
// signer that was never revoked.
func TestMaySignFor_UpstreamFailureIsNotADenial(t *testing.T) {
	boom := errors.New("accounts-api unreachable")
	svc := signerFixture(t, &fakeAccounts{byWalletErr: boom}, tenantSigner)

	err := svc.MaySignFor(context.Background(), "t1", ownerWallet)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrSignerNotAuthorized, "an outage is not a policy denial")
	assert.ErrorIs(t, err, boom)
}

// A tenant with a license but no signer has nothing to sign with. That is a
// configuration state an operator can fix, not a fault in this request.
func TestMaySignFor_TenantWithoutSignerIsDenial(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: tenantSigner},
	}}
	svc := signerFixture(t, accounts, "")

	assert.ErrorIs(t, svc.MaySignFor(context.Background(), "t1", ownerWallet), ErrSignerNotAuthorized)
}

// Addresses reach this service from several directions — on-chain owner
// lookups, a caller's database, a JSON body — with inconsistent casing. The
// comparison must not care, or the share button disappears for a correct owner.
func TestMaySignFor_ComparisonIsCaseInsensitive(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: lower(tenantSigner)},
	}}
	svc := signerFixture(t, accounts, tenantSigner)

	assert.NoError(t, svc.MaySignFor(context.Background(), "t1", lower(ownerWallet)))
}

// FilterSignable is the display gate. It must return only authorized owners,
// checksummed, and must not be confused by duplicates or junk.
func TestFilterSignable(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: tenantSigner},
		lower(otherSigner): {WalletAddress: otherSigner, ProvidedSignerAddress: otherSigner},
	}}
	svc := signerFixture(t, accounts, tenantSigner)

	got, _, err := svc.FilterSignable(context.Background(), "t1",
		[]string{lower(ownerWallet), otherSigner, "not-an-address", ""})
	require.NoError(t, err)
	assert.Equal(t, []string{ownerWallet}, got,
		"only the owner whose kernel registered this signer, EIP-55 checksummed")
}

// A customer tenant's whole fleet usually sits on one kernel account, so the
// same owner arrives once per vehicle. Deduplicating before the upstream call
// is what keeps a hundred-vehicle list from becoming a hundred lookups.
func TestFilterSignable_DeduplicatesBeforeCallingUpstream(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: tenantSigner},
	}}
	svc := signerFixture(t, accounts, tenantSigner)

	owners := make([]string, 50)
	for i := range owners {
		owners[i] = ownerWallet
	}
	got, _, err := svc.FilterSignable(context.Background(), "t1", owners)
	require.NoError(t, err)
	assert.Equal(t, []string{ownerWallet}, got, "one distinct owner, one entry")
}

// Repeated questions about the same owner must not re-hit accounts-api within
// the TTL — a vehicle list renders this gate on every request.
func TestFilterSignable_CachesWithinTTL(t *testing.T) {
	accounts := &countingAccounts{inner: &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: tenantSigner},
	}}}
	svc := signerFixture(t, accounts.fake(), tenantSigner)
	svc.accounts = accounts

	for i := 0; i < 3; i++ {
		_, _, err := svc.FilterSignable(context.Background(), "t1", []string{ownerWallet})
		require.NoError(t, err)
	}
	assert.Equal(t, 1, accounts.calls, "the answer should be cached across calls")
}

// An upstream failure fails the whole call rather than quietly shortening the
// list. A partial answer would hide share buttons during a blip and be
// indistinguishable from the feature being off.
func TestFilterSignable_UpstreamFailureFailsTheCall(t *testing.T) {
	svc := signerFixture(t, &fakeAccounts{byWalletErr: errors.New("boom")}, tenantSigner)

	_, _, err := svc.FilterSignable(context.Background(), "t1", []string{ownerWallet})
	assert.Error(t, err, "a degraded answer would look like the feature being switched off")
}

type countingAccounts struct {
	inner *fakeAccounts
	calls int
}

func (c *countingAccounts) fake() *fakeAccounts { return c.inner }
func (c *countingAccounts) GetAccountByEmail(e, j string) (*gateway.Account, error) {
	return c.inner.GetAccountByEmail(e, j)
}
func (c *countingAccounts) CreateAccount(e, s, j string) (*gateway.Account, error) {
	return c.inner.CreateAccount(e, s, j)
}
func (c *countingAccounts) GetAccountByWallet(w, j string) (*gateway.Account, error) {
	c.calls++
	return c.inner.GetAccountByWallet(w, j)
}

// ---- the durable store (2026-08-22) ----

// THE POINT OF THE WHOLE CHANGE. A learned answer survives the process, so the
// second render asks accounts-api nothing. Before this, a 600-vehicle operator
// with hundreds of distinct owners paid one sequential HTTP call per owner on
// EVERY page load — 45 seconds against a caller that gives up at 5.
func TestFilterSignable_LearnedAnswerSurvivesANewService(t *testing.T) {
	accounts := &countingAccounts{inner: &fakeAccounts{byWallet: map[string]*gateway.Account{
		lower(ownerWallet): {WalletAddress: ownerWallet, ProvidedSignerAddress: tenantSigner},
	}}}
	svc := signerFixture(t, accounts.fake(), tenantSigner)
	svc.accounts = accounts

	got, _, err := svc.FilterSignable(context.Background(), "t1", []string{ownerWallet})
	require.NoError(t, err)
	require.Equal(t, []string{ownerWallet}, got)
	require.Equal(t, 1, accounts.calls)

	// A different service instance — a restart, or the other replica.
	logger := zerolog.Nop()
	creds := &fakeCreds{
		minted:    &models.MintedToken{Token: "jwt", ClientID: "0xclient"},
		effective: &EffectiveCredential{TenantID: "t1", ClientID: "0xclient", SignerAddress: tenantSigner},
	}
	fresh := NewSharedSignerService(&logger, accounts, creds, NewSharedAccountStore(testStore(t)))

	got, _, err = fresh.FilterSignable(context.Background(), "t1", []string{ownerWallet})
	require.NoError(t, err)
	assert.Equal(t, []string{ownerWallet}, got)
	assert.Equal(t, 1, accounts.calls, "a restart must not re-ask what is already known")
}

// A positive is permanent because providedSignerAddress cannot be revoked
// (docs/signer-permanence.md). This is the property that makes storing it
// different from caching it — and if accounts-api ever grows a revoke, this
// test is the one that must be revisited, not quietly deleted.
func TestSharedAccountRecord_PositiveNeverGoesStale(t *testing.T) {
	rec := SharedAccountRecord{SignerAddress: tenantSigner, CheckedAt: time.Now().Add(-3650 * 24 * time.Hour)}
	assert.True(t, rec.Fresh(time.Now()), "a registered signer cannot be revoked, so age is irrelevant")
}

// A negative is NOT the mirror image: a wallet with no shared account today can
// register one tomorrow. Freezing it would permanently hide sharing from anyone
// looked up shortly before their account existed.
func TestSharedAccountRecord_NegativeAgesOut(t *testing.T) {
	now := time.Now()
	assert.True(t, SharedAccountRecord{CheckedAt: now.Add(-time.Hour)}.Fresh(now),
		"a recent negative is still believed")
	assert.False(t, SharedAccountRecord{CheckedAt: now.Add(-negativeRecheckAfter - time.Minute)}.Fresh(now),
		"an old negative must be asked again — accounts can be created")
}

// A negative arriving after a positive is always the older truth, since nothing
// unregisters a signer. Two concurrent renders must not be able to land in an
// order that erases what is known, so the guard is in the SQL rather than in Go.
func TestSharedAccountStore_NegativeNeverErasesAPositive(t *testing.T) {
	store := NewSharedAccountStore(testStore(t))
	ctx := context.Background()
	defer func() {
		_, _ = store.pdb.DBS().Writer.Exec(`DELETE FROM shared_accounts WHERE wallet = $1`, ownerWallet)
	}()

	require.NoError(t, store.Record(ctx, ownerWallet, tenantSigner))
	require.NoError(t, store.Record(ctx, ownerWallet, ""))

	got, err := store.Lookup(ctx, []string{ownerWallet})
	require.NoError(t, err)
	assert.Equal(t, tenantSigner, got[ownerWallet].SignerAddress,
		"a later 'no account' must not erase a signer that cannot have been revoked")
}

// "Asked, and this wallet has none" is worth remembering — it is the answer for
// most vehicle owners, and re-asking it per render is exactly the cost this
// change removes.
func TestFilterSignable_RemembersNegatives(t *testing.T) {
	accounts := &countingAccounts{inner: &fakeAccounts{byWallet: map[string]*gateway.Account{}}}
	svc := signerFixture(t, accounts.fake(), tenantSigner)
	svc.accounts = accounts

	for i := 0; i < 3; i++ {
		got, _, err := svc.FilterSignable(context.Background(), "t1", []string{ownerWallet})
		require.NoError(t, err)
		assert.Empty(t, got)
	}
	assert.Equal(t, 1, accounts.calls, "an unshareable owner is asked about once, not once per render")
}

// The credential is resolved once per call, not once per owner. It was per
// owner before, which multiplied a cold render's cost by the credential path on
// top of the accounts-api call.
func TestFilterSignable_ResolvesTheCredentialOncePerCall(t *testing.T) {
	accounts := &fakeAccounts{byWallet: map[string]*gateway.Account{}}
	logger := zerolog.Nop()
	creds := &countingCreds{inner: &fakeCreds{
		minted:    &models.MintedToken{Token: "jwt", ClientID: "0xclient"},
		effective: &EffectiveCredential{TenantID: "t1", ClientID: "0xclient", SignerAddress: tenantSigner},
	}}
	store := testStore(t)
	owners := []string{
		"0x1111111111111111111111111111111111110001",
		"0x1111111111111111111111111111111111110002",
		"0x1111111111111111111111111111111111110003",
	}
	_, _ = store.DBS().Writer.Exec(`DELETE FROM shared_accounts WHERE wallet = ANY($1)`,
		"{"+strings.Join(owners, ",")+"}")
	t.Cleanup(func() {
		_, _ = store.DBS().Writer.Exec(`DELETE FROM shared_accounts WHERE wallet = ANY($1)`,
			"{"+strings.Join(owners, ",")+"}")
	})

	svc := NewSharedSignerService(&logger, accounts, creds, NewSharedAccountStore(store))
	_, _, err := svc.FilterSignable(context.Background(), "t1", owners)
	require.NoError(t, err)

	assert.Equal(t, 1, creds.effectiveCalls, "one effective-credential resolution for the whole fleet")
	assert.Equal(t, 1, creds.mintCalls, "one minted JWT for the whole batch")
}

type countingCreds struct {
	inner          *fakeCreds
	effectiveCalls int
	mintCalls      int
}

func (c *countingCreds) Effective(ctx context.Context, tenantID string) (*EffectiveCredential, error) {
	c.effectiveCalls++
	return c.inner.Effective(ctx, tenantID)
}

func (c *countingCreds) DeveloperJWT(ctx context.Context, tenantID string) (*models.MintedToken, error) {
	c.mintCalls++
	return c.inner.DeveloperJWT(ctx, tenantID)
}
