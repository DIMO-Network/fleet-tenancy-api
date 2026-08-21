package service

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each rejection here is a share that would otherwise succeed on-chain and read
// as working in the UI while granting nothing useful.
func TestValidateGrantee(t *testing.T) {
	owner := common.HexToAddress("0x1111111111111111111111111111111111111111")
	other := common.HexToAddress("0x2222222222222222222222222222222222222222")

	t.Run("a normal wallet is fine", func(t *testing.T) {
		assert.NoError(t, ValidateGrantee(other.Hex(), owner))
	})

	t.Run("lowercase is fine — callers paste addresses in any casing", func(t *testing.T) {
		assert.NoError(t, ValidateGrantee("0x2222222222222222222222222222222222222222", owner))
	})

	// A bare 40-character hex string with no 0x prefix is accepted, because
	// HexToAddress resolves it to exactly the same address. Documented as
	// deliberate leniency rather than left to be discovered — rejecting it
	// would only produce spurious failures for a caller that trimmed a prefix.
	t.Run("a bare hex string without 0x is accepted", func(t *testing.T) {
		assert.NoError(t, ValidateGrantee("2222222222222222222222222222222222222222", owner))
	})

	for name, grantee := range map[string]string{
		"empty":            "",
		"not hex":          "definitely-not-an-address",
		"too short":        "0x2222",
		"too long":         "0x22222222222222222222222222222222222222223",
		"an ENS-like name": "alice.eth",
	} {
		t.Run(name+" is rejected", func(t *testing.T) {
			assert.ErrorIs(t, ValidateGrantee(grantee, owner), ErrGranteeInvalid)
		})
	}

	// Granting to the zero address burns the permission into nothing while
	// looking, in the UI, exactly like a share that worked.
	t.Run("the zero address is rejected", func(t *testing.T) {
		assert.ErrorIs(t, ValidateGrantee(common.Address{}.Hex(), owner), ErrGranteeInvalid)
	})

	// The owner already holds every permission by owning the NFT, so a
	// self-share is a no-op the customer would read as success.
	t.Run("the owner itself is rejected", func(t *testing.T) {
		assert.ErrorIs(t, ValidateGrantee(owner.Hex(), owner), ErrGranteeInvalid)
	})

	// The endpoint validates shape before it knows the owner, so the
	// owner-equality half must be skippable.
	t.Run("a zero owner skips the self-share check", func(t *testing.T) {
		assert.NoError(t, ValidateGrantee(other.Hex(), common.Address{}),
			"shape is checkable before the owner is known")
	})
}

// GranteeClientID resolves what a grant_sacd operation grants to. The unit
// cases pin the malformed-id guard; the resolution rule itself (own
// credential, else the parent's) is the same query TestEffectiveCredential
// covers, exercised once more below through the authorizer to pin the claim
// that the grantee comes from the EFFECTIVE credential.
func TestGranteeClientID(t *testing.T) {
	logger := zerolog.Nop()
	authorizer := func(creds credentialProvider) *ShareAuthorizer {
		return NewShareAuthorizer(&logger, nil, nil, nil, creds, nil)
	}
	ctx := context.Background()

	t.Run("a well-formed client id resolves to its address", func(t *testing.T) {
		a := authorizer(&fakeCreds{effective: &EffectiveCredential{
			TenantID: "t1", ClientID: "0xCCCC000000000000000000000000000000000001"}})
		got, err := a.GranteeClientID(ctx, "t1")
		require.NoError(t, err)
		assert.Equal(t, common.HexToAddress("0xCCCC000000000000000000000000000000000001"), got)
	})

	t.Run("a malformed client id is ErrNoClientID, not a bad grant", func(t *testing.T) {
		// The empty string is the shape legacy rows carried before the
		// backfill NULLed them — granting to what it parses to (the zero
		// address) would burn the permission into nothing.
		for _, clientID := range []string{"", "not-hex", "0x1234"} {
			a := authorizer(&fakeCreds{effective: &EffectiveCredential{TenantID: "t1", ClientID: clientID}})
			_, err := a.GranteeClientID(ctx, "t1")
			assert.ErrorIs(t, err, ErrNoClientID, "client id %q", clientID)
		}
	})

	t.Run("a failed resolution keeps its named error", func(t *testing.T) {
		a := authorizer(&fakeCreds{effErr: ErrNoCredential})
		_, err := a.GranteeClientID(ctx, "t1")
		assert.ErrorIs(t, err, ErrNoCredential,
			"the controller's 409 mapping depends on the error surviving the wrap")
	})

	t.Run("a managed customer grants to its operator's client id", func(t *testing.T) {
		creds, cctx := credService(t, nil)
		a := authorizer(creds)
		got, err := a.GranteeClientID(cctx, custTenant)
		require.NoError(t, err)
		assert.Equal(t, common.HexToAddress(credOpClientID), got,
			"the grantee is the effective credential's id — the license the tenant actually presents")
	})
}
