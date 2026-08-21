// Package sharing sends this service's server-signed on-chain operations:
// vehicle SACD permission grants, and the typed shared-account operations of
// plan 06 step 3 (transfer, synthetic-device burn, vehicle burn, re-grant).
//
// Every one of them is a UserOperation sent from the vehicle owner's ZeroDev
// kernel account and signed by the acting tenant's signer key. The owner never
// signs — the kernel registered the tenant's signer as a secondary
// weighted-ECDSA validator when the account was created, and that is the whole
// basis on which this service may act.
//
// The mechanism is kaufmann-oracle's, ported: SharedAccountTransferWorker
// re-shares a transferred vehicle back to its tenant exactly this way. What is
// new here is that a share's grantee is chosen by a customer rather than being
// the tenant itself, which is why the authorization chain around it is
// stricter.
//
// This file is the plumbing only — the client and its lifecycle. The share
// call construction is in call.go and its worker in worker.go; the typed
// operations are whole in shared_ops.go; the authorization chain is
// internal/service/sharing.go. The decisions behind them are recorded in
// docs/HANDOFF.md under "Vehicle sharing" and in
// docs/plans/06-signer-key-consolidation.md.
package sharing

import (
	"fmt"
	"math/big"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/go-zerodev/fleet"
)

// Receipt polling. The bundler is asked every ReceiptPollingDelaySeconds up to
// ReceiptPollingRetries times, so these two multiply into how long a worker
// will wait for a UserOp to land: 5s × 60 = 5 minutes, matching kaufmann.
//
// Worth keeping in step with the worker's River timeout when that lands — a
// timeout shorter than this window kills the job while the transaction is
// still in flight, and the grant then exists on-chain with the job recorded as
// failed.
const (
	receiptPollingDelaySeconds = 5
	receiptPollingRetries      = 60
)

// ErrNotConfigured is returned when sharing settings are absent. It is a
// distinct error rather than a nil client so callers fail with something
// readable instead of a panic; see Settings.SharingConfigured.
var ErrNotConfigured = fmt.Errorf("vehicle sharing is not configured (SACD_ADDRESS, SYNTHETIC_NFT_ADDRESS, RPC_URL, BUNDLER_URL, VEHICLE_NFT_ADDRESS, CHAIN_ID)")

// NewFleetClient builds the ZeroDev fleet client used to send UserOps from an
// owner's kernel account.
//
// The bundler URL is passed as the paymaster URL as well. That is not an
// oversight: ZeroDev serves both RPCs from one project URL, and kaufmann's
// client is configured the same way.
//
// Returns ErrNotConfigured rather than dialling with empty URLs when the
// feature is off, so an unconfigured environment gets a named error at startup
// instead of a confusing dial failure.
func NewFleetClient(settings *config.Settings) (*fleet.Client, error) {
	if !settings.SharingConfigured() {
		return nil, ErrNotConfigured
	}
	client, err := fleet.NewClient(&fleet.ClientConfig{
		RpcURL:                     &settings.RPCURL,
		BundlerURL:                 &settings.BundlerURL,
		PaymasterURL:               &settings.BundlerURL,
		ChainID:                    new(big.Int).SetInt64(settings.ChainID),
		ReceiptPollingDelaySeconds: receiptPollingDelaySeconds,
		ReceiptPollingRetries:      receiptPollingRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("fleet client: %w", err)
	}
	return client, nil
}
