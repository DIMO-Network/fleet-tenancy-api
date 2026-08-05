-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL query';

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- A tenant is an organization in one of two kinds:
--   operator  — holds the DIMO developer license all data access runs under,
--               plus the signer wallet used for on-behalf-of operations.
--   customer  — parent_tenant_id points at its operator. Holds no credentials
--               of its own by default; resolves to the parent's.
-- A customer with parent_tenant_id NULL is a legacy self-serve tenant with its
-- own credentials — how every existing fleet-lite tenant migrates in.
CREATE TABLE IF NOT EXISTS tenants (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL,                     -- operator | customer
    parent_tenant_id UUID REFERENCES tenants (id) ON DELETE RESTRICT,
    status           TEXT NOT NULL DEFAULT 'active',    -- active | suspended
    managed          BOOLEAN NOT NULL DEFAULT FALSE,    -- operator-managed
    -- implicit = everything the effective dev license is privileged on
    --            (operator and self-serve tenants)
    -- explicit = the vehicle_entitlements rows written by the operator
    entitlement_mode TEXT NOT NULL DEFAULT 'explicit',
    -- Does this tenant appear as a selectable fleet in fleet-lite-app? Operators
    -- with thousands of vehicles turn this off; fleet-lite targets sub-500.
    fleet_lite_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    external_ref     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT tenants_kind_check CHECK (kind IN ('operator', 'customer')),
    CONSTRAINT operator_has_no_parent CHECK (kind <> 'operator' OR parent_tenant_id IS NULL)
);

CREATE INDEX IF NOT EXISTS idx_tenants_parent ON tenants (parent_tenant_id);
-- Names are unique per operator, not globally: two operators may each have a
-- customer called "Acme". Unparented tenants are exempt.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_name_per_parent
    ON tenants (parent_tenant_id, lower(name)) WHERE parent_tenant_id IS NOT NULL;

-- Secrets are AES-256-GCM encrypted with a key derived from TENANT_SECRET_ENC_KEY.
-- Plaintext never leaves this service: callers ask for a minted DIMO developer
-- JWT rather than the key itself.
CREATE TABLE IF NOT EXISTS tenant_credentials (
    tenant_id        UUID PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    dimo_client_id   TEXT,
    dimo_api_key_enc TEXT,
    signer_address   VARCHAR(43),
    signer_key_enc   TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Makes developer-license -> tenant resolution unambiguous.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tenant_credentials_client_id
    ON tenant_credentials (lower(dimo_client_id)) WHERE dimo_client_id IS NOT NULL;

-- Users are identified by wallet, stored EIP-55 checksummed.
CREATE TABLE IF NOT EXISTS users (
    wallet        VARCHAR(43) PRIMARY KEY,
    email         TEXT,
    first_name    TEXT,
    last_name     TEXT,
    phone         TEXT,
    business_name TEXT,
    shared_account_signer_address VARCHAR(43),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_users_email ON users (lower(email));

-- permissions is authoritative for authorization; role is a display label and a
-- preset used when adding a member. Two authoritative sources for one decision
-- is how authorization bugs happen.
--
-- scope_group_ids NULL = unrestricted. It is the single mechanism for group
-- scope; there is deliberately no view_all_fleets capability, which would encode
-- the same fact twice with no defined resolution when the two disagree.
CREATE TABLE IF NOT EXISTS memberships (
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    wallet               VARCHAR(43) NOT NULL REFERENCES users (wallet) ON DELETE CASCADE,
    role                 TEXT NOT NULL DEFAULT 'member',   -- owner | admin | member
    permissions          JSONB NOT NULL DEFAULT '[]'::jsonb,
    scope_group_ids      TEXT[],
    granted_by_tenant_id UUID REFERENCES tenants (id),
    granted_by_wallet    VARCHAR(43),
    last_login_at        TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, wallet)
);

CREATE INDEX IF NOT EXISTS idx_memberships_wallet ON memberships (wallet);

-- Management only. A delegation never grants a fleet-lite session: operator
-- staff work in b2b-fleet-mgr-app and there is no impersonation.
-- Authorization always checks this row rather than parent_tenant_id directly, so
-- revoking is a single delete.
CREATE TABLE IF NOT EXISTS tenant_delegations (
    operator_tenant_id UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    customer_tenant_id UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    scopes             TEXT[] NOT NULL,   -- manage_members | manage_vehicles | manage_settings
    created_by_wallet  VARCHAR(43),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (operator_tenant_id, customer_tenant_id)
);

-- Rows exist only for explicit-mode tenants. Operator and self-serve tenants
-- resolve their fleet from the license's privileged set and have no rows here.
--
-- source_group_id is provenance for a bulk assign-by-group: it names an
-- OPERATOR-side fleet group used to select vehicles at assign time. It is not a
-- cross-tenant link; the customer's own groups are separate and theirs.
--
-- Invariant enforced in the service layer (needs the parent lookup, so a partial
-- unique index can't express it): a vehicle token has at most one active
-- entitlement among the explicit-mode tenants of a given operator.
CREATE TABLE IF NOT EXISTS vehicle_entitlements (
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    vehicle_token_id     BIGINT NOT NULL,
    source               TEXT NOT NULL,   -- operator | sacd | import
    source_group_id      TEXT,
    granted_by_tenant_id UUID REFERENCES tenants (id),
    granted_by_wallet    VARCHAR(43),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at           TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, vehicle_token_id)
);

CREATE INDEX IF NOT EXISTS idx_vehicle_entitlements_token
    ON vehicle_entitlements (vehicle_token_id);
CREATE INDEX IF NOT EXISTS idx_vehicle_entitlements_active
    ON vehicle_entitlements (tenant_id) WHERE revoked_at IS NULL;

-- Email invitations. Only the SHA-256 hash of the token is stored; the wallet is
-- unknown until the invitee accepts with their own DIMO passkey.
CREATE TABLE IF NOT EXISTS invitations (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    email                TEXT NOT NULL,
    role                 TEXT NOT NULL DEFAULT 'member',
    permissions          JSONB NOT NULL DEFAULT '[]'::jsonb,
    scope_group_ids      TEXT[],
    token_hash           TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'pending',  -- pending | accepted | revoked
    invited_by_wallet    VARCHAR(43) NOT NULL,
    created_by_tenant_id UUID REFERENCES tenants (id),
    invitee_wallet       VARCHAR(43),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ NOT NULL,
    accepted_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_invitations_token_hash ON invitations (token_hash);
CREATE INDEX IF NOT EXISTS idx_invitations_tenant_id ON invitations (tenant_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL query';

DROP TABLE IF EXISTS invitations;
DROP TABLE IF EXISTS vehicle_entitlements;
DROP TABLE IF EXISTS tenant_delegations;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenant_credentials;
DROP TABLE IF EXISTS tenants;

-- +goose StatementEnd
