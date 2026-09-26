package sharing

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/DIMO-Network/go-transactions/contracts/sacd"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// contractCaller is the slice of ethclient.Client that reading a grant needs,
// named so the decode is testable without an RPC.
type contractCaller interface {
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
}

// ReadCurrentGrant returns the SACD record a setPermissions call for
// (tokenID, grantee) would overwrite.
//
// Read from the contract rather than identity-api's sacds because the question
// is "what is on chain right now, about to be replaced", and an indexer that
// is a few blocks behind answers a different question — most visibly right
// after a share lands, which is exactly when an upgrade tends to follow.
//
// currentPermissionRecord reads the record at the token's CURRENT version, so a
// grant left behind by a previous owner does not count: a transfer bumps the
// version and those records are unreachable, to setPermissions as much as to
// this read.
func ReadCurrentGrant(ctx context.Context, caller contractCaller,
	sacdAddr, vehicleNft common.Address, tokenID int64, grantee common.Address,
) (sacd.ISacdPermissionRecord, error) {
	contract := sacd.NewSacd()
	data, err := contract.TryPackCurrentPermissionRecord(vehicleNft, big.NewInt(tokenID), grantee)
	if err != nil {
		return sacd.ISacdPermissionRecord{}, fmt.Errorf("pack currentPermissionRecord: %w", err)
	}
	out, err := caller.CallContract(ctx, ethereum.CallMsg{To: &sacdAddr, Data: data}, nil)
	if err != nil {
		return sacd.ISacdPermissionRecord{}, fmt.Errorf("call currentPermissionRecord: %w", err)
	}
	rec, err := contract.UnpackCurrentPermissionRecord(out)
	if err != nil {
		return sacd.ISacdPermissionRecord{}, fmt.Errorf("decode currentPermissionRecord: %w", err)
	}
	return rec, nil
}

// GrantIsLive reports whether a record still gives its grantee anything that
// overwriting it could take away.
//
// Unexpired, and carrying either permission bits or a source. The source alone
// is enough: a document-only share is valid with a zero mask, and its whole
// value is the source. A revocation writes a zero mask, a zero expiration and
// no source, and reads as dead on every clause; so does a grantee that never
// had a record.
func GrantIsLive(rec sacd.ISacdPermissionRecord, now time.Time) bool {
	if rec.Expiration == nil || rec.Expiration.Cmp(big.NewInt(now.Unix())) <= 0 {
		return false
	}
	return (rec.Permissions != nil && rec.Permissions.Sign() != 0) || rec.Source != ""
}
