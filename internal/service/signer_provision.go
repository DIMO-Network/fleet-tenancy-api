package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// ErrSignerExists is returned when provisioning finds a signer already in
// place. It is a refusal, not a failure: overwriting an existing signer is the
// orphaning event plan 06 is built around avoiding — the old key is the one
// some kernel accounts registered as their validator, and those vehicles would
// become permanently unsignable with no error naming the cause.
var ErrSignerExists = errors.New("tenant already has a signer")

// ProvisionResult reports what provisioning did — the address is safe to log;
// the key is already encrypted and gone.
type ProvisionResult struct {
	TenantID      string
	SignerAddress string
	// EffectiveUnused is set when the tenant's effective credential would not
	// serve this signer — its credential row holds no dimo_client_id, so
	// callers resolve to the parent's credential and the signer just written
	// will never sign anything. Surfaced rather than refused: the row may be
	// about to receive a client id as part of the same provisioning flow.
	EffectiveUnused bool
}

// ProvisionSigner generates and stores a signer for ONE tenant that has none.
//
// CREATE-IF-ABSENT ONLY, AND THE CONDITION LIVES IN SQL. A guard in Go would
// hold between the read and the write of two racing runs; the WHERE clause
// cannot. There is deliberately no overwrite path and no rotate path — see
// docs/signer-permanence.md for why the absence is a decision, not an
// omission.
//
// The generated key is hex WITHOUT a 0x prefix, which is the format the share
// path parses (sharing.go's HexToECDSA does not trim a prefix — signer-diff
// warns if a prefixed key ever appears).
func (s *CredentialService) ProvisionSigner(ctx context.Context, tenantID string) (*ProvisionResult, error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate signer key: %w", err)
	}
	address := crypto.PubkeyToAddress(key.PublicKey).Hex()
	privHex := hexutil.Encode(crypto.FromECDSA(key))[2:]

	keyEnc, err := EncryptSecret(s.settings.TenantSecretEncKey, privHex)
	if err != nil {
		return nil, fmt.Errorf("encrypt signer key: %w", err)
	}

	// The guard treats an empty string like NULL: an empty ciphertext is not a
	// signer, and refusing to replace it would make a half-written row
	// permanent.
	res, err := s.pdb.DBS().Writer.ExecContext(ctx, `
		INSERT INTO tenant_credentials (tenant_id, signer_address, signer_key_enc)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (tenant_id) DO UPDATE
		   SET signer_address = EXCLUDED.signer_address,
		       signer_key_enc = EXCLUDED.signer_key_enc,
		       updated_at     = NOW()
		 WHERE tenant_credentials.signer_key_enc IS NULL
		    OR tenant_credentials.signer_key_enc = ''`,
		tenantID, address, keyEnc)
	if err != nil {
		return nil, fmt.Errorf("provision signer for tenant %s: %w", tenantID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSignerExists, tenantID)
	}

	out := &ProvisionResult{TenantID: tenantID, SignerAddress: address}

	// Would anything use it? The effective credential requires a client id on
	// the holding row, so a signer on a row without one is inert.
	var hasClientID bool
	if err := s.pdb.DBS().Reader.QueryRowContext(ctx, `
		SELECT dimo_client_id IS NOT NULL AND dimo_client_id <> ''
		  FROM tenant_credentials WHERE tenant_id = $1::uuid`,
		tenantID).Scan(&hasClientID); err == nil && !hasClientID {
		out.EffectiveUnused = true
	}

	s.logger.Info().Str("tenant_id", tenantID).Str("signer_address", address).
		Bool("effective_unused", out.EffectiveUnused).Msg("signer provisioned")
	return out, nil
}
