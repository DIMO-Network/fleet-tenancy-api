package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog"
)

var (
	// ErrClientIDRegistered is returned when the developer license is already
	// registered to a tenant. The unique index on lower(dimo_client_id) is
	// what makes license → tenant resolution unambiguous; a second holder is
	// refused, never silently merged.
	ErrClientIDRegistered = errors.New("that developer license is already registered to a tenant")

	// ErrCredentialInvalid is returned when the supplied client id + API key
	// cannot mint a developer JWT. The caller's input, not our fault: 400.
	ErrCredentialInvalid = errors.New("could not authenticate with DIMO using the provided client ID and API key")
)

// credentialValidator is the slice of CredentialService self-serve creation
// needs: proof that a client id + key can actually mint. An interface so the
// database halves are testable without a live DIMO auth exchange.
type credentialValidator interface {
	ValidateCredential(clientID, apiKeyPlain string) error
}

// SelfServeService creates unparented self-serve tenants and sets tenant
// credentials — the two writes that close the last un-cutover path:
// fleet-lite's POST /tenants used to write only its own table, and since the
// authz cutover a tenant this service has never heard of cannot be opened at
// all.
type SelfServeService struct {
	logger   *zerolog.Logger
	pdb      *db.Store
	settings *config.Settings
	tenants  *TenantService
	creds    credentialValidator
}

func NewSelfServeService(logger *zerolog.Logger, pdb *db.Store, settings *config.Settings,
	tenants *TenantService, creds credentialValidator) *SelfServeService {
	return &SelfServeService{logger: logger, pdb: pdb, settings: settings, tenants: tenants, creds: creds}
}

// Create writes a self-serve tenant, its credential and its owner membership
// in ONE transaction.
//
// Atomicity is what makes the caller's retry story work: the only way a
// second attempt can hit the client-id conflict is if the first attempt
// committed — in which case it succeeded and the caller's failure was
// downstream of here. A tenant without its credential, or without its owner,
// never exists.
//
// The id is minted HERE and returned: this service is the authority on tenant
// identity, and the caller materialises its local row under the same uuid —
// the same key trick the backfill used, run forward instead of backward. A
// caller that commits remotely and fails locally converges through its own
// mirror path rather than diverging.
func (s *SelfServeService) Create(ctx context.Context, in *models.CreateSelfServeInput) (*models.Tenant, error) {
	name := strings.TrimSpace(in.Name)
	switch {
	case name == "":
		return nil, fmt.Errorf("name is required")
	case in.ClientID == "" || in.APIKey == "":
		return nil, fmt.Errorf("clientId and apiKey are required")
	case in.OwnerWallet == "":
		return nil, fmt.Errorf("ownerWallet is required")
	}

	// Prove the credential mints BEFORE anything persists — the same order
	// fleet-lite's own create has always used, for the same reason: a tenant
	// with a dead credential is a support case, not a tenant.
	if err := s.creds.ValidateCredential(in.ClientID, in.APIKey); err != nil {
		s.logger.Warn().Err(err).Str("client_id", in.ClientID).Msg("self-serve credential validation failed")
		return nil, ErrCredentialInvalid
	}

	enc, err := EncryptSecret(s.settings.TenantSecretEncKey, in.APIKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt API key: %w", err)
	}

	ownerWallet := common.HexToAddress(in.OwnerWallet).Hex()
	// The owner preset — the same mapping fleet-lite's write-through uses.
	ownerPerms, err := json.Marshal([]string{"manage_members", "manage_settings"})
	if err != nil {
		return nil, fmt.Errorf("marshal owner permissions: %w", err)
	}

	tx, err := s.pdb.DBS().Writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Unparented, unmanaged, implicit: a self-serve tenant's fleet is
	// everything its own license is privileged on — exactly today's fleet-lite
	// behaviour, which is the coexistence guarantee.
	var id string
	if err = tx.QueryRowContext(ctx,
		`INSERT INTO tenants (name, kind, managed, entitlement_mode)
		 VALUES ($1, $2, FALSE, $3)
		 RETURNING id`,
		name, models.KindCustomer, models.EntitlementImplicit).Scan(&id); err != nil {
		return nil, fmt.Errorf("insert tenant: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO tenant_credentials (tenant_id, dimo_client_id, dimo_api_key_enc)
		 VALUES ($1, $2, $3)`, id, in.ClientID, enc); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrClientIDRegistered
		}
		return nil, fmt.Errorf("insert credential: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO users (wallet, email) VALUES ($1, NULLIF($2, ''))
		 ON CONFLICT (wallet) DO UPDATE SET
		   email = COALESCE(NULLIF(EXCLUDED.email, ''), users.email),
		   updated_at = NOW()`, ownerWallet, in.OwnerEmail); err != nil {
		return nil, fmt.Errorf("upsert owner user: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO memberships (tenant_id, wallet, role, permissions, scope_group_ids, granted_by_wallet)
		 VALUES ($1, $2, 'owner', $3, NULL, $2)`, id, ownerWallet, ownerPerms); err != nil {
		return nil, fmt.Errorf("insert owner membership: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.logger.Info().Str("tenant_id", id).Str("name", name).
		Str("client_id", in.ClientID).Str("owner", ownerWallet).
		Msg("self-serve tenant created")
	return s.tenants.Get(ctx, id)
}

// SetCredentials replaces a tenant's own developer license, validated by
// minting first. This is fleet-lite's Settings screen for self-serve tenants
// — and the graduation path for a managed customer being handed its own
// license, at which point it stops resolving to its operator's.
func (s *SelfServeService) SetCredentials(ctx context.Context, tenantID string, in *models.SetCredentialsInput) error {
	if in.ClientID == "" || in.APIKey == "" {
		return fmt.Errorf("clientId and apiKey are required")
	}
	if err := s.creds.ValidateCredential(in.ClientID, in.APIKey); err != nil {
		s.logger.Warn().Err(err).Str("client_id", in.ClientID).Msg("credential rotation validation failed")
		return ErrCredentialInvalid
	}
	enc, err := EncryptSecret(s.settings.TenantSecretEncKey, in.APIKey)
	if err != nil {
		return fmt.Errorf("encrypt API key: %w", err)
	}

	var exists bool
	err = s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT true FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("load tenant %s: %w", tenantID, err)
	}

	if _, err = s.pdb.DBS().Writer.ExecContext(ctx,
		`INSERT INTO tenant_credentials (tenant_id, dimo_client_id, dimo_api_key_enc)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id) DO UPDATE SET
		   dimo_client_id = EXCLUDED.dimo_client_id,
		   dimo_api_key_enc = EXCLUDED.dimo_api_key_enc,
		   updated_at = NOW()`, tenantID, in.ClientID, enc); err != nil {
		if isUniqueViolation(err) {
			return ErrClientIDRegistered
		}
		return fmt.Errorf("upsert credential: %w", err)
	}

	s.logger.Info().Str("tenant_id", tenantID).Str("client_id", in.ClientID).
		Msg("tenant credentials replaced")
	return nil
}
