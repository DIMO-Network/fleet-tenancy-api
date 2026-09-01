package config

import (
	"net/url"
	"strings"
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

// SharingConfigured gates whether the vehicle-sharing queue starts and whether
// the share endpoint can run at all. Every field is load-bearing: a missing
// SACD address means calling the wrong contract, a missing bundler means no way
// to send the UserOp, and a zero chain id would sign for the wrong chain. The
// table asserts each one alone is enough to turn the feature off.
func TestSharingConfigured(t *testing.T) {
	full := func() *Settings {
		return &Settings{
			SacdAddress:         "0x3c152B5d96769661008Ff404224d6530FCAC766d",
			SyntheticNftAddress: "0x4804e8D1661cd1a1e5dDdE1ff458A7f878c0aC6D",
			VehicleNftAddress:   "0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF",
			RPCURL:              mustURL(t, "https://polygon-mainnet.example/v2/key"),
			BundlerURL:          mustURL(t, "https://rpc.zerodev.app/api/v2/bundler/proj"),
			ChainID:             137,
		}
	}

	require.True(t, full().SharingConfigured(), "a fully populated config must be considered configured")

	for name, blank := range map[string]func(*Settings){
		"no SACD address":          func(s *Settings) { s.SacdAddress = "" },
		"no synthetic NFT address": func(s *Settings) { s.SyntheticNftAddress = "" },
		"no vehicle NFT address":   func(s *Settings) { s.VehicleNftAddress = "" },
		"no RPC URL":               func(s *Settings) { s.RPCURL = url.URL{} },
		"no bundler URL":           func(s *Settings) { s.BundlerURL = url.URL{} },
		"no chain id":              func(s *Settings) { s.ChainID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			s := full()
			blank(s)
			assert.False(t, s.SharingConfigured(), "%s must turn sharing off", name)
		})
	}
}

// OwnerModeConfigured layers ON TOP of sharing rather than joining its
// all-or-nothing set: AA_BUNDLER_URL absent means owner mode off with sharing
// untouched, and AA_BUNDLER_URL present cannot compensate for a missing
// sharing setting. Deliberately NOT part of validateSharing — the feature is
// optional the way SACD_UPLOAD_URL is.
func TestOwnerModeConfigured(t *testing.T) {
	full := func() *Settings {
		return &Settings{
			SacdAddress:         "0x3c152B5d96769661008Ff404224d6530FCAC766d",
			SyntheticNftAddress: "0x4804e8D1661cd1a1e5dDdE1ff458A7f878c0aC6D",
			VehicleNftAddress:   "0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF",
			RPCURL:              mustURL(t, "https://polygon-mainnet.example/v2/key"),
			BundlerURL:          mustURL(t, "https://rpc.zerodev.app/api/v2/bundler/proj"),
			AABundlerURL:        mustURL(t, "https://rpc.zerodev.app/api/v3/aa-proj/chain/137"),
			ChainID:             137,
		}
	}

	require.True(t, full().OwnerModeConfigured())

	t.Run("no AA bundler URL turns owner mode off, sharing stays on", func(t *testing.T) {
		s := full()
		s.AABundlerURL = url.URL{}
		assert.False(t, s.OwnerModeConfigured())
		assert.True(t, s.SharingConfigured())
	})

	t.Run("an AA URL cannot stand in for a missing sharing setting", func(t *testing.T) {
		s := full()
		s.BundlerURL = url.URL{}
		assert.False(t, s.OwnerModeConfigured())
	})
}

// Sharing settings are not boot-required, and stay that way. The service is
// load-bearing for two apps that fail closed on /v1/authz, so it must keep
// booting in an environment where sharing is simply off. What Validate does
// enforce is all-or-nothing: see validateSharing and the test below it. Nothing
// set at all is the supported off state; a half-configured feature is not.
func TestValidate_SharingSettingsAreNotBootRequired(t *testing.T) {
	s := &Settings{
		Environment:        "prod",
		TenantSecretEncKey: "a-real-key",
		TrustedCallerKeys:  "fleet-lite:" + strings.Repeat("k", MinTrustedCallerKeyLength),
	}
	require.False(t, s.SharingConfigured(), "precondition: sharing is unconfigured here")
	assert.NoError(t, s.Validate(), "an unconfigured sharing feature must not stop the service booting")
}

func mustURL(t *testing.T, raw string) url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return *u
}

// Sharing is optional as a whole but must not be half-configured. An operator
// who sets the SACD address and forgets the bundler reads the chart as
// "sharing is on" while SharingConfigured reads it as off, and every share
// 503s with nothing in the config explaining why.
func TestValidate_RejectsPartialSharingConfiguration(t *testing.T) {
	base := func() *Settings {
		return &Settings{
			Environment:        "prod",
			TenantSecretEncKey: "a-real-key",
			TrustedCallerKeys:  "fleet-lite:" + strings.Repeat("k", MinTrustedCallerKeyLength),
		}
	}
	full := func() *Settings {
		s := base()
		s.SacdAddress = "0x3c152B5d96769661008Ff404224d6530FCAC766d"
		s.SyntheticNftAddress = "0x4804e8D1661cd1a1e5dDdE1ff458A7f878c0aC6D"
		s.VehicleNftAddress = "0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF"
		s.RPCURL = mustURL(t, "https://polygon-mainnet.example/v2/key")
		s.BundlerURL = mustURL(t, "https://rpc.zerodev.example/api/v2/bundler/proj")
		s.ChainID = 137
		return s
	}

	t.Run("nothing set is fine — the feature is simply off", func(t *testing.T) {
		s := base()
		require.False(t, s.SharingConfigured())
		assert.NoError(t, s.Validate())
	})

	t.Run("everything set is fine", func(t *testing.T) {
		s := full()
		require.True(t, s.SharingConfigured())
		assert.NoError(t, s.Validate())
	})

	for name, blank := range map[string]func(*Settings){
		"SACD_ADDRESS":          func(s *Settings) { s.SacdAddress = "" },
		"SYNTHETIC_NFT_ADDRESS": func(s *Settings) { s.SyntheticNftAddress = "" },
		"RPC_URL":               func(s *Settings) { s.RPCURL = url.URL{} },
		"BUNDLER_URL":           func(s *Settings) { s.BundlerURL = url.URL{} },
		"VEHICLE_NFT_ADDRESS":   func(s *Settings) { s.VehicleNftAddress = "" },
		"CHAIN_ID":              func(s *Settings) { s.ChainID = 0 },
	} {
		t.Run("missing "+name+" refuses to boot", func(t *testing.T) {
			s := full()
			blank(s)
			err := s.Validate()
			require.Error(t, err, "a partial sharing config must not boot")
			assert.Contains(t, err.Error(), name, "the error must name what is missing")
		})
	}

	// A developer running against a local database has no bundler and should
	// not need one to start the service.
	t.Run("local is exempt", func(t *testing.T) {
		s := full()
		s.Environment = "local"
		s.SacdAddress = ""
		assert.NoError(t, s.Validate())
	})
}
