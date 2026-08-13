package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/DIMO-Network/shared/pkg/dimoauth"
	"github.com/ethereum/go-ethereum/common"
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
	cred, _, err := s.effectiveWithKey(ctx, tenantID)
	return cred, err
}

// effectiveWithKey additionally returns the encrypted private key, for the
// minter only. Decryption happens as late as possible, in DeveloperJWT.
func (s *CredentialService) effectiveWithKey(ctx context.Context, tenantID string) (*EffectiveCredential, string, error) {
	var (
		holder, clientID string
		signer, keyEnc   sql.NullString
	)
	err := s.pdb.DBS().Reader.QueryRowContext(ctx, `
		SELECT c.tenant_id, c.dimo_client_id, c.signer_address, c.dimo_api_key_enc
		  FROM tenants t
		  JOIN tenant_credentials c
		    ON (c.tenant_id = t.id OR c.tenant_id = t.parent_tenant_id)
		   AND c.dimo_client_id IS NOT NULL
		 WHERE t.id = $1::uuid
		 ORDER BY (c.tenant_id = t.id) DESC
		 LIMIT 1`, tenantID).Scan(&holder, &clientID, &signer, &keyEnc)
	if errors.Is(err, sql.ErrNoRows) {
		// No credential row at all, or none with a client id — check the tenant
		// exists so an unknown uuid is not misreported as "no credential".
		var exists bool
		if lookupErr := s.pdb.DBS().Reader.QueryRowContext(ctx,
			`SELECT true FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&exists); lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return nil, "", ErrTenantNotFound
			}
			return nil, "", fmt.Errorf("load tenant %s: %w", tenantID, lookupErr)
		}
		return nil, "", ErrNoCredential
	}
	if err != nil {
		return nil, "", fmt.Errorf("resolve effective credential of %s: %w", tenantID, err)
	}
	return &EffectiveCredential{
		TenantID:      holder,
		ClientID:      clientID,
		SignerAddress: signer.String,
	}, keyEnc.String, nil
}

// DeveloperJWT mints a developer-license JWT for the tenant's effective
// credential. The underlying AuthService caches the token and refreshes it on
// expiry, so calling this per request is fine.
func (s *CredentialService) DeveloperJWT(ctx context.Context, tenantID string) (*models.MintedToken, error) {
	cred, keyEnc, err := s.effectiveWithKey(ctx, tenantID)
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

	token := auth.GetToken()
	if token == nil {
		return nil, fmt.Errorf("minting developer JWT for client id %s failed", cred.ClientID)
	}
	minted := &models.MintedToken{
		Token:              token.Raw,
		ClientID:           cred.ClientID,
		CredentialTenantID: cred.TenantID,
	}
	if exp, err := token.Claims.GetExpirationTime(); err == nil && exp != nil {
		minted.ExpiresAt = exp.Time.UTC()
	}
	return minted, nil
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
