package service

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"math/big"
	"net/url"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture client id, distinct from every other test file's — the unique index
// on lower(dimo_client_id) makes collisions test-order-dependent.
const aaOpClientID = "0xAAAA00000000000000000000000000000000AA01"

// aaTestWallet is the fixture kernel address. Any address distinct from the
// root EOA works; the chain answers come from the fake.
var aaTestWallet = common.HexToAddress("0xBBBB000000000000000000000000000000000042")

type fakeChain struct {
	chainID  *big.Int
	code     []byte
	owner    common.Address
	chainErr error
	codeErr  error
	callErr  error

	gotCall []byte
	closed  bool
}

func (f *fakeChain) ChainID(context.Context) (*big.Int, error) {
	if f.chainErr != nil {
		return nil, f.chainErr
	}
	return f.chainID, nil
}

func (f *fakeChain) CodeAt(_ context.Context, _ common.Address, _ *big.Int) ([]byte, error) {
	return f.code, f.codeErr
}

func (f *fakeChain) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	f.gotCall = call.Data
	if f.callErr != nil {
		return nil, f.callErr
	}
	return common.LeftPadBytes(f.owner.Bytes(), 32), nil
}

func (f *fakeChain) Close() { f.closed = true }

// aaWalletFixture seeds the standard tenants (operator holding a license, a
// managed customer with no credential row, a parented tenant with its own
// license) and returns a service whose chain answers come from the fake.
func aaWalletFixture(t *testing.T) (*AAWalletService, *fakeChain, *config.Settings, context.Context) {
	t.Helper()
	store := testStore(t)
	seed(t, store)
	w := store.DBS().Writer

	_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE lower(dimo_client_id) = lower($1)`, aaOpClientID)
	_, err := w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id)
		VALUES ($1,$2)
		ON CONFLICT (tenant_id) DO UPDATE SET dimo_client_id = EXCLUDED.dimo_client_id`,
		opTenant, aaOpClientID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = $1`, opTenant)
	})

	settings := &config.Settings{
		TenantSecretEncKey: credEncKey,
		ChainID:            137,
		RPCURL:             url.URL{Scheme: "https", Host: "rpc.test.invalid"},
	}
	chain := &fakeChain{chainID: big.NewInt(137), code: []byte{0x60, 0x80}}
	l := zerolog.Nop()
	svc := NewAAWalletService(&l, store, settings,
		func(context.Context) (ChainReader, error) { return chain, nil })
	return svc, chain, settings, context.Background()
}

func aaTestKey(t *testing.T) (*ecdsa.PrivateKey, string, common.Address) {
	t.Helper()
	pk, err := crypto.GenerateKey()
	require.NoError(t, err)
	return pk, hex.EncodeToString(crypto.FromECDSA(pk)), crypto.PubkeyToAddress(pk.PublicKey)
}

func TestSetAAWalletInputValidation(t *testing.T) {
	svc, _, _, ctx := aaWalletFixture(t)
	pk, keyHex, root := aaTestKey(t)
	_ = pk

	for name, in := range map[string]*models.SetAAWalletInput{
		"missing wallet":         {PrivateKey: keyHex},
		"missing key":            {WalletAddress: aaTestWallet.Hex()},
		"bad address":            {WalletAddress: "not-an-address", PrivateKey: keyHex},
		"bad key":                {WalletAddress: aaTestWallet.Hex(), PrivateKey: "zz"},
		"wallet is the root EOA": {WalletAddress: root.Hex(), PrivateKey: keyHex},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Set(ctx, opTenant, in)
			assert.ErrorIs(t, err, ErrAAWalletInvalid)
		})
	}

	t.Run("unknown tenant", func(t *testing.T) {
		_, err := svc.Set(ctx, "00000000-0000-0000-0000-00000000dead",
			&models.SetAAWalletInput{WalletAddress: aaTestWallet.Hex(), PrivateKey: keyHex})
		assert.ErrorIs(t, err, ErrTenantNotFound)
	})

	t.Run("managed customer holds no license", func(t *testing.T) {
		_, err := svc.Set(ctx, custTenant,
			&models.SetAAWalletInput{WalletAddress: aaTestWallet.Hex(), PrivateKey: keyHex})
		assert.ErrorIs(t, err, ErrAANotCredentialHolder,
			"the wallet is configured where the license lives, or effective resolution can never see it")
	})
}

func TestSetAAWalletChainChecks(t *testing.T) {
	svc, chain, settings, ctx := aaWalletFixture(t)
	_, keyHex, root := aaTestKey(t)
	in := &models.SetAAWalletInput{WalletAddress: aaTestWallet.Hex(), PrivateKey: keyHex}
	chain.owner = root

	t.Run("RPC unconfigured is unavailable, not a verdict", func(t *testing.T) {
		settings.RPCURL = url.URL{}
		defer func() { settings.RPCURL = url.URL{Scheme: "https", Host: "rpc.test.invalid"} }()
		_, err := svc.Set(ctx, opTenant, in)
		assert.ErrorIs(t, err, ErrChainUnavailable)
	})

	t.Run("wrong chain is our config fault, not the wallet's", func(t *testing.T) {
		chain.chainID = big.NewInt(1)
		defer func() { chain.chainID = big.NewInt(137) }()
		_, err := svc.Set(ctx, opTenant, in)
		assert.ErrorIs(t, err, ErrChainUnavailable)
	})

	t.Run("undeployed kernel is refused", func(t *testing.T) {
		chain.code = nil
		defer func() { chain.code = []byte{0x60, 0x80} }()
		_, err := svc.Set(ctx, opTenant, in)
		assert.ErrorIs(t, err, ErrAAWalletInvalid)
		assert.Contains(t, err.Error(), "deployed")
	})

	t.Run("no recorded sudo owner is refused", func(t *testing.T) {
		chain.owner = common.Address{}
		defer func() { chain.owner = root }()
		_, err := svc.Set(ctx, opTenant, in)
		assert.ErrorIs(t, err, ErrAAWalletInvalid)
	})

	t.Run("owner mismatch is refused, naming both addresses", func(t *testing.T) {
		other := common.HexToAddress("0x1111000000000000000000000000000000001111")
		chain.owner = other
		defer func() { chain.owner = root }()
		_, err := svc.Set(ctx, opTenant, in)
		assert.ErrorIs(t, err, ErrAAWalletInvalid)
		assert.Contains(t, err.Error(), other.Hex())
		assert.Contains(t, err.Error(), root.Hex())
	})

	t.Run("nothing persisted by any refusal", func(t *testing.T) {
		got, err := svc.Get(ctx, opTenant)
		require.NoError(t, err)
		assert.False(t, got.Configured)
	})
}

func TestSetAAWalletHappyPath(t *testing.T) {
	svc, chain, settings, ctx := aaWalletFixture(t)
	_, keyHex, root := aaTestKey(t)
	chain.owner = root

	// The pasted form is deliberately messy: 0x prefix and whitespace. What is
	// stored must be canonical regardless.
	status, err := svc.Set(ctx, opTenant, &models.SetAAWalletInput{
		WalletAddress: "0xbbbb000000000000000000000000000000000042", // lowercase in
		PrivateKey:    " 0x" + keyHex + "\n",
	})
	require.NoError(t, err)
	assert.True(t, status.Configured)
	assert.Equal(t, aaTestWallet.Hex(), status.WalletAddress, "stored EIP-55 checksummed")
	assert.Equal(t, opTenant, status.CredentialTenantID)

	t.Run("the validator was asked about this kernel", func(t *testing.T) {
		require.NotEmpty(t, chain.gotCall)
		assert.Equal(t, ecdsaValidatorStorageSelector, chain.gotCall[:4])
		assert.Equal(t, common.LeftPadBytes(aaTestWallet.Bytes(), 32), chain.gotCall[4:])
	})

	t.Run("the stored key round-trips through the signer, canonically", func(t *testing.T) {
		l := zerolog.Nop()
		creds := NewCredentialService(&l, svc.pdb, settings, &fakeIdentity{uri: "https://example.com"})
		wallet, pk, err := creds.AAWalletSigner(ctx, opTenant)
		require.NoError(t, err)
		assert.Equal(t, aaTestWallet, wallet)
		assert.Equal(t, keyHex, hex.EncodeToString(crypto.FromECDSA(pk)))
	})

	t.Run("a managed customer inherits through effective resolution", func(t *testing.T) {
		got, err := svc.Get(ctx, custTenant)
		require.NoError(t, err)
		assert.True(t, got.Configured)
		assert.Equal(t, aaTestWallet.Hex(), got.WalletAddress)
		assert.Equal(t, opTenant, got.CredentialTenantID, "inherited, and the holder is named")

		l := zerolog.Nop()
		creds := NewCredentialService(&l, svc.pdb, settings, &fakeIdentity{uri: "https://example.com"})
		wallet, _, err := creds.AAWalletSigner(ctx, custTenant)
		require.NoError(t, err)
		assert.Equal(t, aaTestWallet, wallet, "the signer resolves the same way the readback does")
	})

	t.Run("no wallet on the effective credential is ErrNoAAWallet", func(t *testing.T) {
		require.NoError(t, svc.Clear(ctx, opTenant))
		l := zerolog.Nop()
		creds := NewCredentialService(&l, svc.pdb, settings, &fakeIdentity{uri: "https://example.com"})
		_, _, err := creds.AAWalletSigner(ctx, custTenant)
		assert.ErrorIs(t, err, ErrNoAAWallet)
	})

	t.Run("clear is idempotent", func(t *testing.T) {
		require.NoError(t, svc.Clear(ctx, opTenant))
		got, err := svc.Get(ctx, opTenant)
		require.NoError(t, err)
		assert.False(t, got.Configured)
		assert.Equal(t, opTenant, got.CredentialTenantID,
			"the credential still resolves; only the wallet is gone")
	})
}

func TestSetAAWalletDialFailure(t *testing.T) {
	svc, _, _, ctx := aaWalletFixture(t)
	_, keyHex, _ := aaTestKey(t)
	svc.dial = func(context.Context) (ChainReader, error) { return nil, errors.New("boom") }
	_, err := svc.Set(ctx, opTenant,
		&models.SetAAWalletInput{WalletAddress: aaTestWallet.Hex(), PrivateKey: keyHex})
	assert.ErrorIs(t, err, ErrChainUnavailable)
}
