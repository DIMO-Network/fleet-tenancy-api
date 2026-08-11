-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL query';

-- Marks a credential as a service caller: allowed to ask /v1 about any tenant
-- rather than only those whose effective credential is its own.
--
-- WHY THIS EXISTS. /v1 authenticates the caller but, until now, put no bound on
-- which tenant it could ask about. That is not a theoretical gap: of the eight
-- credentials that can authenticate today, four belong to *customer* tenants —
-- Fresh Coast Garage, My Test Fleet, TEST and 0x0065fa40… — and those developer
-- licenses are held by outside companies. Any of them could ask for any tenant's
-- authorization data, including Kaufmann's 149 memberships. Having no ingress
-- made that unreachable, not unauthorized.
--
-- WHY NOT SIMPLY "caller must equal subject". Because the architecture's own
-- resolution rule forbids it: a tenant's effective credential is its own if it
-- has one, otherwise its parent's, so an operator-managed customer holds no
-- license at all and is reached with the operator's. Strict equality would work
-- today — every tenant is currently unparented — and would break on the first
-- operator-managed customer, which is the thing this programme exists to create.
--
-- So the scope rule mirrors credential resolution, and this column carves out
-- the one case that rule cannot express: a shared proxy that legitimately acts
-- across tenants. b2b-fleet-mgr-app is the intended holder. Note it cannot
-- authenticate at all yet — its CLIENT_ID is the Login-with-DIMO app id, shared
-- with fleet-lite, and is not a registered tenant credential.
--
-- Default FALSE, deliberately: a credential is scoped unless someone decides
-- otherwise, and that decision is visible as a row change.
ALTER TABLE tenant_credentials
    ADD COLUMN IF NOT EXISTS is_service_caller BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN tenant_credentials.is_service_caller IS
    'Credential may query /v1 for any tenant, not only those resolving to it. Grant sparingly.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL query';

ALTER TABLE tenant_credentials DROP COLUMN IF EXISTS is_service_caller;

-- +goose StatementEnd
