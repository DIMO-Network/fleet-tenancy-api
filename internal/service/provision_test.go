package service

import (
	"context"
	"strings"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const provisionedWallet = "0x6666666666666666666666666666666666666666"

type fakeCreds struct {
	minted    *models.MintedToken
	mintErr   error
	effective *EffectiveCredential
	effErr    error
}

func (f *fakeCreds) DeveloperJWT(context.Context, string) (*models.MintedToken, error) {
	return f.minted, f.mintErr
}
func (f *fakeCreds) Effective(context.Context, string) (*EffectiveCredential, error) {
	return f.effective, f.effErr
}

type fakeAccounts struct {
	getAcct    *gateway.Account
	getErr     error
	created    *gateway.Account
	createErr  error
	getCalls   int
	createArgs []string // email, signer, jwt of the last CreateAccount call

	// By-wallet lookups belong to vehicle sharing, not provisioning. Kept on
	// the same fake so there is one AccountsAPI stand-in, but provisioning
	// never reaches them.
	byWallet    map[string]*gateway.Account
	byWalletErr error
}

func (f *fakeAccounts) GetAccountByEmail(email, jwt string) (*gateway.Account, error) {
	f.getCalls++
	return f.getAcct, f.getErr
}
func (f *fakeAccounts) CreateAccount(email, signer, jwt string) (*gateway.Account, error) {
	f.createArgs = []string{email, signer, jwt}
	return f.created, f.createErr
}
func (f *fakeAccounts) GetAccountByWallet(wallet, _ string) (*gateway.Account, error) {
	if f.byWalletErr != nil {
		return nil, f.byWalletErr
	}
	acct, ok := f.byWallet[strings.ToLower(wallet)]
	if !ok {
		return nil, gateway.ErrAccountNotFound
	}
	return acct, nil
}

func provisionFixture(t *testing.T, creds *fakeCreds, accounts *fakeAccounts) (*ProvisionService, *AuthzService, context.Context) {
	t.Helper()
	store := testStore(t)
	seed(t, store)
	l := zerolog.Nop()

	t.Cleanup(func() {
		w := store.DBS().Writer
		_, _ = w.Exec(`DELETE FROM memberships WHERE wallet = $1`, provisionedWallet)
		_, _ = w.Exec(`DELETE FROM users WHERE wallet = $1`, provisionedWallet)
	})

	return NewProvisionService(&l, store, NewMemberService(&l, store), creds, accounts),
		NewAuthzService(&l, store), context.Background()
}

func TestProvision(t *testing.T) {
	token := &models.MintedToken{Token: "jwt-abc", ClientID: "0xCCCC000000000000000000000000000000000001"}
	cred := &EffectiveCredential{TenantID: opTenant, ClientID: token.ClientID,
		SignerAddress: "0x9999000000000000000000000000000000000009"}

	t.Run("existing account becomes a membership", func(t *testing.T) {
		accounts := &fakeAccounts{getAcct: &gateway.Account{WalletAddress: provisionedWallet}}
		svc, authz, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)

		res, err := svc.Provision(ctx, custTenant, &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			Email:             "person@example.com",
			Role:              "member",
			Permissions:       []string{models.CapReports},
			ScopeGroupIDs:     scope(`[]`),
			GrantedByTenantID: opTenant,
		}})
		require.NoError(t, err)
		assert.Equal(t, provisionedWallet, res.Member.Wallet)
		assert.False(t, res.Created)
		// The response is the written membership as the member list would show
		// it, so the console renders the new row without a second call.
		assert.Equal(t, "member", res.Member.Role)
		assert.Equal(t, []string{models.CapReports}, res.Member.Permissions)
		require.NotNil(t, res.Member.ScopeGroupIDs)
		assert.Empty(t, res.Member.ScopeGroupIDs, "empty scope survives the round trip as empty, not null")
		require.NotNil(t, res.Member.GrantedByTenantID)
		assert.Equal(t, opTenant, *res.Member.GrantedByTenantID)
		require.NotNil(t, res.Member.Email)
		assert.Equal(t, "person@example.com", *res.Member.Email)

		got, err := authz.Authorize(ctx, custTenant, provisionedWallet)
		require.NoError(t, err)
		assert.True(t, got.Member)
		assert.True(t, got.HasCapability(models.CapReports))
		assert.False(t, got.Unrestricted(), "empty scope stays empty through provisioning")
	})

	t.Run("missing account is created with the effective signer", func(t *testing.T) {
		accounts := &fakeAccounts{
			getErr:  gateway.ErrAccountNotFound,
			created: &gateway.Account{WalletAddress: provisionedWallet},
		}
		svc, _, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)

		res, err := svc.Provision(ctx, custTenant, &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			Email: "new@example.com", Permissions: []string{}, ScopeGroupIDs: scope(`null`),
		}})
		require.NoError(t, err)
		assert.True(t, res.Created)
		require.Equal(t, []string{"new@example.com", cred.SignerAddress, token.Token}, accounts.createArgs,
			"created under the effective credential's signer and JWT")

		// The signer registered on the account is recorded on the user.
		var signer string
		err = svc.pdb.DBS().Reader.QueryRow(
			`SELECT COALESCE(shared_account_signer_address,'') FROM users WHERE wallet=$1`,
			provisionedWallet).Scan(&signer)
		require.NoError(t, err)
		assert.Equal(t, cred.SignerAddress, signer)
	})

	t.Run("no signer on the credential refuses to create", func(t *testing.T) {
		accounts := &fakeAccounts{getErr: gateway.ErrAccountNotFound}
		noSigner := &EffectiveCredential{TenantID: opTenant, ClientID: cred.ClientID}
		svc, _, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: noSigner}, accounts)

		_, err := svc.Provision(ctx, custTenant, &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			Email: "new@example.com", ScopeGroupIDs: scope(`null`),
		}})
		assert.ErrorIs(t, err, ErrNoSignerAddress)
	})

	t.Run("an account without a wallet is refused, not keyed on email", func(t *testing.T) {
		accounts := &fakeAccounts{getAcct: &gateway.Account{}}
		svc, _, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)

		_, err := svc.Provision(ctx, custTenant, &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			Email: "person@example.com", ScopeGroupIDs: scope(`null`),
		}})
		assert.ErrorIs(t, err, ErrUpstream)
	})

	t.Run("validation runs before any accounts-api call", func(t *testing.T) {
		accounts := &fakeAccounts{}
		svc, _, ctx := provisionFixture(t, &fakeCreds{minted: token, effective: cred}, accounts)

		_, err := svc.Provision(ctx, custTenant, &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			ScopeGroupIDs: scope(`null`),
		}})
		require.Error(t, err, "email is required")

		_, err = svc.Provision(ctx, custTenant, &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			Email: "person@example.com",
		}})
		require.Error(t, err, "scope is required")

		assert.Zero(t, accounts.getCalls,
			"a bad request must not create a DIMO account as a side effect")
	})

	t.Run("a mint failure surfaces as upstream, and nothing is written", func(t *testing.T) {
		accounts := &fakeAccounts{}
		svc, authz, ctx := provisionFixture(t,
			&fakeCreds{mintErr: assert.AnError}, accounts)

		_, err := svc.Provision(ctx, custTenant, &models.ProvisionRequest{MemberWrite: models.MemberWrite{
			Email: "person@example.com", ScopeGroupIDs: scope(`null`),
		}})
		assert.ErrorIs(t, err, ErrUpstream)

		got, err := authz.Authorize(ctx, custTenant, provisionedWallet)
		require.NoError(t, err)
		assert.False(t, got.Member)
	})
}
