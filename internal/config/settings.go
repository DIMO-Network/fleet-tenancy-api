package config

import (
	"fmt"
	"net/url"

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
	return nil
}
