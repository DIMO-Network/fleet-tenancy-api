-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL query';

-- A membership is the commercial record for one vehicle: a term the customer
-- has paid for, movable to another vehicle when the first is discontinued.
--
-- DELIBERATELY NOT COLUMNS ON vehicle_entitlements. The entitlement answers
-- "may this customer see this vehicle"; the membership answers "is it paid for,
-- and until when". Folding them together would make moving a membership a
-- revoke-and-regrant, which discards the entitlement's provenance
-- (source_group_id, granted_by_wallet, created_at) as a side effect of a purely
-- commercial action — and would leave a future purchase flow nothing to hang
-- off. Keeping them apart is also what lets an entitlement be revoked without
-- destroying paid time, which is exactly the discontinued-vehicle case.
CREATE TABLE IF NOT EXISTS vehicle_memberships (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    vehicle_token_id  BIGINT NOT NULL,
    term_months       SMALLINT NOT NULL CHECK (term_months IN (1, 12, 24, 36, 48)),
    starts_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Stored rather than derived from starts_at + term_months. Renewal extends
    -- the expiry without rewriting the start, so a derived column would
    -- disagree with the row the first time anyone renewed.
    expires_at        TIMESTAMPTZ NOT NULL,
    canceled_at       TIMESTAMPTZ,
    created_by_wallet VARCHAR(43),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one LIVE membership per vehicle per tenant.
--
-- "Live" here is canceled_at IS NULL, NOT "unexpired". NOW() is not immutable,
-- so an expiry test cannot appear in an index predicate at all — an expired
-- membership therefore still occupies its vehicle's slot until something
-- cancels or supersedes it, which is what the service does.
--
-- The service is what refuses to create over an unexpired membership, with a
-- message naming the situation. This index is the backstop for the
-- read-then-write race, exactly as idx_vehicle_entitlements_one_active_holder
-- is for assignment: two concurrent creates can both pass the read, and the
-- failure mode of that race is a customer paying twice for one vehicle.
CREATE UNIQUE INDEX IF NOT EXISTS idx_vehicle_memberships_one_live
    ON vehicle_memberships (tenant_id, vehicle_token_id) WHERE canceled_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_vehicle_memberships_tenant_live
    ON vehicle_memberships (tenant_id) WHERE canceled_at IS NULL;

-- Where a membership has been. After a move the membership row itself no longer
-- records the vehicle it came from, and "which vehicle was this against in
-- March" is a question support will be asked.
CREATE TABLE IF NOT EXISTS vehicle_membership_moves (
    membership_id   UUID NOT NULL REFERENCES vehicle_memberships (id) ON DELETE CASCADE,
    from_token_id   BIGINT NOT NULL,
    to_token_id     BIGINT NOT NULL,
    moved_by_wallet VARCHAR(43),
    moved_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_vehicle_membership_moves_membership
    ON vehicle_membership_moves (membership_id, moved_at DESC);

-- Whether fleet-lite hides this tenant's vehicles that have no active
-- membership.
--
-- Off by default, deliberately and load-bearingly. Turning it on removes
-- vehicles from what the customer's app returns at all, so a default of true
-- would blank the fleet of every existing customer the moment this migration
-- ran. Enforcement is turned on per customer, from the console, once their
-- memberships have been assigned and checked.
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS memberships_enforced BOOLEAN NOT NULL DEFAULT false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL query';

ALTER TABLE tenants DROP COLUMN IF EXISTS memberships_enforced;
DROP TABLE IF EXISTS vehicle_membership_moves;
DROP TABLE IF EXISTS vehicle_memberships;

-- +goose StatementEnd
