package sharing

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	zerodev "github.com/DIMO-Network/go-zerodev"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ErrOwnerModeNotConfigured is returned when an owner-mode operation is asked
// for while the sharing settings are absent. Reachable only through a race or
// a bug — the authorizer consults OwnerModeConfigured before ever selecting
// owner mode — but the worker guards anyway, because the alternative is
// signing with the wrong validator and burning the attempt.
var ErrOwnerModeNotConfigured = fmt.Errorf("owner-mode signing is not configured (sharing settings)")

// OwnerCaller is the owner-mode counterpart of fleetCaller: a UserOperation
// sent FROM the tenant's own AA wallet, signed with its root key through the
// kernel's sudo ECDSA validator. Same shape so worker tests can substitute one
// and assert on the call without a bundler. Exported, unlike fleetCaller,
// because main holds a nil-able variable of this type — a typed-nil
// *OwnerClient smuggled into the interface would defeat the nil guard.
type OwnerCaller interface {
	SendOwnerCall(ctx context.Context, wallet common.Address, rootPK *ecdsa.PrivateKey,
		msg *ethereum.CallMsg, waitForReceipt bool) (*zerodev.UserOperationResult, error)
}

// OwnerClient sends owner-mode UserOperations (docs/plans/08-aa-owner-signing.md,
// step 2). One long-lived client serves every tenant's wallet: go-zerodev's
// Client binds an account at construction, but its build-sign-send pieces are
// exposed separately, so the account-bound half is bypassed — the op is built
// for the job's wallet, signed by a per-job signer, and submitted pre-signed.
//
// The signature is the raw secp256k1 over the UserOp hash (V normalised),
// which is what the sudo ECDSA validator expects — the same envelope
// kaufmann's minting has used in production since the beginning. The
// weighted-ECDSA EIP-191 wrap that fleet.SendCall applies is exactly wrong
// here; the two modes diverge on that one line, which is why this client
// exists instead of a flag on the fleet one.
type OwnerClient struct {
	cli *zerodev.Client
}

// NewOwnerClient dials the same ZeroDev project the fleet client uses — one
// project sponsors both signing paths (decided 2026-09-01). The construction
// key is an ephemeral throwaway: go-zerodev's Client refuses a nil AccountPK
// because its bound signer needs one, but this client never uses the bound
// signer — every job brings its own wallet and key. Nothing is ever signed
// with the ephemeral key, and it goes out of scope here.
func NewOwnerClient(settings *config.Settings) (*OwnerClient, error) {
	if !settings.OwnerModeConfigured() {
		return nil, ErrOwnerModeNotConfigured
	}
	ephemeral, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate construction key: %w", err)
	}
	cli, err := zerodev.NewClient(&zerodev.ClientConfig{
		AccountAddress:             crypto.PubkeyToAddress(ephemeral.PublicKey),
		AccountPK:                  ephemeral,
		EntryPointVersion:          zerodev.EntryPointVersion07,
		RpcURL:                     &settings.RPCURL,
		BundlerURL:                 &settings.BundlerURL,
		PaymasterURL:               &settings.BundlerURL,
		ChainID:                    new(big.Int).SetInt64(settings.ChainID),
		ReceiptPollingDelaySeconds: receiptPollingDelaySeconds,
		ReceiptPollingRetries:      receiptPollingRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("owner-mode client: %w", err)
	}
	return &OwnerClient{cli: cli}, nil
}

// SendOwnerCall builds, signs and submits one owner-mode UserOperation.
//
// The ctx is accepted for interface symmetry with fleetCaller and honoured
// between the phases; go-zerodev's own calls do not take one, so cancellation
// lands at the next phase boundary rather than mid-RPC.
func (c *OwnerClient) SendOwnerCall(ctx context.Context, wallet common.Address, rootPK *ecdsa.PrivateKey,
	msg *ethereum.CallMsg, waitForReceipt bool) (*zerodev.UserOperationResult, error) {
	callData, err := zerodev.EncodeExecuteCall(msg)
	if err != nil {
		return nil, fmt.Errorf("encode execute call: %w", err)
	}

	// Nonce, gas, sponsorship and the hash — for the JOB's wallet, not the
	// client's bound account. The default nonce key selects the kernel's root
	// validator, which is the whole point of owner mode.
	op, opHash, err := c.cli.GetUserOperationAndHashToSign(wallet, callData)
	if err != nil {
		return nil, fmt.Errorf("build owner-mode UserOp: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	signer, err := c.cli.GetSmartAccountSigner(wallet, rootPK)
	if err != nil {
		return nil, fmt.Errorf("build owner signer: %w", err)
	}
	sig, err := signer.SignUserOperationHash(*opHash)
	if err != nil {
		return nil, fmt.Errorf("sign owner-mode UserOp: %w", err)
	}
	op.Signature = sig

	return c.cli.SendSignedUserOperation(op, waitForReceipt)
}
