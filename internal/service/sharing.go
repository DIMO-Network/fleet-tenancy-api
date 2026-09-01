package service

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"errors"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog"
)

var (
	// ErrNotEntitled means the vehicle is not in the tenant's fleet. The
	// isolation boundary under D2/D5 is enforced here, not by the chain, so
	// this is the check that keeps one customer from acting on another's
	// vehicle through a shared operator license.
	ErrNotEntitled = errors.New("vehicle is not entitled to this tenant")

	// ErrVehicleUnknown means identity-api has no such vehicle.
	ErrVehicleUnknown = errors.New("vehicle not found")

	// ErrGranteeInvalid covers a grantee that is missing, malformed, the zero
	// address, or the owner itself.
	ErrGranteeInvalid = errors.New("grantee is not a usable address")

	// ErrNoSignerKey means the tenant's effective credential has no signer
	// private key stored, so there is nothing to sign with.
	ErrNoSignerKey = errors.New("tenant has no signer key")

	// ErrNoClientID means the tenant's effective credential holds no usable
	// DIMO client id, so a SACD grant to "the tenant itself" has no address to
	// aim at. Distinct from ErrNoCredential — a credential can exist for
	// minting and still carry a malformed client id.
	ErrNoClientID = errors.New("tenant's effective credential has no usable DIMO client id")
)

// ShareAuthorizer resolves and enforces everything that permits a vehicle
// share. It is used twice per share, deliberately: once by the HTTP handler so
// the customer gets a synchronous answer, and again inside the worker
// immediately before the irreversible call.
//
// The second check is not redundant. A job can sit in the queue while the
// vehicle is transferred or the owner revokes our signer, and acting on the
// handler's older answer would send a grant the current owner never agreed to.
type ShareAuthorizer struct {
	logger   zerolog.Logger
	pdb      *db.Store
	identity gateway.IdentityAPI
	signer   *SharedSignerService
	creds    credentialProvider
	settings *config.Settings
}

func NewShareAuthorizer(logger *zerolog.Logger, pdb *db.Store, identity gateway.IdentityAPI,
	signer *SharedSignerService, creds credentialProvider, settings *config.Settings) *ShareAuthorizer {
	return &ShareAuthorizer{
		logger:   logger.With().Str("component", "share-authorizer").Logger(),
		pdb:      pdb,
		identity: identity,
		signer:   signer,
		creds:    creds,
		settings: settings,
	}
}

// ValidateGrantee checks the grantee independently of any vehicle.
//
// Separate from AuthorizeShare because it is the one check that needs no
// upstream call, so the endpoint can reject an obviously bad address before
// spending an identity-api round trip on it.
func ValidateGrantee(grantee string, owner common.Address) error {
	if !common.IsHexAddress(grantee) {
		return fmt.Errorf("%w: %q is not a hex address", ErrGranteeInvalid, grantee)
	}
	addr := common.HexToAddress(grantee)
	if addr == (common.Address{}) {
		// Granting to the zero address burns the permission into nothing and
		// looks, in the UI, exactly like a share that worked.
		return fmt.Errorf("%w: the zero address", ErrGranteeInvalid)
	}
	if owner != (common.Address{}) && addr == owner {
		// The owner already has every permission by virtue of owning the NFT.
		// A self-share is a no-op the customer would read as success.
		return fmt.Errorf("%w: the grantee is the vehicle's owner", ErrGranteeInvalid)
	}
	return nil
}

// AuthorizeShare runs the chain and returns what a share needs to execute: the
// vehicle's current owner, the key to sign with, and WHICH WAY to sign —
// ownerMode true means the key is the owner's own AA-wallet root key and the
// UserOp goes through the kernel's sudo validator; false means the key is the
// tenant's signer and it goes through the owner's secondary weighted-ECDSA
// validator, exactly as before.
//
// The order is chosen so the cheapest and most local check fails first, and so
// no upstream call is made on behalf of a tenant that has no business asking:
//
//  1. entitlement — is this vehicle in the tenant's fleet at all?
//  2. owner       — who currently holds it? (live, never cached)
//  3. mode        — is the owner the tenant's own AA wallet?
//     (docs/plans/08-aa-owner-signing.md, D5: decided per vehicle from the
//     live owner, never a per-tenant switch — the chain already says who owns
//     each vehicle, and a switch would store that fact twice)
//  4. signer      — otherwise: did that owner authorise this tenant's signer? (live)
//  5. key         — do we actually hold the key the chosen mode signs with?
//
// In owner mode the MaySignFor check is deliberately skipped: the wallet is
// the tenant's own and accounts-api has nothing to say about it. What stands
// in for it is the config-time proof that the stored key controls the wallet's
// sudo validator, plus the owner equality established here against the SAME
// live lookup the signer path uses.
//
// The caller's capability is NOT checked here. That is the request's property,
// checked once at the HTTP boundary; re-reading it at execution time would let
// a membership edit between submit and run cancel work that was permitted when
// it started.
func (a *ShareAuthorizer) AuthorizeShare(ctx context.Context, tenantID string, tokenID int64) (common.Address, *ecdsa.PrivateKey, bool, error) {
	if err := a.assertEntitled(ctx, tenantID, tokenID); err != nil {
		return common.Address{}, nil, false, err
	}

	ownerHex, err := a.identity.VehicleOwner(tokenID)
	if err != nil {
		if errors.Is(err, gateway.ErrVehicleNotFound) {
			return common.Address{}, nil, false, ErrVehicleUnknown
		}
		return common.Address{}, nil, false, fmt.Errorf("resolve owner of vehicle %d: %w", tokenID, err)
	}
	owner := common.HexToAddress(ownerHex)

	cred, err := a.creds.Effective(ctx, tenantID)
	if err != nil {
		return common.Address{}, nil, false, fmt.Errorf("resolve effective credential: %w", err)
	}

	if a.settings.OwnerModeConfigured() && cred.AAWalletAddress != "" &&
		owner == common.HexToAddress(cred.AAWalletAddress) {
		rootPK, err := a.aaWalletKey(ctx, cred.TenantID)
		if err != nil {
			return common.Address{}, nil, false, err
		}
		return owner, rootPK, true, nil
	}

	if err := a.signer.MaySignFor(ctx, tenantID, owner.Hex()); err != nil {
		return common.Address{}, nil, false, err
	}

	signerPK, err := a.signerKeyOf(ctx, cred.TenantID)
	if err != nil {
		return common.Address{}, nil, false, err
	}
	return owner, signerPK, false, nil
}

// GranteeClientID resolves the address a grant_sacd operation grants to: the
// tenant's own DIMO client id, from its effective credential.
//
// The EFFECTIVE credential deliberately, not the tenant's own row. The grant
// exists so the tenant's data access survives a transfer, and data access is
// exercised with the license the tenant actually presents — which for an
// operator-managed customer is its operator's. Granting a client id the tenant
// cannot authenticate as would produce a share that looks complete on-chain
// and does nothing. It is also the same resolution AuthorizeShare's signer
// lookup uses, which keeps "whose signer signs" and "whose client id is
// granted" the same answer — the property the plan calls one signer
// resolution.
func (a *ShareAuthorizer) GranteeClientID(ctx context.Context, tenantID string) (common.Address, error) {
	cred, err := a.creds.Effective(ctx, tenantID)
	if err != nil {
		return common.Address{}, fmt.Errorf("resolve effective credential: %w", err)
	}
	if !common.IsHexAddress(cred.ClientID) {
		// Effective's SQL requires a non-NULL client id, so this is a
		// malformed one — legacy empty strings existed in the sources and the
		// backfill NULLs them, but the guard costs nothing.
		return common.Address{}, ErrNoClientID
	}
	return common.HexToAddress(cred.ClientID), nil
}

// assertEntitled enforces the fleet boundary, and only for explicit-mode
// tenants.
//
// An implicit-mode tenant — an operator, or a self-serve tenant — holds no
// entitlement rows at all; its fleet is whatever its own license is privileged
// on, which this service does not mirror. Requiring a row would refuse every
// share for those tenants. What still bounds them is the signer check: they can
// only act on owners whose kernel accounts registered their signer, which is
// the same authority they hold for transfer and delete.
func (a *ShareAuthorizer) assertEntitled(ctx context.Context, tenantID string, tokenID int64) error {
	var mode string
	err := a.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT entitlement_mode FROM tenants WHERE id = $1`, tenantID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("read entitlement mode of %s: %w", tenantID, err)
	}
	if mode != models.EntitlementExplicit {
		return nil
	}

	var ok bool
	err = a.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT true FROM vehicle_entitlements
		  WHERE tenant_id = $1 AND vehicle_token_id = $2 AND revoked_at IS NULL`,
		tenantID, tokenID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotEntitled
	}
	if err != nil {
		return fmt.Errorf("assert entitled: %w", err)
	}
	return nil
}

// signerKeyOf decrypts the signer private key of the credential-holding tenant
// AuthorizeShare already resolved.
//
// The decrypted key exists only for the duration of the call that uses it and
// goes nowhere else — not to a log, not to a response, not to a cache. Same
// discipline as CredentialService, which is the only other place in this
// service that holds plaintext key material.
func (a *ShareAuthorizer) signerKeyOf(ctx context.Context, credTenantID string) (*ecdsa.PrivateKey, error) {
	var keyEnc sql.NullString
	err := a.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT signer_key_enc FROM tenant_credentials WHERE tenant_id = $1`,
		credTenantID).Scan(&keyEnc)
	if errors.Is(err, sql.ErrNoRows) || !keyEnc.Valid || keyEnc.String == "" {
		return nil, ErrNoSignerKey
	}
	if err != nil {
		return nil, fmt.Errorf("read signer key: %w", err)
	}

	plaintext, err := DecryptSecret(a.settings.TenantSecretEncKey, keyEnc.String)
	if err != nil {
		// GCM authenticates, so this is the wrong master key or a corrupt row.
		// Named without the material, like the credential service does.
		return nil, fmt.Errorf("decrypt signer key of tenant %s: %w", credTenantID, err)
	}
	pk, err := crypto.HexToECDSA(plaintext)
	if err != nil {
		return nil, fmt.Errorf("parse signer key of tenant %s: %w", credTenantID, err)
	}
	return pk, nil
}

// aaWalletKey decrypts the AA wallet root key of the credential-holding
// tenant, under the same one-call lifetime discipline as signerKeyOf. The
// stored form is canonical hex (AAWalletService.Set guarantees it), so a parse
// failure means a corrupt row, not a formatting quirk.
func (a *ShareAuthorizer) aaWalletKey(ctx context.Context, credTenantID string) (*ecdsa.PrivateKey, error) {
	var keyEnc sql.NullString
	err := a.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT aa_wallet_key_enc FROM tenant_credentials WHERE tenant_id = $1`,
		credTenantID).Scan(&keyEnc)
	if errors.Is(err, sql.ErrNoRows) || !keyEnc.Valid || keyEnc.String == "" {
		// The CHECK constraint makes address-without-key unrepresentable, so
		// reaching this means the row changed between the Effective read and
		// now. Named for what it is rather than misreported as a signer issue.
		return nil, fmt.Errorf("AA wallet key of tenant %s vanished during authorization: %w",
			credTenantID, ErrNoAAWallet)
	}
	if err != nil {
		return nil, fmt.Errorf("read AA wallet key: %w", err)
	}

	plaintext, err := DecryptSecret(a.settings.TenantSecretEncKey, keyEnc.String)
	if err != nil {
		return nil, fmt.Errorf("decrypt AA wallet key of tenant %s: %w", credTenantID, err)
	}
	pk, err := crypto.HexToECDSA(plaintext)
	if err != nil {
		return nil, fmt.Errorf("parse AA wallet key of tenant %s: %w", credTenantID, err)
	}
	return pk, nil
}
