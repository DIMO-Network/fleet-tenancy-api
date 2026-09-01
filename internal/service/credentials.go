package service

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/DIMO-Network/shared/pkg/dimoauth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog"
)

// ErrNoCredential is returned when neither the tenant nor its parent holds a
// usable developer license. For an operator-managed customer this is a
// configuration state, not a caller mistake — the operator has not set a
// license yet — so controllers surface it as 409 rather than 4xx-blaming the
// caller or 5xx-blaming the service.
var ErrNoCredential = errors.New("tenant has no effective credential")

// EffectiveCredential is the public face of a tenant's resolved credential:
// which tenant actually holds it and the identifiers a caller may see. It
// deliberately carries no key material — decrypted keys exist only inside
// CredentialService, which is the point of the token minter existing at all.
type EffectiveCredential struct {
	// TenantID holds the credential — the subject itself, or its parent when
	// the subject is an operator-managed customer with no license of its own.
	TenantID      string
	ClientID      string
	SignerAddress string
	// AAWalletAddress is the tenant's own Kernel smart account when one is
	// configured (docs/plans/08-aa-owner-signing.md) — the wallet whose
	// vehicles this credential can sign for in owner mode. Empty when none.
	AAWalletAddress string
}

// CredentialService resolves effective credentials and mints developer JWTs
// from them. It is the only code in the service that sees a decrypted key at
// runtime, and the decrypted key goes exactly one place: a cached
// dimoauth.AuthService, which needs it to re-sign the login challenge when the
// token expires. Never logged, never returned, never stored decrypted.
type CredentialService struct {
	logger   *zerolog.Logger
	pdb      *db.Store
	settings *config.Settings
	identity gateway.IdentityAPI

	mu sync.Mutex
	// minters is keyed by lower(clientID) rather than tenant id, because every
	// tenant sharing a credential shares its token — an operator and all its
	// managed customers mint from one AuthService and one cached token.
	minters map[string]*minterEntry
}

type minterEntry struct {
	// keyFingerprint invalidates the cached AuthService when the credential is
	// rotated. kaufmann caches per tenant forever and serves a rotated-away key
	// until restart; hashing costs nothing and removes that failure mode.
	keyFingerprint [32]byte
	auth           *dimoauth.AuthService
}

func NewCredentialService(logger *zerolog.Logger, pdb *db.Store, settings *config.Settings,
	identity gateway.IdentityAPI) *CredentialService {
	return &CredentialService{
		logger:   logger,
		pdb:      pdb,
		settings: settings,
		identity: identity,
		minters:  map[string]*minterEntry{},
	}
}

// Effective resolves which credential reaches a tenant: its own if it holds
// one, otherwise its parent's. The WHERE mirrors CallerMayAccess — "holds one"
// means dimo_client_id IS NOT NULL, the same expression the scope rule and the
// resolver's unique index use — so scope, resolution and minting can never
// disagree about whose license a tenant is reached with.
func (s *CredentialService) Effective(ctx context.Context, tenantID string) (*EffectiveCredential, error) {
	cred, _, _, err := s.effectiveWithKey(ctx, tenantID)
	return cred, err
}

// effectiveWithKey additionally returns the encrypted API key and the
// encrypted AA wallet root key. Decryption happens as late as possible — in
// DeveloperJWT and AAWalletSigner respectively.
func (s *CredentialService) effectiveWithKey(ctx context.Context, tenantID string) (*EffectiveCredential, string, string, error) {
	var (
		holder, clientID      string
		signer, keyEnc        sql.NullString
		aaWallet, aaWalletKey sql.NullString
	)
	err := s.pdb.DBS().Reader.QueryRowContext(ctx, `
		SELECT c.tenant_id, c.dimo_client_id, c.signer_address, c.dimo_api_key_enc,
		       c.aa_wallet_address, c.aa_wallet_key_enc
		  FROM tenants t
		  JOIN tenant_credentials c
		    ON (c.tenant_id = t.id OR c.tenant_id = t.parent_tenant_id)
		   AND c.dimo_client_id IS NOT NULL
		 WHERE t.id = $1::uuid
		 ORDER BY (c.tenant_id = t.id) DESC
		 LIMIT 1`, tenantID).Scan(&holder, &clientID, &signer, &keyEnc, &aaWallet, &aaWalletKey)
	if errors.Is(err, sql.ErrNoRows) {
		// No credential row at all, or none with a client id — check the tenant
		// exists so an unknown uuid is not misreported as "no credential".
		var exists bool
		if lookupErr := s.pdb.DBS().Reader.QueryRowContext(ctx,
			`SELECT true FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&exists); lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return nil, "", "", ErrTenantNotFound
			}
			return nil, "", "", fmt.Errorf("load tenant %s: %w", tenantID, lookupErr)
		}
		return nil, "", "", ErrNoCredential
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("resolve effective credential of %s: %w", tenantID, err)
	}
	return &EffectiveCredential{
		TenantID:        holder,
		ClientID:        clientID,
		SignerAddress:   signer.String,
		AAWalletAddress: aaWallet.String,
	}, keyEnc.String, aaWalletKey.String, nil
}

// DeveloperJWT mints a developer-license JWT for the tenant's effective
// credential. The underlying AuthService caches the token and refreshes it on
// expiry, so calling this per request is fine.
func (s *CredentialService) DeveloperJWT(ctx context.Context, tenantID string) (*models.MintedToken, error) {
	cred, keyEnc, _, err := s.effectiveWithKey(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if keyEnc == "" {
		return nil, fmt.Errorf("credential of tenant %s (client id %s) has no API key stored: %w",
			cred.TenantID, cred.ClientID, ErrNoCredential)
	}
	if s.settings.DimoAuthURL.String() == "" {
		return nil, fmt.Errorf("DIMO_AUTH_URL is not configured")
	}

	privateKeyHex, err := DecryptSecret(s.settings.TenantSecretEncKey, keyEnc)
	if err != nil {
		// GCM authenticates, so this means the wrong master key or a corrupt
		// row — an operational fault worth naming, without the material.
		return nil, fmt.Errorf("decrypt credential of tenant %s: %w", cred.TenantID, err)
	}

	auth, err := s.minterFor(cred.ClientID, privateKeyHex)
	if err != nil {
		return nil, err
	}

	// Retried: the login challenge is unreliable and single-use, so a second
	// call is a fresh attempt rather than the same request repeated. See
	// mintWithRetry for the evidence that the key is not the problem.
	token := mintWithRetry(auth, func(attempt int) {
		s.logger.Warn().Int("attempt", attempt).
			Str("client_id", cred.ClientID).
			Str("tenant_id", cred.TenantID).
			Msg("developer JWT mint failed, retrying with a fresh challenge")
	})
	if token == nil {
		return nil, fmt.Errorf("minting developer JWT for client id %s failed after %d attempts",
			cred.ClientID, mintAttempts)
	}
	minted := &models.MintedToken{
		Token:              token.Raw,
		ClientID:           cred.ClientID,
		CredentialTenantID: cred.TenantID,
	}
	if exp, err := token.Claims.GetExpirationTime(); err == nil && exp != nil {
		minted.ExpiresAt = exp.UTC()
	}
	return minted, nil
}

// SignAsTenant signs message with the tenant's effective developer-license
// private key, ERC-191 style ("\x19Ethereum Signed Message:\n<len><msg>",
// keccak256, secp256k1, V normalised to 27/28) — byte-identical to what both
// source apps' attestation publishers produced, so verifiers cannot tell the
// producer changed. Returns the credential alongside so the caller can build
// the event's source without a second resolution.
//
// The key is decrypted, used, and discarded here — it does not leave this
// service, same rule as the minter.
func (s *CredentialService) SignAsTenant(ctx context.Context, tenantID string, message []byte) (string, *EffectiveCredential, error) {
	cred, keyEnc, _, err := s.effectiveWithKey(ctx, tenantID)
	if err != nil {
		return "", nil, err
	}
	if keyEnc == "" {
		return "", nil, fmt.Errorf("credential of tenant %s (client id %s) has no API key stored: %w",
			cred.TenantID, cred.ClientID, ErrNoCredential)
	}
	privateKeyHex, err := DecryptSecret(s.settings.TenantSecretEncKey, keyEnc)
	if err != nil {
		return "", nil, fmt.Errorf("decrypt credential of tenant %s: %w", cred.TenantID, err)
	}
	sig, err := signERC191(message, privateKeyHex)
	if err != nil {
		return "", nil, fmt.Errorf("sign as tenant %s: %w", cred.TenantID, err)
	}
	return sig, cred, nil
}

// ValidateCredential proves a client id + plaintext key can actually mint a
// developer JWT, before anything persists them. Runs through minterFor, so a
// successful validation also warms the exact minter the credential will use
// once stored — and the fingerprint keying means a later rotation rebuilds it.
func (s *CredentialService) ValidateCredential(clientID, apiKeyPlain string) error {
	if clientID == "" || apiKeyPlain == "" {
		return fmt.Errorf("client id and API key are required")
	}
	minter, err := s.minterFor(clientID, apiKeyPlain)
	if err != nil {
		return err
	}
	// Retried for a different reason than the mint above: this runs when a
	// human has just pasted a credential, and rejecting a VALID key because the
	// challenge flaked tells them their key is wrong when it is not — a lie
	// that costs a support conversation and invites them to rotate a working
	// key.
	if mintWithRetry(minter, func(attempt int) {
		s.logger.Warn().Int("attempt", attempt).Str("client_id", clientID).
			Msg("credential validation mint failed, retrying with a fresh challenge")
	}) == nil {
		return fmt.Errorf("minting a developer JWT for client id %s failed after %d attempts",
			clientID, mintAttempts)
	}
	return nil
}

// ErrNoAAWallet is returned when a tenant's effective credential has no AA
// wallet configured. Like ErrNoCredential, a configuration state: the
// license-holding tenant has not set one up.
var ErrNoAAWallet = errors.New("tenant's effective credential has no AA wallet configured")

// AAWalletSigner returns the effective credential's AA wallet address and its
// decrypted root key, for owner-mode signing (docs/plans/08-aa-owner-signing.md).
// Same rule as every other key in this service: decrypted for the duration of
// one operation, never logged, never returned over the wire, never cached.
// The stored form is canonical 64-char hex (AAWalletService.Set guarantees
// it), so a parse failure here means a corrupt row, not a formatting quirk.
func (s *CredentialService) AAWalletSigner(ctx context.Context, tenantID string) (common.Address, *ecdsa.PrivateKey, error) {
	cred, _, aaKeyEnc, err := s.effectiveWithKey(ctx, tenantID)
	if err != nil {
		return common.Address{}, nil, err
	}
	if cred.AAWalletAddress == "" || aaKeyEnc == "" {
		return common.Address{}, nil, fmt.Errorf("credential of tenant %s: %w", cred.TenantID, ErrNoAAWallet)
	}
	keyHex, err := DecryptSecret(s.settings.TenantSecretEncKey, aaKeyEnc)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("decrypt AA wallet key of tenant %s: %w", cred.TenantID, err)
	}
	pk, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("AA wallet key of tenant %s does not parse: %w", cred.TenantID, err)
	}
	return common.HexToAddress(cred.AAWalletAddress), pk, nil
}

// signERC191 is the personal_sign scheme both source apps used (kaufmann's
// vinvc.SignMessage, fleet-lite's signDataSecp256k1): 0x-prefixed hex of the
// 65-byte signature with V as 27/28.
func signERC191(message []byte, privateKeyHex string) (string, error) {
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(privateKeyHex, "0x"))
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	prefixed := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	sig, err := crypto.Sign(crypto.Keccak256([]byte(prefixed)), pk)
	if err != nil {
		return "", err
	}
	if sig[64] < 27 {
		sig[64] += 27
	}
	return "0x" + hex.EncodeToString(sig), nil
}

// minterFor returns the cached AuthService for a client id, building one on
// first use or after a key rotation. The redirect URI is resolved from
// identity-api at build time — it lives on the license, and a mint against a
// URI the license no longer registers fails upstream anyway.
func (s *CredentialService) minterFor(clientID, privateKeyHex string) (*dimoauth.AuthService, error) {
	fingerprint := sha256.Sum256([]byte(privateKeyHex))
	key := common.HexToAddress(clientID).Hex()

	s.mu.Lock()
	entry, ok := s.minters[key]
	s.mu.Unlock()
	if ok && entry.keyFingerprint == fingerprint {
		return entry.auth, nil
	}

	redirectURI, err := s.identity.RedirectURIForClientID(clientID)
	if err != nil {
		return nil, fmt.Errorf("resolve redirect URI for client id %s: %w", clientID, err)
	}
	redirectURL, err := url.Parse(redirectURI)
	if err != nil {
		return nil, fmt.Errorf("redirect URI for client id %s: %w", clientID, err)
	}

	auth, err := dimoauth.NewAuthService(*s.logger, &dimoauth.Settings{
		AuthURL:            s.settings.DimoAuthURL,
		TokenExchangeURL:   s.settings.TokenExchangeURL,
		NFTContractAddress: common.HexToAddress(s.settings.VehicleNftAddress),
		ClientID:           common.HexToAddress(clientID),
		RedirectURL:        *redirectURL,
		PrivateKeyHex:      privateKeyHex,
	})
	if err != nil {
		return nil, fmt.Errorf("build minter for client id %s: %w", clientID, err)
	}

	s.mu.Lock()
	s.minters[key] = &minterEntry{keyFingerprint: fingerprint, auth: auth}
	s.mu.Unlock()
	return auth, nil
}
