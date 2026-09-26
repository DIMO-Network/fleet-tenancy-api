package sharing

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/DIMO-Network/go-transactions/contracts/sacd"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCaller answers CallContract with a fixed payload and records the call.
type stubCaller struct {
	out []byte
	msg ethereum.CallMsg
}

func (s *stubCaller) CallContract(_ context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	s.msg = msg
	return s.out, nil
}

// The read is aimed at the SACD contract with currentPermissionRecord for the
// vehicle NFT — the record setPermissions would overwrite — and the returned
// tuple decodes field for field.
func TestReadCurrentGrant_ReadsTheRecordSetPermissionsWouldOverwrite(t *testing.T) {
	sacdAddr := common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d")
	vehicleNft := common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF")
	want := sacd.ISacdPermissionRecord{
		Permissions: DefaultPermissions(),
		Expiration:  big.NewInt(1_800_000_000),
		TemplateId:  big.NewInt(0),
		Source:      "ipfs://bafyexisting",
	}

	parsed, err := sacd.SacdMetaData.ParseABI()
	require.NoError(t, err)
	out, err := parsed.Methods["currentPermissionRecord"].Outputs.Pack(want)
	require.NoError(t, err)
	caller := &stubCaller{out: out}

	got, err := ReadCurrentGrant(context.Background(), caller, sacdAddr, vehicleNft, 42, testGrantee)
	require.NoError(t, err)

	require.NotNil(t, caller.msg.To)
	assert.Equal(t, sacdAddr, *caller.msg.To)
	assert.Equal(t, sacd.NewSacd().PackCurrentPermissionRecord(vehicleNft, big.NewInt(42), testGrantee), caller.msg.Data)
	assert.Equal(t, 0, want.Permissions.Cmp(got.Permissions))
	assert.Equal(t, 0, want.Expiration.Cmp(got.Expiration))
	assert.Equal(t, want.Source, got.Source)
}

func TestGrantIsLive(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	future := big.NewInt(now.Add(24 * time.Hour).Unix())
	past := big.NewInt(now.Add(-time.Second).Unix())

	for name, tc := range map[string]struct {
		rec  sacd.ISacdPermissionRecord
		live bool
	}{
		"never granted": {sacd.ISacdPermissionRecord{}, false},
		"revoked":       {sacd.ISacdPermissionRecord{Permissions: NoPermissions(), Expiration: RevokedExpiration()}, false},
		"expired":       {sacd.ISacdPermissionRecord{Permissions: DefaultPermissions(), Expiration: past, Source: "ipfs://x"}, false},
		"expires now":   {sacd.ISacdPermissionRecord{Permissions: DefaultPermissions(), Expiration: big.NewInt(now.Unix())}, false},
		"permissions":   {sacd.ISacdPermissionRecord{Permissions: DefaultPermissions(), Expiration: future}, true},
		// A document-only share is valid with a zero mask; the source is all
		// it has, and an empty-source overwrite would take it.
		"document only":       {sacd.ISacdPermissionRecord{Permissions: NoPermissions(), Expiration: future, Source: "ipfs://x"}, true},
		"empty but unexpired": {sacd.ISacdPermissionRecord{Permissions: NoPermissions(), Expiration: future}, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.live, GrantIsLive(tc.rec, now))
		})
	}
}
