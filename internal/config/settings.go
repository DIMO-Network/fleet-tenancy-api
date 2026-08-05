package config

import (
	"fmt"
	"net/url"
)

// Settings is the service configuration, loaded from settings.yaml or the
// environment. Field names mirror fleet-lite-app and kaufmann-oracle so the
// three deploy the same way.
type Settings struct {
	Environment string `yaml:"ENVIRONMENT"`
	LogLevel    string `yaml:"LOG_LEVEL"`
	ServiceName string `yaml:"SERVICE_NAME"`

	APIPort        string `yaml:"API_PORT"`
	MonitoringPort string `yaml:"MONITORING_PORT"`

	DBUser               string `yaml:"DB_USER"`
	DBPassword           string `yaml:"DB_PASSWORD"`
	DBHost               string `yaml:"DB_HOST"`
	DBPort               string `yaml:"DB_PORT"`
	DBName               string `yaml:"DB_NAME"`
	DBSSLMode            string `yaml:"DB_SSL_MODE"`
	DBMaxOpenConnections int    `yaml:"DB_MAX_OPEN_CONNECTIONS"`
	DBMaxIdleConnections int    `yaml:"DB_MAX_IDLE_CONNECTIONS"`

	// JwtKeySetURL verifies both end-user JWTs and developer-license JWTs —
	// same DIMO issuer for both.
	JwtKeySetURL url.URL `yaml:"JWT_KEY_SET_URL"`

	// TenantSecretEncKey derives the AES-256-GCM key for credentials at rest.
	TenantSecretEncKey string `yaml:"TENANT_SECRET_ENC_KEY"`
}

func (s *Settings) IsLocal() bool { return s.Environment == "local" || s.Environment == "localdev" }

// Validate rejects configurations that would silently do the wrong thing.
//
// An empty TenantSecretEncKey is the one that matters: sha256("") is a valid
// 32-byte AES key, so encryption succeeds and every stored credential is
// protected by a constant anyone can compute. Nothing errors, nothing logs. It
// has to be caught at startup or not at all.
func (s *Settings) Validate() error {
	if s.TenantSecretEncKey == "" && !s.IsLocal() {
		return fmt.Errorf("TENANT_SECRET_ENC_KEY is empty in environment %q: "+
			"credentials would be encrypted with sha256(\"\"), a publicly known key", s.Environment)
	}
	return nil
}
