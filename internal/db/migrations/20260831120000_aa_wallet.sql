-- +goose Up
-- +goose StatementBegin

-- The tenant's own AA wallet (docs/plans/08-aa-owner-signing.md): a Kernel
-- v3.1 smart account that owns fleet vehicles, plus the root EOA key that is
-- its sudo validator, encrypted exactly like signer_key_enc under
-- TENANT_SECRET_ENC_KEY. When a vehicle's live owner IS this wallet, the
-- sharing engine signs in owner mode (sudo) instead of requiring the
-- owner-registered-our-signer arrangement.
--
-- On tenant_credentials rather than its own table because the wallet rides on
-- the credential (decision D2): effective-credential resolution — own row if
-- it holds a license, else the parent's — is what lets an operator-managed
-- customer share vehicles the operator's wallet owns, with no second
-- resolution rule to disagree with the first.
--
-- The CHECK is the both-or-neither invariant: an address without a key can
-- never sign, and a key without an address can never be matched against an
-- owner. Either half alone is a misconfiguration this schema refuses to hold.

ALTER TABLE tenant_credentials
    ADD COLUMN aa_wallet_address VARCHAR(43),
    ADD COLUMN aa_wallet_key_enc TEXT,
    ADD CONSTRAINT tenant_credentials_aa_wallet_pair CHECK (
        (aa_wallet_address IS NULL) = (aa_wallet_key_enc IS NULL)
    );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE tenant_credentials
    DROP CONSTRAINT tenant_credentials_aa_wallet_pair,
    DROP COLUMN aa_wallet_key_enc,
    DROP COLUMN aa_wallet_address;

-- +goose StatementEnd
