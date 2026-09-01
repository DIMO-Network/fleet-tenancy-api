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
	Environment    string `yaml:"ENVIRONMENT"`
	LogLevel       string `yaml:"LOG_LEVEL"`
	ServiceName    string `yaml:"SERVICE_NAME"`
	APIPort        int    `yaml:"API_PORT"`
	MonitoringPort int    `yaml:"MONITORING_PORT"`

	// WebhookPort serves the ONE publicly reachable surface — Postmark's
	// delivery webhook — on its own listener, so the chart's ingress can
	// target it without /v1 being reachable from the internet even if that
	// ingress is later misconfigured. See app.WebhookApp.
	WebhookPort int         `yaml:"WEBHOOK_PORT"`
	DB          db.Settings `yaml:"DB"` // secret

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

	// On-chain vehicle sharing (docs/HANDOFF.md, "Vehicle sharing"). A share is
	// one SACD setPermissions0 call, sent as a UserOp from the vehicle owner's
	// kernel account and signed by the acting tenant's signer key — the same
	// mechanism kaufmann-oracle uses to re-share a transferred vehicle, pointed
	// at a grantee the customer chooses.
	//
	// SacdAddress is the DIMO SACD contract. It is NOT the registry and not the
	// VehicleId NFT: three different addresses, and setPermissions0 sent to the
	// wrong one fails in ways that look like a permissions bug. Polygon prod is
	// 0x3c152B5d96769661008Ff404224d6530FCAC766d.
	//
	// RPCURL and BundlerURL carry API keys and are secrets. The bundler URL is
	// used as the paymaster URL too, matching kaufmann's fleet client — ZeroDev
	// serves both from one project URL.
	//
	// Sharing is optional as a whole: leave all of these unset and the feature
	// is off, the job queue never starts, and the service boots normally. That
	// asymmetry is deliberate — this service is what both apps fail closed on,
	// and a feature neither of them has to use must not be able to stop it
	// answering /v1/authz.
	//
	// What Validate does refuse is a PARTIAL configuration. Half-set, the
	// feature would look configured to an operator reading the chart and be
	// silently off to SharingConfigured, or worse, on with an address pointing
	// at the wrong contract. All or nothing is the only state worth booting.
	//
	// SyntheticNftAddress is the SyntheticDeviceId NFT contract, the target of
	// the burn_synthetic shared operation (plan 06 step 3). A fourth address to
	// keep apart from the three above; the Polygon prod value matches
	// kaufmann-oracle's SYNTHETIC_NFT_ADDRESS, which runs the same burn today.
	// It joins the all-or-nothing set below because its absence is the
	// dangerous kind: half-configured, a synthetic-device burn would be aimed
	// at the zero address.
	SacdAddress string `yaml:"SACD_ADDRESS"`
	// SacdUploadURL pins a share's SACD document and answers with its CID; the
	// grant records `ipfs://<cid>` as its source. Without a document a grantee
	// gets telemetry but no glovebox — dimo-app-backend reads the cloudevent
	// agreements from this document, and permission bits do not substitute.
	// Empty disables publishing and shares degrade to that older behaviour.
	SacdUploadURL       string  `yaml:"SACD_UPLOAD_URL"`
	SyntheticNftAddress string  `yaml:"SYNTHETIC_NFT_ADDRESS"`
	RPCURL              url.URL `yaml:"RPC_URL"` // secret
	// BundlerURL also carries owner-mode operations — UserOps sent from a
	// tenant's own AA wallet (docs/plans/08-aa-owner-signing.md). One project
	// for both signing paths, decided 2026-09-01: sponsorship is per-project
	// and the sponsoring project was confirmed to be this one, so a second URL
	// holding the same value would be two copies of one fact.
	BundlerURL url.URL `yaml:"BUNDLER_URL"` // secret

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

	// Access-granted notification (Postmark transactional email), sent when a
	// member is provisioned into a tenant. The whole feature is optional: an
	// empty server token means no email is sent and provisioning reports
	// emailSent=false — deliberately NOT boot-required, so the service runs in
	// environments where the Postmark side has not been set up.
	//
	// PostmarkServerToken authenticates against the Postmark API (server-scoped).
	// ProvisionEmailFrom must be a verified Postmark sender signature/domain.
	// ProvisionTemplateAlias must exist in that Postmark server — see
	// templates/postmark/ and the push-postmark-templates command.
	// FleetAppBaseURL is the customer app's public origin, used for the
	// sign-in link in the email.
	PostmarkServerToken    string  `yaml:"POSTMARK_SERVER_TOKEN"` // secret
	ProvisionEmailFrom     string  `yaml:"PROVISION_EMAIL_FROM"`
	ProvisionTemplateAlias string  `yaml:"POSTMARK_PROVISION_TEMPLATE_ALIAS"`
	FleetAppBaseURL        url.URL `yaml:"FLEET_APP_BASE_URL"`

	// Email invitations (docs/plans/04-invitations-into-tenancy.md). Key names
	// match fleet-lite-app's exactly — the flow moved here, and identical names
	// keep the Postmark-side configuration (template aliases, webhook secret)
	// portable between the two while both exist.
	//
	// Like the provisioning email, none of these are boot-required: an empty
	// Postmark token means invites are recorded but report emailSent=false, and
	// an empty webhook secret disables the /webhooks/postmark endpoint.
	//
	// InviteAcceptURLBase is where the emailed accept link points. Every accept
	// happens in fleet-lite regardless of who sent the invite — operator-sent
	// invites link to the same page — so there is one configured base, not one
	// per surface.
	PostmarkWebhookSecret   string  `yaml:"POSTMARK_WEBHOOK_SECRET"` // secret
	InvitationFromEmail     string  `yaml:"INVITATION_FROM_EMAIL"`
	InvitationTemplateAlias string  `yaml:"POSTMARK_INVITATION_TEMPLATE_ALIAS"`
	InviteExpiryHours       int     `yaml:"INVITE_EXPIRY_HOURS"`
	InviteAcceptURLBase     url.URL `yaml:"INVITE_ACCEPT_URL_BASE"`
}

// IsLocal is "local" exactly. Anything else — including an unset ENVIRONMENT or
// a typo like "localdev" — fails closed, so a misconfigured deployment refuses
// to boot rather than quietly encrypting under the weak key. Matches
// fleet-lite-app's definition.
func (s *Settings) IsLocal() bool {
	return s.Environment == "local"
}

// SharingConfigured reports whether on-chain vehicle sharing has everything it
// needs to run. Every input is required: without the SACD address there is no
// contract to call, and without the RPC and bundler URLs there is no way to
// send the UserOp.
//
// Callers use this to answer "is this feature on?" in one place rather than
// re-deriving it from four fields — and, until the step-2 PR makes these
// boot-required, so the share endpoint can fail with a named error instead of
// a nil-pointer panic in an unconfigured environment.
func (s *Settings) SharingConfigured() bool {
	return s.SacdAddress != "" &&
		s.SyntheticNftAddress != "" &&
		s.VehicleNftAddress != "" &&
		s.RPCURL.String() != "" &&
		s.BundlerURL.String() != "" &&
		s.ChainID != 0
}

// OwnerModeConfigured reports whether owner-mode signing — UserOps sent from a
// tenant's own AA wallet (docs/plans/08-aa-owner-signing.md) — can run. It is
// exactly SharingConfigured: owner mode uses the same ZeroDev project as the
// shared-signer path (decided 2026-09-01 — the sponsoring project was
// confirmed and BUNDLER_URL is it). The feature's real switch is per tenant:
// no aa_wallet row on the effective credential, no owner mode. Kept as its own
// method so the three surfaces that consult it — the authorizer, the display
// gate, the workers — keep naming the question they ask, and so a dedicated
// switch, if one is ever wanted, is a one-line change here rather than three.
func (s *Settings) OwnerModeConfigured() bool {
	return s.SharingConfigured()
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
	// Vehicle sharing: all of it or none of it. A partial configuration is the
	// dangerous state — an operator who set SACD_ADDRESS and forgot the bundler
	// reads the chart as "sharing is on", while SharingConfigured reads it as
	// off and every share 503s with nothing explaining why.
	//
	// Local is exempt for the same reason as everything else here: a developer
	// running against a local database has no bundler and should not need one.
	if !s.IsLocal() {
		if err := s.validateSharing(); err != nil {
			return err
		}
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

// validateSharing refuses a half-configured sharing feature.
//
// Nothing set at all is fine — the feature is simply off. Anything set means
// everything must be, including the two that are also needed elsewhere
// (VEHICLE_NFT_ADDRESS, CHAIN_ID), because a share signed for the wrong chain
// or aimed at the zero address fails in ways that read as a permissions bug.
func (s *Settings) validateSharing() error {
	present := map[string]bool{
		"SACD_ADDRESS":          s.SacdAddress != "",
		"SYNTHETIC_NFT_ADDRESS": s.SyntheticNftAddress != "",
		"RPC_URL":               s.RPCURL.String() != "",
		"BUNDLER_URL":           s.BundlerURL.String() != "",
		"VEHICLE_NFT_ADDRESS":   s.VehicleNftAddress != "",
		"CHAIN_ID":              s.ChainID != 0,
	}
	missing := []string{}
	any := false
	for _, name := range []string{"SACD_ADDRESS", "SYNTHETIC_NFT_ADDRESS", "RPC_URL", "BUNDLER_URL", "VEHICLE_NFT_ADDRESS", "CHAIN_ID"} {
		if present[name] {
			any = true
		} else {
			missing = append(missing, name)
		}
	}
	if !any || len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("vehicle sharing is partially configured in environment %q: %s missing. "+
		"Set all of them to enable sharing, or none to leave it off",
		s.Environment, strings.Join(missing, ", "))
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
