package service

import (
	"context"
	"strings"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signerProvisionFixture(t *testing.T) (*CredentialService, context.Context) {
	t.Helper()
	store := testStore(t)
	seed(t, store)
	// otherTen has no credential row at all in the shared fixture, which is
	// the provisioning starting state.
	_, _ = store.DBS().Writer.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = $1`, otherTen)
	t.Cleanup(func() {
		_, _ = store.DBS().Writer.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = $1`, otherTen)
	})
	l := zerolog.Nop()
	svc := NewCredentialService(&l, store, &config.Settings{TenantSecretEncKey: "test-master-key"}, nil)
	return svc, context.Background()
}

func TestProvisionSignerCreatesOnce(t *testing.T) {
	svc, ctx := signerProvisionFixture(t)

	res, err := svc.ProvisionSigner(ctx, otherTen)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.SignerAddress, "0x"))
	assert.Len(t, res.SignerAddress, 42)

	// The second run must refuse, and must not have touched the first key.
	_, err = svc.ProvisionSigner(ctx, otherTen)
	require.ErrorIs(t, err, ErrSignerExists)

	var addr string
	require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
		`SELECT signer_address FROM tenant_credentials WHERE tenant_id = $1`,
		otherTen).Scan(&addr))
	assert.Equal(t, res.SignerAddress, addr, "a refused run must leave the row untouched")
}

// The property everything downstream depends on: the stored key decrypts,
// parses the way sharing.go parses (no 0x trim there), and derives the stored
// address. If this holds at provisioning time, signer-diff's cross-checks hold
// for the key's whole life, because nothing else ever writes it.
func TestProvisionedKeyRoundTripsToItsAddress(t *testing.T) {
	svc, ctx := signerProvisionFixture(t)

	res, err := svc.ProvisionSigner(ctx, otherTen)
	require.NoError(t, err)

	var keyEnc string
	require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
		`SELECT signer_key_enc FROM tenant_credentials WHERE tenant_id = $1`,
		otherTen).Scan(&keyEnc))

	plain, err := DecryptSecret("test-master-key", keyEnc)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(plain, "0x"),
		"sharing.go's HexToECDSA does not trim a prefix, so none may be stored")

	pk, err := crypto.HexToECDSA(plain)
	require.NoError(t, err)
	assert.Equal(t, res.SignerAddress, crypto.PubkeyToAddress(pk.PublicKey).Hex())
}

func TestProvisionSignerFillsAnEmptyRowInPlace(t *testing.T) {
	svc, ctx := signerProvisionFixture(t)

	// A row that exists with client credentials but no signer — the managed
	// path's shape — must be fillable in place.
	_, err := svc.pdb.DBS().Writer.Exec(`
		INSERT INTO tenant_credentials (tenant_id, dimo_client_id) VALUES ($1, 'client-x')`,
		otherTen)
	require.NoError(t, err)

	res, err := svc.ProvisionSigner(ctx, otherTen)
	require.NoError(t, err)
	assert.False(t, res.EffectiveUnused, "the row holds a client id, so the signer is live")

	var clientID string
	require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
		`SELECT dimo_client_id FROM tenant_credentials WHERE tenant_id = $1`,
		otherTen).Scan(&clientID))
	assert.Equal(t, "client-x", clientID, "filling the signer must not disturb the credential")
}

func TestProvisionSignerFlagsAnInertSigner(t *testing.T) {
	svc, ctx := signerProvisionFixture(t)

	res, err := svc.ProvisionSigner(ctx, otherTen)
	require.NoError(t, err)
	assert.True(t, res.EffectiveUnused,
		"no dimo_client_id on the row means the effective credential resolves elsewhere")
}
