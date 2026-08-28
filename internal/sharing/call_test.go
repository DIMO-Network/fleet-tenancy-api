package sharing

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	sacdContract = common.HexToAddress("0x3c152B5d96769661008Ff404224d6530FCAC766d")
	vehicleNFT   = common.HexToAddress("0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF")
	granteeAddr  = common.HexToAddress("0x1111111111111111111111111111111111111111")
)

// The call must be aimed at the SACD contract and carry the VehicleId NFT as
// the asset. Three addresses are in play and swapping the first two is the
// easiest mistake to make: it produces a transaction that fails in ways that
// read as a permissions problem rather than a configuration one.
func TestBuildSetPermissionsCall_TargetsSacdWithNftAsAsset(t *testing.T) {
	msg, err := BuildSetPermissionsCall(sacdContract, vehicleNFT, 42, granteeAddr,
		DefaultPermissions(), big.NewInt(1800000000), "")
	require.NoError(t, err)

	require.NotNil(t, msg.To)
	assert.Equal(t, sacdContract, *msg.To, "the call is SENT to the SACD contract")
	assert.Equal(t, big.NewInt(0), msg.Value, "setPermissions0 is not payable")

	// The NFT address is the asset argument, so it appears in the calldata
	// rather than in To. Its absence would mean the asset was left unset.
	assert.Contains(t, common.Bytes2Hex(msg.Data), common.Bytes2Hex(vehicleNFT.Bytes()),
		"the VehicleId NFT is the asset, encoded in the calldata")
	assert.Contains(t, common.Bytes2Hex(msg.Data), common.Bytes2Hex(granteeAddr.Bytes()),
		"the grantee is encoded in the calldata")
}

// Different inputs must produce different calldata. A packer that silently
// ignored an argument would otherwise pass every other test here — and would
// grant a share to the wrong wallet or on the wrong vehicle.
func TestBuildSetPermissionsCall_EveryArgumentReachesTheCalldata(t *testing.T) {
	base, err := BuildSetPermissionsCall(sacdContract, vehicleNFT, 42, granteeAddr,
		DefaultPermissions(), big.NewInt(1800000000), "")
	require.NoError(t, err)

	for name, build := range map[string]func() ([]byte, error){
		"a different token id": func() ([]byte, error) {
			m, e := BuildSetPermissionsCall(sacdContract, vehicleNFT, 43, granteeAddr,
				DefaultPermissions(), big.NewInt(1800000000), "")
			return m.Data, e
		},
		"a different grantee": func() ([]byte, error) {
			other := common.HexToAddress("0x2222222222222222222222222222222222222222")
			m, e := BuildSetPermissionsCall(sacdContract, vehicleNFT, 42, other,
				DefaultPermissions(), big.NewInt(1800000000), "")
			return m.Data, e
		},
		"different permissions": func() ([]byte, error) {
			m, e := BuildSetPermissionsCall(sacdContract, vehicleNFT, 42, granteeAddr,
				FullPermissions(), big.NewInt(1800000000), "")
			return m.Data, e
		},
		"a different expiration": func() ([]byte, error) {
			m, e := BuildSetPermissionsCall(sacdContract, vehicleNFT, 42, granteeAddr,
				DefaultPermissions(), big.NewInt(1900000000), "")
			return m.Data, e
		},
		"a different asset": func() ([]byte, error) {
			other := common.HexToAddress("0x3333333333333333333333333333333333333333")
			m, e := BuildSetPermissionsCall(sacdContract, other, 42, granteeAddr,
				DefaultPermissions(), big.NewInt(1800000000), "")
			return m.Data, e
		},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := build()
			require.NoError(t, err)
			assert.NotEqual(t, base.Data, data, "%s must change the calldata", name)
		})
	}
}

// The selector is the first four bytes and identifies setPermissions0. It must
// not drift with the permission set or anything else about the arguments.
func TestBuildSetPermissionsCall_SelectorIsStable(t *testing.T) {
	a, err := BuildSetPermissionsCall(sacdContract, vehicleNFT, 1, granteeAddr,
		DefaultPermissions(), big.NewInt(1), "")
	require.NoError(t, err)
	b, err := BuildSetPermissionsCall(sacdContract, vehicleNFT, 999, granteeAddr,
		FullPermissions(), big.NewInt(2), "")
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(a.Data), 4)
	assert.Equal(t, a.Data[:4], b.Data[:4], "the function selector must not vary with arguments")
}
