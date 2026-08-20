package service

import (
	"context"
	"net/url"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture client ids, distinct from anything the resolve tests use — the
// unique index on lower(dimo_client_id) makes collisions test-order-dependent.
const (
	credOpClientID   = "0xCCCC000000000000000000000000000000000001"
	credCustClientID = "0xCCCC000000000000000000000000000000000002"
	credEncKey       = "test-enc-key"
)

type fakeIdentity struct {
	uri string
	err error

	// Vehicle ownership belongs to sharing, not the credential service. Kept
	// on the same fake so there is one IdentityAPI stand-in.
	owners    map[int64]string
	ownersErr error

	// The roster sweep's bulk read, keyed by client id. Same reasoning: one
	// stand-in for the whole IdentityAPI surface.
	privileged    map[string][]gateway.RosterVehicle
	privilegedErr error

	// Single-vehicle lookups, for the roster's entitled-gap fill.
	details   map[int64]gateway.RosterVehicle
	detailErr error
}

func (f *fakeIdentity) RedirectURIForClientID(string) (string, error) { return f.uri, f.err }
func (f *fakeIdentity) VehicleOwner(tokenID int64) (string, error) {
	if f.ownersErr != nil {
		return "", f.ownersErr
	}
	owner, ok := f.owners[tokenID]
	if !ok {
		return "", gateway.ErrVehicleNotFound
	}
	return owner, nil
}

func (f *fakeIdentity) PrivilegedVehicles(clientID string) ([]gateway.RosterVehicle, error) {
	if f.privilegedErr != nil {
		return nil, f.privilegedErr
	}
	return f.privileged[clientID], nil
}

func (f *fakeIdentity) VehicleDetail(tokenID int64) (*gateway.RosterVehicle, error) {
	if f.detailErr != nil {
		return nil, f.detailErr
	}
	v, ok := f.details[tokenID]
	if !ok {
		return nil, gateway.ErrVehicleNotFound
	}
	return &v, nil
}

func credService(t *testing.T, settings *config.Settings) (*CredentialService, context.Context) {
	t.Helper()
	store := testStore(t)
	seed(t, store)
	w := store.DBS().Writer

	// The operator holds a license; custTenant is managed (no credential row);
	// otherTen holds its own despite being parented.
	_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = ANY($1)`,
		"{"+opTenant+","+custTenant+","+otherTen+"}")
	opKeyEnc, err := EncryptSecret(credEncKey, "aaaa000000000000000000000000000000000000000000000000000000000001")
	require.NoError(t, err)
	_, err = w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id, dimo_api_key_enc, signer_address)
		VALUES ($1,$2,$3,'0x9999000000000000000000000000000000000009')`,
		opTenant, credOpClientID, opKeyEnc)
	require.NoError(t, err)
	_, err = w.Exec(`INSERT INTO tenant_credentials (tenant_id, dimo_client_id)
		VALUES ($1,$2)`, otherTen, credCustClientID)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = ANY($1)`,
			"{"+opTenant+","+custTenant+","+otherTen+"}")
	})

	if settings == nil {
		settings = &config.Settings{TenantSecretEncKey: credEncKey}
	}
	l := zerolog.Nop()
	return NewCredentialService(&l, store, settings, &fakeIdentity{uri: "https://example.com"}), context.Background()
}

// The resolution rule in one place: own if present, otherwise the parent's.
// This is the same rule CallerMayAccess enforces from the other side, and the
// tests spell out each branch so a future change to one query has a named
// failure in the other's terms.
func TestEffectiveCredential(t *testing.T) {
	svc, ctx := credService(t, nil)

	t.Run("a tenant with its own credential resolves to itself", func(t *testing.T) {
		got, err := svc.Effective(ctx, opTenant)
		require.NoError(t, err)
		assert.Equal(t, opTenant, got.TenantID)
		assert.Equal(t, credOpClientID, got.ClientID)
		assert.Equal(t, "0x9999000000000000000000000000000000000009", got.SignerAddress)
	})

	t.Run("a managed customer resolves to its parent's", func(t *testing.T) {
		got, err := svc.Effective(ctx, custTenant)
		require.NoError(t, err)
		assert.Equal(t, opTenant, got.TenantID, "the operator holds the effective credential")
		assert.Equal(t, credOpClientID, got.ClientID)
	})

	t.Run("a parented tenant with its own credential keeps its own", func(t *testing.T) {
		got, err := svc.Effective(ctx, otherTen)
		require.NoError(t, err)
		assert.Equal(t, otherTen, got.TenantID,
			"own credential wins over the parent's — the ORDER BY, not chance")
		assert.Equal(t, credCustClientID, got.ClientID)
	})

	t.Run("no credential anywhere is ErrNoCredential, not not-found", func(t *testing.T) {
		_, err := svc.pdb.DBS().Writer.Exec(`DELETE FROM tenant_credentials WHERE tenant_id = $1`, opTenant)
		require.NoError(t, err)
		_, err = svc.Effective(ctx, custTenant)
		assert.ErrorIs(t, err, ErrNoCredential)
	})

	t.Run("an unknown tenant is ErrTenantNotFound", func(t *testing.T) {
		_, err := svc.Effective(ctx, "aaaaaaaa-dead-dead-dead-000000000009")
		assert.ErrorIs(t, err, ErrTenantNotFound)
	})
}

func TestDeveloperJWTGuards(t *testing.T) {
	t.Run("refuses without DIMO_AUTH_URL rather than minting into nowhere", func(t *testing.T) {
		svc, ctx := credService(t, &config.Settings{TenantSecretEncKey: credEncKey})
		_, err := svc.DeveloperJWT(ctx, opTenant)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DIMO_AUTH_URL")
	})

	t.Run("a credential without a stored key is ErrNoCredential", func(t *testing.T) {
		authURL, _ := url.Parse("https://auth.example.com")
		svc, ctx := credService(t, &config.Settings{TenantSecretEncKey: credEncKey, DimoAuthURL: *authURL})
		// otherTen's row has a client id but no dimo_api_key_enc.
		_, err := svc.DeveloperJWT(ctx, otherTen)
		assert.ErrorIs(t, err, ErrNoCredential)
	})

	t.Run("a wrong master key fails loudly, not with garbage", func(t *testing.T) {
		authURL, _ := url.Parse("https://auth.example.com")
		svc, ctx := credService(t, &config.Settings{TenantSecretEncKey: "not-the-key", DimoAuthURL: *authURL})
		_, err := svc.DeveloperJWT(ctx, opTenant)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decrypt", "GCM authentication catches the wrong key")
	})
}
