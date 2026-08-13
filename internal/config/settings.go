package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/DIMO-Network/shared/pkg/db"
)

// Settings is the service configuration, loaded from settings.yaml or the
// environment. Field names and layout mirror fleet-lite-app and kaufmann-oracle
// so all three deploy the same way and code stays portable between them.
type Settings struct {
	Environment    string      `yaml:"ENVIRONMENT"`
	LogLevel       string      `yaml:"LOG_LEVEL"`
	ServiceName    string      `yaml:"SERVICE_NAME"`
	APIPort        int         `yaml:"API_PORT"`
	MonitoringPort int         `yaml:"MONITORING_PORT"`
	DB             db.Settings `yaml:"DB"` // secret

	// JwtKeySetURL verifies both end-user JWTs and developer-license JWTs —
	// they share the DIMO issuer.
	JwtKeySetURL url.URL `yaml:"JWT_KEY_SET_URL"`

	// TenantSecretEncKey derives the AES-256-GCM key for credentials at rest.
	// MUST be set outside local — see Validate.
	TenantSecretEncKey string `yaml:"TENANT_SECRET_ENC_KEY"`

	// The DIMO platform services behind the token minter and provisioning.
	// None are required to boot: /v1/authz must stay available even when the
	// minter is unconfigured, so a missing value fails the individual mint or
	// provision call with a named error instead of failing startup. This is the
	// same asymmetry as the callers' TENANCY_API_URL — only settings whose
	// absence is silently dangerous (the encryption key, the caller keys)
	// refuse to boot.
	DimoAuthURL         url.URL `yaml:"DIMO_AUTH_URL"`
	TokenExchangeURL    url.URL `yaml:"TOKEN_EXCHANGE_URL"`
	VehicleNftAddress   string  `yaml:"VEHICLE_NFT_ADDRESS"`
	IdentityAPIEndpoint url.URL `yaml:"IDENTITY_API_ENDPOINT"`
	AccountsAPIEndpoint url.URL `yaml:"ACCOUNTS_API_ENDPOINT"`

	// The group-attestation publisher (P4 of the groups move). AttestAPIURL
	// receives dimo.document.vehicle.groups CloudEvents; ChainID is part of
	// both the subject DID (did:erc721:<chain>:...) and nothing else here.
	// Not boot-required for the same reason as the minter settings above —
	// only the publish-group-attestations command needs them, and it fails
	// with a named error when they are absent.
	AttestAPIURL url.URL `yaml:"ATTEST_API_URL"`
	ChainID      int64   `yaml:"CHAIN_ID"`

	// TrustedCallerKeys is the pre-shared key set that gates /v1, formatted
	// "name:key,name:key". The name is for logging and revocation only; it is
	// the key that authenticates.
	//
	// This answers "is this a trusted application?". It deliberately does not
	// answer "which tenant may it act for?" — the developer-license JWT and the
	// scope rule do that, and collapsing the two would make every key holder
	// able to read every tenant.
	//
	// One key per caller rather than one shared key, so a single caller can be
	// rotated or revoked without a coordinated redeploy of the others, and so a
	// rejected request names who was rejected.
	TrustedCallerKeys string `yaml:"TRUSTED_CALLER_KEYS"`
}

// IsLocal is "local" exactly. Anything else — including an unset ENVIRONMENT or
// a typo like "localdev" — fails closed, so a misconfigured deployment refuses
// to boot rather than quietly encrypting under the weak key. Matches
// fleet-lite-app's definition.
func (s *Settings) IsLocal() bool {
	return s.Environment == "local"
}

// Validate rejects configurations that would silently do the wrong thing.
//
// The empty-encryption-key case is the reason this exists. sha256("") is a valid
// AES-256 key, so encryption succeeds, the ciphertext looks fine, and every
// tenant's DIMO developer-license private key ends up protected by a constant
// that is public knowledge. Nothing errors and nothing logs — it can only be
// caught here. This exact failure reached production in fleet-lite-app.
func (s *Settings) Validate() error {
	if s.TenantSecretEncKey == "" && !s.IsLocal() {
		return fmt.Errorf("TENANT_SECRET_ENC_KEY is empty in environment %q: tenant "+
			"credentials would be encrypted with sha256(\"\"), a publicly known key",
			s.Environment)
	}
	// Same reasoning as the encryption key: an unset value here is not "no
	// gate", it is an open /v1. Refuse to boot rather than serve every tenant's
	// authorization data to anything that can reach the port.
	if !s.IsLocal() {
		keys, err := s.ParsedTrustedCallerKeys()
		if err != nil {
			return fmt.Errorf("TRUSTED_CALLER_KEYS is invalid: %w", err)
		}
		if len(keys) == 0 {
			return fmt.Errorf("TRUSTED_CALLER_KEYS is empty in environment %q: /v1 would "+
				"accept any caller that can reach the port", s.Environment)
		}
	}
	return nil
}

// MinTrustedCallerKeyLength rejects keys short enough to be guessable. 32 chars
// is what `openssl rand -hex 16` produces; the provisioning docs specify 32
// bytes of entropy, and this is the floor rather than the target.
const MinTrustedCallerKeyLength = 32

// ParsedTrustedCallerKeys parses "name:key,name:key" into name -> key.
//
// Validation is strict on purpose: a malformed entry silently dropped would
// mean a caller that cannot authenticate and a very confusing afternoon. Every
// problem is an error at boot instead.
func (s *Settings) ParsedTrustedCallerKeys() (map[string]string, error) {
	out := map[string]string{}
	raw := strings.TrimSpace(s.TrustedCallerKeys)
	if raw == "" {
		return out, nil
	}
	seenKey := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, key, found := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		// Trimmed to match the transport. HTTP strips leading and trailing
		// whitespace from header values, so a caller presenting "k " sends "k".
		// If the stored value kept a trailing newline — exactly what a careless
		// `openssl rand | aws put-secret` produces, and what nearly bit the
		// encryption key — the configured key would never match anything a
		// caller could send, and the failure would look like a wrong key rather
		// than a stray byte.
		key = strings.TrimSpace(key)
		if !found || name == "" || key == "" {
			return nil, fmt.Errorf("entry %q is not name:key", entry)
		}
		if len(key) < MinTrustedCallerKeyLength {
			return nil, fmt.Errorf("key for %q is %d chars, minimum is %d",
				name, len(key), MinTrustedCallerKeyLength)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("caller %q appears more than once", name)
		}
		if other, dup := seenKey[key]; dup {
			return nil, fmt.Errorf("callers %q and %q share a key, which defeats per-caller revocation", other, name)
		}
		seenKey[key] = name
		out[name] = key
	}
	return out, nil
}
