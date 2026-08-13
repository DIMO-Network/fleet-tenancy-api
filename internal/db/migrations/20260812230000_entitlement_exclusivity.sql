-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL query';

-- A vehicle may be entitled to at most one tenant at a time.
--
-- The design specifies this invariant per operator — "at most one active
-- entitlement among the explicit-mode tenants of a given operator" — and notes
-- that a partial unique index cannot express it, because deciding whether two
-- rows conflict needs each tenant's parent. That is true, and the service layer
-- does enforce exactly that rule, with the holder's name in the error so the
-- console can say who has the vehicle.
--
-- This index is deliberately STRICTER: one active entitlement per vehicle,
-- full stop, regardless of operator. It is a backstop, not the rule.
--
-- Why have both. The service check reads and then writes, so two concurrent
-- assignments of the same vehicle to two different customers can both pass it
-- and both insert. The failure mode of that race is one customer seeing another
-- customer's vehicle — the exact cross-tenant leak that D2/D5 make our code's
-- responsibility rather than the chain's. A constraint violation is a far better
-- outcome than a silent double grant, so the database gets the final say.
--
-- Why stricter is acceptable: Q10 settled that a vehicle is never shared or
-- sub-leased, so per-operator and global uniqueness coincide in practice. If
-- that ever changes, this index is the thing to relax, and the service rule is
-- already written to the looser specification.
--
-- Only rows that are still in force participate: revoked_at IS NOT NULL rows
-- are history, and a vehicle reassigned after a revocation must not collide
-- with the record of where it used to be.
CREATE UNIQUE INDEX IF NOT EXISTS idx_vehicle_entitlements_one_active_holder
    ON vehicle_entitlements (vehicle_token_id) WHERE revoked_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL query';

DROP INDEX IF EXISTS idx_vehicle_entitlements_one_active_holder;

-- +goose StatementEnd
