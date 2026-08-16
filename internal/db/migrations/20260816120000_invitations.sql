-- +goose Up
-- +goose StatementBegin

-- Email invitations move here from fleet-lite-app — the last membership-adjacent
-- record living outside this service. See docs/plans/04-invitations-into-tenancy.md.
-- This is P1: schema, service and endpoints, no caller yet, no backfill yet.
--
-- Ported from fleet-lite's 20260617120000_invitations.sql plus its
-- email-tracking columns, with the differences the plan settles:
--
--   * scope_group_ids replaces allowed_group_ids, carrying the same tri-state
--     as memberships (NULL = unrestricted, {} = restricted to nothing) so the
--     invite's scope becomes the membership's scope verbatim at accept, with
--     no translation between two encodings;
--   * created_by_tenant_id (nullable) records which tenant issued the invite
--     when it was not the subject tenant itself — the operator console's
--     invites carry the operator here, customer-sent invites carry NULL. It is
--     the audit answer to "who invited this person", per the original spec;
--   * no FK from scope_group_ids to fleet_groups — it is an array, like
--     memberships.scope_group_ids, and gains its constraint in the same
--     deferred sweep (see HANDOFF, P5 notes).
--
-- The token is the credential: only its SHA-256 hash is stored, the plaintext
-- exists in the email link and this service's memory at mint time. The unique
-- index on token_hash is also what makes a resend invalidate the old link —
-- the row's hash is replaced, so the superseded token no longer resolves.
--
-- P2's backfill is id-preserving and copies token_hash intact, so outstanding
-- emailed links keep working across the cutover. Nothing here may assume ids
-- were minted by this service.
CREATE TABLE IF NOT EXISTS invitations (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    email                TEXT NOT NULL,
    role                 TEXT NOT NULL DEFAULT 'member',
    token_hash           TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'pending', -- pending | accepted | revoked
    invited_by_wallet    VARCHAR(43) NOT NULL,
    invitee_wallet       VARCHAR(43),
    created_by_tenant_id UUID REFERENCES tenants (id) ON DELETE SET NULL,
    scope_group_ids      TEXT[],                          -- NULL = unrestricted, {} = none

    -- Email-delivery tracking, stamped on send and upgraded by the Postmark
    -- webhook. Upgrades are monotonic (sent < delivered < opened; bounced
    -- beats all) because Postmark retries and events arrive out of order.
    postmark_message_id  TEXT,
    email_status         TEXT,                            -- sent | delivered | opened | bounced
    email_status_at      TIMESTAMPTZ,
    email_status_detail  TEXT,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ NOT NULL,
    accepted_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_invitations_token_hash ON invitations (token_hash);
CREATE INDEX IF NOT EXISTS idx_invitations_tenant_id ON invitations (tenant_id);
CREATE INDEX IF NOT EXISTS idx_invitations_status ON invitations (status);
-- Webhook fallback lookup when message metadata is missing.
CREATE INDEX IF NOT EXISTS idx_invitations_postmark_message_id ON invitations (postmark_message_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS invitations;
-- +goose StatementEnd
