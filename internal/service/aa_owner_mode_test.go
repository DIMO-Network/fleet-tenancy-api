package service

import (
	"context"
	"encoding/hex"
	"net/url"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ownerModeSettings is a fully sharing-configured Settings with the AA project
// URL set — the state in which OwnerModeConfigured is true.
func ownerModeSettings() *config.Settings {
	return &config.Settings{
		TenantSecretEncKey:  credEncKey,
		ChainID:             137,
		SacdAddress:         "0x3c152B5d96769661008Ff404224d6530FCAC766d",
		SyntheticNftAddress: "0x4804e8D1661cd1a1e5dDdE1ff458A7f878c0aC6D",
		VehicleNftAddress:   "0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF",
		RPCURL:              url.URL{Scheme: "https", Host: "rpc.test.invalid"},
		BundlerURL:          url.URL{Scheme: "https", Host: "bundler.test.invalid"},
	}
}

// AuthorizeShare's mode selection, end to end against the database: the owner
// comparison, the sudo-key decrypt, the skip of MaySignFor, and each
// fall-through to the signer path. The accounts fake is EMPTY throughout —
// every owner it is asked about is a denial — which is what proves owner mode
// never asks it: a share that succeeds here cannot have consulted accounts-api.
func TestAuthorizeShareOwnerMode(t *testing.T) {
	store := testStore(t)
	seed(t, store)
	w := store.DBS().Writer

	aaPK, err := crypto.GenerateKey()
	require.NoError(t, err)
	aaKeyHex := hex.EncodeToString(crypto.FromECDSA(aaPK))
	aaWallet := common.HexToAddress("0xBBBB0000000000000000000000000000000000AA")
	keyEnc, err := EncryptSecret(credEncKey, aaKeyHex)
	require.NoError(t, err)

	_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = $1`, opTenant)
	_, err = w.Exec(`INSERT INTO tenant_credentials
		(tenant_id, dimo_client_id, aa_wallet_address, aa_wallet_key_enc)
		VALUES ($1, '0xAAAA00000000000000000000000000000000AB01', $2, $3)`,
		opTenant, aaWallet.Hex(), keyEnc)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = $1`, opTenant)
	})

	const tokenID = int64(4242)
	settings := ownerModeSettings()
	logger := zerolog.Nop()
	identity := &fakeIdentity{owners: map[int64]string{tokenID: aaWallet.Hex()}}
	creds := NewCredentialService(&logger, store, settings, identity)
	signer := NewSharedSignerService(&logger, &fakeAccounts{}, creds,
		NewSharedAccountStore(store), settings)
	authorizer := NewShareAuthorizer(&logger, store, identity, signer, creds, settings)
	ctx := context.Background()

	t.Run("owner == AA wallet selects owner mode with the root key", func(t *testing.T) {
		owner, pk, ownerMode, aerr := authorizer.AuthorizeShare(ctx, opTenant, tokenID)
		require.NoError(t, aerr, "MaySignFor must not have been consulted — the accounts fake denies everyone")
		assert.True(t, ownerMode)
		assert.Equal(t, aaWallet, owner)
		require.NotNil(t, pk)
		assert.Equal(t, aaKeyHex, hex.EncodeToString(crypto.FromECDSA(pk)),
			"the key is the wallet's root, not the tenant signer")
	})

	t.Run("a managed customer inherits owner mode through the effective credential", func(t *testing.T) {
		// custTenant is explicit-mode, so the entitlement gate applies; give it
		// the row the share needs.
		_, serr := w.Exec(`INSERT INTO vehicle_entitlements (tenant_id, vehicle_token_id, source)
			VALUES ($1, $2, 'operator') ON CONFLICT DO NOTHING`, custTenant, tokenID)
		require.NoError(t, serr)
		t.Cleanup(func() {
			_, _ = w.Exec(`DELETE FROM vehicle_entitlements WHERE tenant_id = $1 AND vehicle_token_id = $2`,
				custTenant, tokenID)
		})

		owner, _, ownerMode, aerr := authorizer.AuthorizeShare(ctx, custTenant, tokenID)
		require.NoError(t, aerr)
		assert.True(t, ownerMode)
		assert.Equal(t, aaWallet, owner)
	})

	t.Run("a different owner falls through to the signer path", func(t *testing.T) {
		identity.owners[tokenID] = "0xBbbBBB0000000000000000000000000000000002"
		defer func() { identity.owners[tokenID] = aaWallet.Hex() }()

		_, _, _, aerr := authorizer.AuthorizeShare(ctx, opTenant, tokenID)
		assert.ErrorIs(t, aerr, ErrSignerNotAuthorized,
			"the empty accounts fake denies, proving the signer path ran")
	})

	t.Run("owner mode unconfigured falls through even for the AA wallet's own vehicle", func(t *testing.T) {
		off := ownerModeSettings()
		off.BundlerURL = url.URL{}
		offSigner := NewSharedSignerService(&logger, &fakeAccounts{}, creds,
			NewSharedAccountStore(store), off)
		offAuthorizer := NewShareAuthorizer(&logger, store, identity, offSigner, creds, off)

		_, _, _, aerr := offAuthorizer.AuthorizeShare(ctx, opTenant, tokenID)
		assert.ErrorIs(t, aerr, ErrSignerNotAuthorized,
			"with sharing unconfigured the wallet must not be selected for owner mode — "+
				"off means off, never a wrong-validator attempt")
	})
}

// FilterSignable's AA-wallet half: the display gate that makes the share
// button light up for owner-mode vehicles.
func TestFilterSignableOwnerMode(t *testing.T) {
	logger := zerolog.Nop()
	aaWallet := common.HexToAddress("0xBBBB0000000000000000000000000000000000AA")
	build := func(settings *config.Settings, signerAddr string) (*SharedSignerService, *countingAccounts) {
		accounts := &countingAccounts{inner: &fakeAccounts{}}
		creds := &fakeCreds{
			minted: &models.MintedToken{Token: "jwt", ClientID: "0xclient"},
			effective: &EffectiveCredential{TenantID: "t1", ClientID: "0xclient",
				SignerAddress: signerAddr, AAWalletAddress: aaWallet.Hex()},
		}
		return NewSharedSignerService(&logger, accounts, creds,
			NewSharedAccountStore(testStore(t)), settings), accounts
	}

	t.Run("the AA wallet is positive with no accounts-api call, even signerless", func(t *testing.T) {
		svc, accounts := build(ownerModeSettings(), "")
		got, unresolved, ownerModeWallet, err := svc.FilterSignable(context.Background(), "t1",
			[]string{lower(aaWallet.Hex()), ownerWallet})
		require.NoError(t, err)
		assert.Equal(t, []string{aaWallet.Hex()}, got,
			"the tenant's own wallet is shareable; the stranger, with no signer to compare, is a denial")
		assert.Empty(t, unresolved, "a denial for lack of a signer is an answer, not an unknown")
		assert.Equal(t, aaWallet.Hex(), ownerModeWallet)
		assert.Zero(t, accounts.calls, "configuration answered everything; accounts-api was never asked")
	})

	t.Run("owner mode unconfigured hides the wallet from the display gate too", func(t *testing.T) {
		off := ownerModeSettings()
		off.BundlerURL = url.URL{}
		svc, _ := build(off, "")
		got, _, ownerModeWallet, err := svc.FilterSignable(context.Background(), "t1",
			[]string{aaWallet.Hex()})
		require.NoError(t, err)
		assert.Empty(t, got,
			"a button the execution path would refuse must not light up — one switch, three surfaces")
		assert.Empty(t, ownerModeWallet)
	})
}
