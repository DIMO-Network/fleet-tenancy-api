package config

import "testing"

// The empty-key case is the reason Validate exists: sha256("") is a valid AES-256
// key, so encryption succeeds silently and every credential is protected by a
// constant anyone can compute. This is the exact failure that reached production
// in fleet-lite-app.
func TestValidate_RejectsEmptyEncKeyOutsideLocal(t *testing.T) {
	for _, env := range []string{"prod", "dev", "staging"} {
		s := &Settings{Environment: env, TenantSecretEncKey: ""}
		if err := s.Validate(); err == nil {
			t.Errorf("environment %q: expected an error for an empty TENANT_SECRET_ENC_KEY, got nil", env)
		}
	}
}

func TestValidate_AllowsEmptyEncKeyLocally(t *testing.T) {
	s := &Settings{Environment: "local", TenantSecretEncKey: ""}
	if err := s.Validate(); err != nil {
		t.Errorf("expected no error for ENVIRONMENT=local, got %v", err)
	}
}

// Anything that is not exactly "local" must fail closed. An unset ENVIRONMENT is
// the dangerous one: treating it as local would skip the check in a real
// deployment, which is the precise failure this guard exists to prevent.
func TestValidate_FailsClosedOnAmbiguousEnvironment(t *testing.T) {
	for _, env := range []string{"", "localdev", "Local", "LOCAL", "dev-local"} {
		s := &Settings{Environment: env, TenantSecretEncKey: ""}
		if err := s.Validate(); err == nil {
			t.Errorf("environment %q: expected an error, got nil", env)
		}
	}
}

func TestValidate_AllowsSetEncKey(t *testing.T) {
	s := &Settings{Environment: "prod", TenantSecretEncKey: "a-real-key"}
	if err := s.Validate(); err != nil {
		t.Errorf("expected no error with a key set, got %v", err)
	}
}
