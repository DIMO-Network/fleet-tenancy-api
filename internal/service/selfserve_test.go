package service

import (
	"context"
	"errors"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeValidator struct {
	err   error
	calls int
}

func (f *fakeValidator) ValidateCredential(_, _ string) error {
	f.calls++
	return f.err
}

const (
	ssOwner    = "0x6666666666666666666666666666666666666666"
	ssClientA  = "0xDDDD000000000000000000000000000000000001"
	ssClientB  = "0xDDDD000000000000000000000000000000000002"
	ssTestName = "SelfServe Fixture"
)

func newSelfServe(t *testing.T) (*SelfServeService, *fakeValidator, context.Context) {
	t.Helper()
	store := testStore(t)
	w := store.DBS().Writer
	// Clean by the fixture client ids and name — never touch other rows.
	_, _ = w.Exec(`DELETE FROM memberships WHERE tenant_id IN
		(SELECT id FROM tenants WHERE name LIKE 'SelfServe %')`)
	_, _ = w.Exec(`DELETE FROM tenant_credentials WHERE lower(dimo_client_id) IN (lower($1), lower($2))
		OR tenant_id IN (SELECT id FROM tenants WHERE name LIKE 'SelfServe %')`, ssClientA, ssClientB)
	_, _ = w.Exec(`DELETE FROM tenants WHERE name LIKE 'SelfServe %'`)
	_, _ = w.Exec(`DELETE FROM users WHERE wallet = $1`, ssOwner)

	l := zerolog.Nop()
	tenants := NewTenantService(&l, store)
	v := &fakeValidator{}
	svc := NewSelfServeService(&l, store, &config.Settings{TenantSecretEncKey: "test-enc-key"}, tenants, v)
	return svc, v, context.Background()
}

func TestSelfServeCreate(t *testing.T) {
	svc, v, ctx := newSelfServe(t)

	created, err := svc.Create(ctx, &models.CreateSelfServeInput{
		Name: ssTestName, ClientID: ssClientA, APIKey: "aa11", OwnerWallet: ssOwner,
		OwnerEmail: "owner@example.com",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	assert.Equal(t, 1, v.calls, "the credential must be proven before anything persists")

	t.Run("the tenant is unparented, unmanaged and implicit", func(t *testing.T) {
		assert.Equal(t, models.KindCustomer, created.Kind)
		assert.Nil(t, created.ParentTenantID)
		assert.False(t, created.Managed)
		assert.Equal(t, models.EntitlementImplicit, created.EntitlementMode)
		assert.True(t, created.FleetLiteEnabled)
	})

	t.Run("the owner membership answers authz immediately", func(t *testing.T) {
		l := zerolog.Nop()
		authz := NewAuthzService(&l, svc.pdb)
		got, err := authz.Authorize(ctx, created.ID, ssOwner)
		require.NoError(t, err)
		assert.True(t, got.Member)
		assert.Equal(t, "owner", got.Role)
		assert.True(t, got.HasCapability("manage_members"))
		assert.True(t, got.HasCapability("manage_settings"))
		assert.True(t, got.Unrestricted())
	})

	t.Run("the stored key decrypts under the service key", func(t *testing.T) {
		var enc string
		require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
			`SELECT dimo_api_key_enc FROM tenant_credentials WHERE tenant_id = $1`,
			created.ID).Scan(&enc))
		dec, derr := DecryptSecret("test-enc-key", enc)
		require.NoError(t, derr)
		assert.Equal(t, "aa11", dec)
	})

	t.Run("a second tenant on the same license is refused", func(t *testing.T) {
		_, err := svc.Create(ctx, &models.CreateSelfServeInput{
			Name: "SelfServe Duplicate", ClientID: ssClientA, APIKey: "bb22", OwnerWallet: ssOwner,
		})
		assert.True(t, errors.Is(err, ErrClientIDRegistered), "got %v", err)

		// The refusal must leave nothing behind: a tenant without its
		// credential would be exactly the orphan the transaction exists to
		// prevent.
		var n int
		require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
			`SELECT count(*) FROM tenants WHERE name = 'SelfServe Duplicate'`).Scan(&n))
		assert.Zero(t, n)
	})

	t.Run("an invalid credential persists nothing", func(t *testing.T) {
		v.err = errors.New("mint failed")
		_, err := svc.Create(ctx, &models.CreateSelfServeInput{
			Name: "SelfServe Invalid", ClientID: ssClientB, APIKey: "cc33", OwnerWallet: ssOwner,
		})
		assert.True(t, errors.Is(err, ErrCredentialInvalid), "got %v", err)
		v.err = nil

		var n int
		require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
			`SELECT count(*) FROM tenants WHERE name = 'SelfServe Invalid'`).Scan(&n))
		assert.Zero(t, n)
	})

	t.Run("missing fields are caller errors", func(t *testing.T) {
		for _, in := range []*models.CreateSelfServeInput{
			{ClientID: ssClientB, APIKey: "x", OwnerWallet: ssOwner},
			{Name: "SelfServe X", APIKey: "x", OwnerWallet: ssOwner},
			{Name: "SelfServe X", ClientID: ssClientB, OwnerWallet: ssOwner},
			{Name: "SelfServe X", ClientID: ssClientB, APIKey: "x"},
		} {
			_, err := svc.Create(ctx, in)
			assert.Error(t, err)
		}
	})

}

func TestSelfServeSetCredentials(t *testing.T) {
	svc, v, ctx := newSelfServe(t)

	created, err := svc.Create(ctx, &models.CreateSelfServeInput{
		Name: ssTestName, ClientID: ssClientA, APIKey: "aa11", OwnerWallet: ssOwner,
	})
	require.NoError(t, err)

	t.Run("rotation replaces the key in place", func(t *testing.T) {
		require.NoError(t, svc.SetCredentials(ctx, created.ID,
			&models.SetCredentialsInput{ClientID: ssClientA, APIKey: "rotated"}))
		var enc string
		require.NoError(t, svc.pdb.DBS().Reader.QueryRow(
			`SELECT dimo_api_key_enc FROM tenant_credentials WHERE tenant_id = $1`,
			created.ID).Scan(&enc))
		dec, derr := DecryptSecret("test-enc-key", enc)
		require.NoError(t, derr)
		assert.Equal(t, "rotated", dec)
	})

	t.Run("a license held by another tenant is refused", func(t *testing.T) {
		other, err := svc.Create(ctx, &models.CreateSelfServeInput{
			Name: "SelfServe Other", ClientID: ssClientB, APIKey: "bb22", OwnerWallet: ssOwner,
		})
		require.NoError(t, err)
		err = svc.SetCredentials(ctx, other.ID,
			&models.SetCredentialsInput{ClientID: ssClientA, APIKey: "zz"})
		assert.True(t, errors.Is(err, ErrClientIDRegistered), "got %v", err)
	})

	t.Run("an unknown tenant is not found", func(t *testing.T) {
		err := svc.SetCredentials(ctx, "dddddddd-0000-0000-0000-000000000009",
			&models.SetCredentialsInput{ClientID: ssClientB, APIKey: "zz"})
		assert.True(t, errors.Is(err, ErrTenantNotFound), "got %v", err)
	})

	t.Run("an invalid credential changes nothing", func(t *testing.T) {
		v.err = errors.New("mint failed")
		err := svc.SetCredentials(ctx, created.ID,
			&models.SetCredentialsInput{ClientID: ssClientA, APIKey: "bad"})
		assert.True(t, errors.Is(err, ErrCredentialInvalid), "got %v", err)
		v.err = nil
	})
}
