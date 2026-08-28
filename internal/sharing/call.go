package sharing

import (
	"fmt"
	"math/big"

	"github.com/DIMO-Network/go-transactions/contracts/sacd"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// BuildSetPermissionsCall builds the SACD setPermissions0 call granting
// `grantee` the given permissions on vehicle `tokenID` until `expiration`.
//
// Three addresses are involved and confusing them produces failures that look
// like permission bugs rather than configuration ones:
//
//	sacdAddr    the SACD contract — where the call is SENT
//	vehicleNft  the VehicleId NFT contract — the ASSET being shared
//	grantee     the wallet receiving the permissions
//
// The calldata is packed by hand rather than through a high-level helper
// because go-transactions v0.4.0 does not have a working one: its
// Client.SetPermissions is commented out and refers to a function
// (executeUserOperationSacd) that does not exist. Do not go looking for it.
//
// Pure and network-free, so the calldata is unit-testable — which matters,
// since this is the one place a mistake becomes an irreversible on-chain grant.
func BuildSetPermissionsCall(
	sacdAddr, vehicleNft common.Address,
	tokenID int64,
	grantee common.Address,
	permissions, expiration *big.Int,
	source string,
) (*ethereum.CallMsg, error) {
	// `source` points at the SACD document describing this grant. Empty is
	// valid on chain and grants telemetry fine, but a grantee gets no document
	// access without one: dimo-app-backend reads the cloudevent agreements out
	// of the document at this URI, and no URI means no agreements means no
	// glovebox. See sacd_document.go.
	callData, err := sacd.NewSacd().TryPackSetPermissions0(
		vehicleNft, big.NewInt(tokenID), grantee, permissions, expiration, source)
	if err != nil {
		return nil, fmt.Errorf("pack setPermissions0: %w", err)
	}
	return &ethereum.CallMsg{
		To:    &sacdAddr,
		Value: big.NewInt(0),
		Data:  callData,
	}, nil
}
