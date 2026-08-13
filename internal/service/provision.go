package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog"
)

// ErrNoSignerAddress is returned when provisioning must create a DIMO account
// but the effective credential has no signer to register on it. accounts-api
// requires one on creation; without it the flow can look up but not create.
var ErrNoSignerAddress = errors.New("effective credential has no signer address, cannot create accounts")

// ErrUpstream wraps failures of the DIMO platform services provisioning talks
// to. Controllers map it to 502: the request was fine, the dependency was not,
// and a caller retrying is correct — which 4xx would wrongly discourage and a
// bare 500 would wrongly blame on this service.
var ErrUpstream = errors.New("upstream service failed")

// credentialProvider is the slice of CredentialService provisioning needs,
// narrowed so tests can fake the minting — which reaches auth and identity —
// while the membership write runs against a real database.
type credentialProvider interface {
	DeveloperJWT(ctx context.Context, tenantID string) (*models.MintedToken, error)
	Effective(ctx context.Context, tenantID string) (*EffectiveCredential, error)
}

// ProvisionService adds a member to a tenant from an email address. It is the
// on-behalf half of member management: the console knows who to add but not
// their wallet, and the wallet is what memberships key on.
type ProvisionService struct {
	logger   *zerolog.Logger
	pdb      *db.Store
	members  *MemberService
	creds    credentialProvider
	accounts gateway.AccountsAPI
}

func NewProvisionService(logger *zerolog.Logger, pdb *db.Store, members *MemberService,
	creds credentialProvider, accounts gateway.AccountsAPI) *ProvisionService {
	return &ProvisionService{logger: logger, pdb: pdb, members: members, creds: creds, accounts: accounts}
}

// Provision resolves the email to a wallet via accounts-api — creating the
// account when none exists — and writes the membership.
//
// The order is lookup, create, then membership write, and a failure at any
// step fails the whole request. The steps are not transactional across
// services and do not need to be: creating a DIMO account that then gets no
// membership leaves a person who can log in to nothing, which is exactly the
// state they were in before the call, and a retry converges because the lookup
// then finds the account the failed attempt created.
func (s *ProvisionService) Provision(ctx context.Context, tenantID string, in *models.ProvisionRequest) (*models.ProvisionResponse, error) {
	if in.Email == "" {
		return nil, fmt.Errorf("email is required")
	}
	// Validate the membership half before touching accounts-api, so a bad
	// request cannot create a DIMO account as a side effect.
	if _, _, present := in.Scope(); !present {
		return nil, fmt.Errorf("scopeGroupIds is required (null for unrestricted, [] for no groups)")
	}

	// All accounts-api calls authenticate as the subject tenant's effective
	// credential: the operator's license when provisioning into a managed
	// customer, the tenant's own otherwise. That is also whose allowlisting
	// determines whether the wallet comes back on lookups.
	minted, err := s.creds.DeveloperJWT(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNoCredential) || errors.Is(err, ErrTenantNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}

	created := false
	wallet := ""
	acct, err := s.accounts.GetAccountByEmail(in.Email, minted.Token)
	switch {
	case err == nil:
		wallet = acct.WalletAddress
	case errors.Is(err, gateway.ErrAccountNotFound):
		cred, credErr := s.creds.Effective(ctx, tenantID)
		if credErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrUpstream, credErr)
		}
		if cred.SignerAddress == "" {
			return nil, ErrNoSignerAddress
		}
		acct, err = s.accounts.CreateAccount(in.Email, cred.SignerAddress, minted.Token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
		}
		wallet = acct.WalletAddress
		created = true
	default:
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}

	if wallet == "" {
		// The Accounts service answers email lookups without the wallet unless
		// the calling license is allowlisted. A membership cannot be written
		// against nothing, and silently keying on the email would fork the
		// user model — every other row in this service keys on wallet.
		return nil, fmt.Errorf("%w: accounts-api returned no wallet for the email — "+
			"is client id %s allowlisted?", ErrUpstream, minted.ClientID)
	}

	if err := s.members.Upsert(ctx, tenantID, wallet, &in.MemberWrite); err != nil {
		return nil, err
	}

	// Record the signer registered on a created account. Best-effort by
	// design: the membership is already written and correct, and this column
	// is provenance ("which signer can act on this person's kernel"), not an
	// authorization input.
	if created {
		cred, credErr := s.creds.Effective(ctx, tenantID)
		if credErr == nil && cred.SignerAddress != "" {
			checksummed := common.HexToAddress(wallet).Hex()
			if _, uerr := s.pdb.DBS().Writer.ExecContext(ctx,
				`UPDATE users SET shared_account_signer_address = $1, updated_at = NOW()
				  WHERE wallet = $2`,
				common.HexToAddress(cred.SignerAddress).Hex(), checksummed); uerr != nil {
				s.logger.Warn().Err(uerr).Str("wallet", checksummed).
					Msg("provision: could not record shared-account signer")
			}
		}
	}

	s.logger.Info().
		Str("tenant_id", tenantID).
		Str("wallet", wallet).
		Bool("created", created).
		Msg("member provisioned")

	member, err := s.members.Get(ctx, tenantID, wallet)
	if err != nil {
		// The write committed; failing the request now would report a grant
		// that happened as one that did not — the exact confusion the
		// write-through exists to prevent. Answer with what was written.
		s.logger.Warn().Err(err).Str("wallet", wallet).Msg("provision: read-back failed")
		member = &models.Member{
			Wallet:      common.HexToAddress(wallet).Hex(),
			Role:        in.Role,
			Permissions: in.Permissions,
		}
	}
	return &models.ProvisionResponse{Created: created, Member: *member}, nil
}
