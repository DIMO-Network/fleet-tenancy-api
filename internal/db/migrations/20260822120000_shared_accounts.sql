-- +goose Up
-- +goose StatementBegin

-- What accounts-api says about a wallet's shared kernel account, remembered
-- instead of re-asked.
--
-- WHY THIS IS SAFE TO STORE, when the same fact was deliberately NOT cached
-- before (see SharedSignerService's own comment): a shared account's
-- providedSignerAddress is accepted exactly once, at registration, and there is
-- no endpoint anywhere in accounts-api that updates or revokes it — checked in
-- that repo 2026-08-21 and written up in docs/signer-permanence.md. The fact is
-- MONOTONIC. It can go unknown -> known, and never known -> false. That is what
-- makes a stored answer different from a stale cache: the failure mode the
-- earlier decision feared ("offers a share that will be refused") cannot occur
-- for a positive answer.
--
-- The negative is NOT symmetric and is not treated as permanent. A wallet with
-- no shared account today can register one tomorrow, so "checked, has none" is
-- re-checked after a while. Freezing it would permanently hide sharing from
-- anyone who happened to be looked up one minute before their account existed.
--
-- WHY A TABLE AND NOT users.shared_account_signer_address, which already
-- exists: `users` means "a person who is a member of a tenant here". A vehicle
-- owner is a kernel account address, and for the population vehicle sharing
-- actually targets — accounts kaufmann-oracle created — there is no user row
-- and never will be. Writing owner rows into `users` would quietly redefine
-- that table into "wallets we have heard of". This table says what it is: a
-- remembered answer about an external account.
--
-- signer_address NULL means CHECKED AND HAS NONE, which is why checked_at is
-- NOT NULL and separate: a row's existence is the record of having asked.
CREATE TABLE IF NOT EXISTS shared_accounts (
    wallet         VARCHAR(43) PRIMARY KEY,
    -- The signer registered on this wallet's kernel account, EIP-55
    -- checksummed. NULL = accounts-api was asked and reported no shared
    -- account (or no account at all).
    signer_address VARCHAR(43),
    checked_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Only the unresolved rows are ever scanned as a set (the re-check sweep);
-- everything else is a primary-key lookup.
CREATE INDEX IF NOT EXISTS idx_shared_accounts_unresolved
    ON shared_accounts (checked_at) WHERE signer_address IS NULL;

-- Carry over what provisioning already learned. These are positives written by
-- ProvisionService when this service created the account, so they are exactly
-- the permanent kind — no re-check needed for any of them.
INSERT INTO shared_accounts (wallet, signer_address, checked_at)
SELECT wallet, shared_account_signer_address, updated_at
  FROM users
 WHERE shared_account_signer_address IS NOT NULL
   AND shared_account_signer_address <> ''
ON CONFLICT (wallet) DO NOTHING;

-- users.shared_account_signer_address is now superseded. It is left in place
-- for one release rather than dropped in the same change that moves its
-- writer, which is the staging this repo uses everywhere else (plan 06 step 5,
-- plan 07 step 5): drop the writers, ship, then drop the column. It has no
-- readers and is in no API response, so nothing observes the difference.
COMMENT ON COLUMN users.shared_account_signer_address IS
    'SUPERSEDED by shared_accounts.signer_address (2026-08-22). No writers, no readers; droppable next release.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

COMMENT ON COLUMN users.shared_account_signer_address IS NULL;
DROP TABLE IF EXISTS shared_accounts;

-- +goose StatementEnd
