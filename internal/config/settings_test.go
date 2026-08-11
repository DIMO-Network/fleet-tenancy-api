package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	// Trusted-caller keys are required outside local too, so a valid prod
	// configuration now needs both. The intent of this test is unchanged: a
	// fully configured prod settings object validates.
	s := &Settings{
		Environment:        "prod",
		TenantSecretEncKey: "a-real-key",
		TrustedCallerKeys:  "fleet-lite-app:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	if err := s.Validate(); err != nil {
		t.Errorf("expected no error with a key set, got %v", err)
	}
}

func TestParsedTrustedCallerKeys(t *testing.T) {
	const k1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const k2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	t.Run("parses name:key pairs", func(t *testing.T) {
		s := &Settings{TrustedCallerKeys: "fleet-lite-app:" + k1 + ",kaufmann-oracle:" + k2}
		got, err := s.ParsedTrustedCallerKeys()
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"fleet-lite-app": k1, "kaufmann-oracle": k2}, got)
	})

	t.Run("tolerates spacing around entries", func(t *testing.T) {
		s := &Settings{TrustedCallerKeys: " fleet-lite-app:" + k1 + " , kaufmann-oracle:" + k2 + " "}
		got, err := s.ParsedTrustedCallerKeys()
		require.NoError(t, err)
		assert.Len(t, got, 2)
	})

	// Every one of these is an error rather than a silently dropped entry: a
	// caller that cannot authenticate for an invisible reason is far worse to
	// diagnose than a service that refuses to boot and says why.
	for _, tc := range []struct{ name, in, wants string }{
		{"missing colon", "fleet-lite-app" + k1, "not name:key"},
		{"empty name", ":" + k1, "not name:key"},
		{"empty key", "fleet-lite-app:", "not name:key"},
		{"key too short", "fleet-lite-app:abc123", "minimum"},
		{"duplicate name", "a:" + k1 + ",a:" + k2, "more than once"},
		{"shared key", "a:" + k1 + ",b:" + k1, "share a key"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			s := &Settings{TrustedCallerKeys: tc.in}
			_, err := s.ParsedTrustedCallerKeys()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wants)
		})
	}

	t.Run("empty is not an error on its own — Validate decides", func(t *testing.T) {
		s := &Settings{TrustedCallerKeys: ""}
		got, err := s.ParsedTrustedCallerKeys()
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestValidateRequiresTrustedCallerKeysOutsideLocal(t *testing.T) {
	const k1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("prod without keys refuses to boot", func(t *testing.T) {
		s := &Settings{Environment: "prod", TenantSecretEncKey: "x"}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "TRUSTED_CALLER_KEYS")
	})

	t.Run("prod with keys is fine", func(t *testing.T) {
		s := &Settings{Environment: "prod", TenantSecretEncKey: "x", TrustedCallerKeys: "fleet-lite-app:" + k1}
		assert.NoError(t, s.Validate())
	})

	t.Run("prod with malformed keys refuses to boot", func(t *testing.T) {
		s := &Settings{Environment: "prod", TenantSecretEncKey: "x", TrustedCallerKeys: "nope"}
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid")
	})

	t.Run("local without keys still boots", func(t *testing.T) {
		s := &Settings{Environment: "local"}
		assert.NoError(t, s.Validate())
	})
}

// The stored value is trimmed because HTTP already trims what callers present.
// Without this, a secret saved with a trailing newline could never match.
func TestTrustedCallerKeyStoredWithTrailingNewlineStillMatches(t *testing.T) {
	const k1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s := &Settings{TrustedCallerKeys: "fleet-lite-app:" + k1 + "\n"}
	got, err := s.ParsedTrustedCallerKeys()
	require.NoError(t, err)
	assert.Equal(t, k1, got["fleet-lite-app"])
}
