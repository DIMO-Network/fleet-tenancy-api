package service

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog"
)

var (
	// ErrAAWalletInvalid is returned when the supplied wallet + key fail
	// validation — the caller's input, not our fault: 400. Always wrapped with
	// the specific reason, which never contains key material.
	ErrAAWalletInvalid = errors.New("invalid AA wallet configuration")

	// ErrAANotCredentialHolder is returned when the subject tenant does not
	// hold its own developer license. The AA wallet rides on the credential row
	// (docs/plans/08-aa-owner-signing.md, D2), so it is configured on the
	// license-holding tenant and inherited by everyone who resolves to that
	// credential — writing it anywhere else would create a row effective
	// resolution can never see. A configuration state, not a caller mistake: 409.
	ErrAANotCredentialHolder = errors.New(
		"tenant does not hold its own developer license; configure the AA wallet on the license-holding tenant")

	// ErrChainUnavailable is returned when the on-chain half of validation
	// cannot run — RPC unconfigured, unreachable, or answering for the wrong
	// chain. Not a verdict on the wallet: 503, so the caller retries later
	// instead of concluding their wallet is bad.
	ErrChainUnavailable = errors.New("chain verification unavailable")
)

// kernelECDSAValidator is the sudo ECDSA validator every Kernel >=0.3.1
// account created through the standard ZeroDev flow uses (the wallet-creator
// flow included). Deployed at the same address across chains; source of truth
// is @zerodev/ecdsa-validator's constants.
var kernelECDSAValidator = common.HexToAddress("0x845ADb2C711129d4f3966735eD98a9F09fC4cE57")

// ecdsaValidatorStorageSelector is keccak256("ecdsaValidatorStorage(address)")[:4].
// The getter returns the kernel's registered sudo owner as a 32-byte-padded
// address. Verified against the deployed Polygon contract 2026-08-31: a known
// kernel account answered its root EOA, an unknown address answers zero.
var ecdsaValidatorStorageSelector = []byte{0x20, 0x70, 0x9e, 0xfc}

// ChainReader is the slice of ethclient.Client the validation needs, an
// interface so the on-chain half is testable without an RPC.
type ChainReader interface {
	ChainID(ctx context.Context) (*big.Int, error)
	CodeAt(ctx context.Context, account common.Address, blockNumber *big.Int) ([]byte, error)
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	Close()
}

// ChainDial opens a ChainReader for the duration of one validation. Config
// writes are rare, so a fresh connection per call is deliberate — no pooled
// client to hold a stale DNS answer or leak between tests.
type ChainDial func(ctx context.Context) (ChainReader, error)

// EthChainDial dials the configured RPC. Split out so tests can substitute a
// fake without a network.
func EthChainDial(settings *config.Settings) ChainDial {
	return func(ctx context.Context) (ChainReader, error) {
		return ethclient.DialContext(ctx, settings.RPCURL.String())
	}
}

// AAWalletService configures and reads the tenant AA wallet
// (docs/plans/08-aa-owner-signing.md, step 1). Set validates strictly before
// anything persists, because the failure mode of a wrong key is a
// gas-spending job with MaxAttempts: 1 — refusing a bad wallet here costs a
// 400; accepting one costs a burned share attempt and a support conversation.
type AAWalletService struct {
	logger   *zerolog.Logger
	pdb      *db.Store
	settings *config.Settings
	dial     ChainDial
}

func NewAAWalletService(logger *zerolog.Logger, pdb *db.Store, settings *config.Settings,
	dial ChainDial) *AAWalletService {
	return &AAWalletService{logger: logger, pdb: pdb, settings: settings, dial: dial}
}

// Set validates and stores the tenant's AA wallet. The checks run cheapest
// first, and nothing persists until every one has passed:
//
//  1. address parses (stored EIP-55 checksummed — the shared-accounts lookup
//     shows what a non-checksummed write costs),
//  2. key parses (0x prefix and whitespace trimmed HERE, at the edge — the
//     share path deliberately does not trim),
//  3. the address is not the key's own EOA (the classic paste mistake: the
//     kernel address and its root EOA are different things),
//  4. the subject holds its own license (D2 — see ErrAANotCredentialHolder),
//  5. on chain: the RPC answers for the configured chain, the kernel is
//     deployed (go-zerodev cannot deploy, so an undeployed wallet would fail
//     at first use instead), and the kernel's sudo ECDSA validator records
//     exactly this key's address as owner.
//
// The stored key is the canonical 64-char hex of the parsed key, so whatever
// form was pasted, what the signer later decrypts always parses.
func (s *AAWalletService) Set(ctx context.Context, tenantID string, in *models.SetAAWalletInput) (*models.AAWalletStatus, error) {
	if in.WalletAddress == "" || in.PrivateKey == "" {
		return nil, fmt.Errorf("%w: walletAddress and privateKey are required", ErrAAWalletInvalid)
	}
	if !common.IsHexAddress(in.WalletAddress) {
		return nil, fmt.Errorf("%w: walletAddress is not a hex address", ErrAAWalletInvalid)
	}
	wallet := common.HexToAddress(in.WalletAddress)

	pk, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(in.PrivateKey), "0x"))
	if err != nil {
		return nil, fmt.Errorf("%w: privateKey does not parse as a secp256k1 key", ErrAAWalletInvalid)
	}
	root := crypto.PubkeyToAddress(pk.PublicKey)
	if root == wallet {
		return nil, fmt.Errorf("%w: walletAddress is the key's own EOA — expected the Kernel smart-account address it controls",
			ErrAAWalletInvalid)
	}

	var holdsLicense bool
	err = s.pdb.DBS().Reader.QueryRowContext(ctx, `
		SELECT c.dimo_client_id IS NOT NULL
		  FROM tenants t
		  LEFT JOIN tenant_credentials c ON c.tenant_id = t.id
		 WHERE t.id = $1::uuid`, tenantID).Scan(&holdsLicense)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load tenant %s: %w", tenantID, err)
	}
	if !holdsLicense {
		return nil, ErrAANotCredentialHolder
	}

	if err := s.verifyOnChain(ctx, wallet, root); err != nil {
		return nil, err
	}

	keyHex := hex.EncodeToString(crypto.FromECDSA(pk))
	enc, err := EncryptSecret(s.settings.TenantSecretEncKey, keyHex)
	if err != nil {
		return nil, fmt.Errorf("encrypt AA wallet key: %w", err)
	}

	res, err := s.pdb.DBS().Writer.ExecContext(ctx, `
		UPDATE tenant_credentials
		   SET aa_wallet_address = $2, aa_wallet_key_enc = $3, updated_at = NOW()
		 WHERE tenant_id = $1`, tenantID, wallet.Hex(), enc)
	if err != nil {
		return nil, fmt.Errorf("store AA wallet for tenant %s: %w", tenantID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// The holder check saw a credential row moments ago; losing it here
		// means a concurrent delete, which the caller should hear about.
		return nil, fmt.Errorf("credential row of tenant %s vanished during AA wallet write", tenantID)
	}

	s.logger.Info().Str("tenant_id", tenantID).Str("aa_wallet", wallet.Hex()).
		Str("root_eoa", root.Hex()).Msg("AA wallet configured")
	return &models.AAWalletStatus{
		Configured:         true,
		WalletAddress:      wallet.Hex(),
		CredentialTenantID: tenantID,
	}, nil
}

// Get answers which AA wallet a tenant resolves to, through the same
// effective-credential expression the minter and the scope rule use — own
// credential first, else the parent's — so an operator-managed customer sees
// the wallet it will actually share with. A tenant whose effective credential
// has no wallet answers configured=false; only an unknown tenant errors.
func (s *AAWalletService) Get(ctx context.Context, tenantID string) (*models.AAWalletStatus, error) {
	var (
		holder string
		addr   sql.NullString
	)
	err := s.pdb.DBS().Reader.QueryRowContext(ctx, `
		SELECT c.tenant_id, c.aa_wallet_address
		  FROM tenants t
		  JOIN tenant_credentials c
		    ON (c.tenant_id = t.id OR c.tenant_id = t.parent_tenant_id)
		   AND c.dimo_client_id IS NOT NULL
		 WHERE t.id = $1::uuid
		 ORDER BY (c.tenant_id = t.id) DESC
		 LIMIT 1`, tenantID).Scan(&holder, &addr)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if lookupErr := s.pdb.DBS().Reader.QueryRowContext(ctx,
			`SELECT true FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&exists); lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return nil, ErrTenantNotFound
			}
			return nil, fmt.Errorf("load tenant %s: %w", tenantID, lookupErr)
		}
		return &models.AAWalletStatus{Configured: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve AA wallet of %s: %w", tenantID, err)
	}
	if !addr.Valid {
		return &models.AAWalletStatus{Configured: false, CredentialTenantID: holder}, nil
	}
	return &models.AAWalletStatus{
		Configured:         true,
		WalletAddress:      addr.String,
		CredentialTenantID: holder,
	}, nil
}

// Clear removes the tenant's own AA wallet. Idempotent — clearing a tenant
// that has none (or no credential row at all) succeeds, because the state the
// caller asked for already holds.
func (s *AAWalletService) Clear(ctx context.Context, tenantID string) error {
	res, err := s.pdb.DBS().Writer.ExecContext(ctx, `
		UPDATE tenant_credentials
		   SET aa_wallet_address = NULL, aa_wallet_key_enc = NULL, updated_at = NOW()
		 WHERE tenant_id = $1 AND aa_wallet_address IS NOT NULL`, tenantID)
	if err != nil {
		return fmt.Errorf("clear AA wallet of tenant %s: %w", tenantID, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.logger.Info().Str("tenant_id", tenantID).Msg("AA wallet cleared")
	}
	return nil
}

// verifyOnChain runs the checks only the chain can answer. RPC problems are
// ErrChainUnavailable (retry later, no verdict); a deployed-and-readable
// wallet that fails a check is ErrAAWalletInvalid (a verdict).
func (s *AAWalletService) verifyOnChain(ctx context.Context, wallet, root common.Address) error {
	if s.settings.RPCURL.String() == "" || s.settings.ChainID == 0 {
		return fmt.Errorf("%w: RPC_URL and CHAIN_ID must be configured", ErrChainUnavailable)
	}
	reader, err := s.dial(ctx)
	if err != nil {
		return fmt.Errorf("%w: dial RPC: %v", ErrChainUnavailable, err)
	}
	defer reader.Close()

	chainID, err := reader.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("%w: eth_chainId: %v", ErrChainUnavailable, err)
	}
	if chainID.Int64() != s.settings.ChainID {
		// Our RPC disagreeing with our CHAIN_ID is a config fault of ours, not
		// a verdict on the wallet — the two-backslash incident is why this is
		// checked at config time instead of discovered by a failed job.
		return fmt.Errorf("%w: RPC answers chain %d, CHAIN_ID is %d",
			ErrChainUnavailable, chainID.Int64(), s.settings.ChainID)
	}

	code, err := reader.CodeAt(ctx, wallet, nil)
	if err != nil {
		return fmt.Errorf("%w: eth_getCode: %v", ErrChainUnavailable, err)
	}
	if len(code) == 0 {
		return fmt.Errorf("%w: %s has no code on chain %d — the Kernel account must be deployed first (the generate flow deploys; a counterfactual address that only received NFTs is not deployed)",
			ErrAAWalletInvalid, wallet.Hex(), s.settings.ChainID)
	}

	calldata := append(append([]byte{}, ecdsaValidatorStorageSelector...),
		common.LeftPadBytes(wallet.Bytes(), 32)...)
	res, err := reader.CallContract(ctx, ethereum.CallMsg{To: &kernelECDSAValidator, Data: calldata}, nil)
	if err != nil {
		return fmt.Errorf("%w: read sudo validator: %v", ErrChainUnavailable, err)
	}
	if len(res) < 32 {
		return fmt.Errorf("%w: sudo validator answered %d bytes", ErrChainUnavailable, len(res))
	}
	owner := common.BytesToAddress(res[12:32])
	if owner == (common.Address{}) {
		return fmt.Errorf("%w: %s has no owner recorded on the Kernel ECDSA validator — not a Kernel v3 account with the standard sudo validator",
			ErrAAWalletInvalid, wallet.Hex())
	}
	if owner != root {
		return fmt.Errorf("%w: the wallet's sudo owner is %s, but the supplied key controls %s",
			ErrAAWalletInvalid, owner.Hex(), root.Hex())
	}
	return nil
}
