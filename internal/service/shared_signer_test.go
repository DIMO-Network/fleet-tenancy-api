package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
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
	return NewSharedSignerService(&logger, accounts, creds)
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

	got, err := svc.FilterSignable(context.Background(), "t1",
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
	got, err := svc.FilterSignable(context.Background(), "t1", owners)
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
		_, err := svc.FilterSignable(context.Background(), "t1", []string{ownerWallet})
		require.NoError(t, err)
	}
	assert.Equal(t, 1, accounts.calls, "the answer should be cached across calls")
}

// An upstream failure fails the whole call rather than quietly shortening the
// list. A partial answer would hide share buttons during a blip and be
// indistinguishable from the feature being off.
func TestFilterSignable_UpstreamFailureFailsTheCall(t *testing.T) {
	svc := signerFixture(t, &fakeAccounts{byWalletErr: errors.New("boom")}, tenantSigner)

	_, err := svc.FilterSignable(context.Background(), "t1", []string{ownerWallet})
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
