-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL query';

-- Grants manage_vehicles to every membership that was at admin level.
--
-- WHY THIS EXISTS. kaufmann-oracle's shared-account routes — POST
-- /v1/vehicle/{transfer,disconnect,delete}/shared — asked only whether the
-- caller was a member of the tenant with at least one capability. The handlers
-- verify that the *owning* account granted the tenant's signer, which says the
-- tenant may act; it has never said which of its members may. So any member
-- holding a single unrelated capability, reports included, could move or burn a
-- vehicle belonging to a customer. manage_vehicles is the missing per-operation
-- gate, and this backfill decides who starts with it.
--
-- WHY role RATHER THAN A CAPABILITY. The rule is "whoever was at admin level",
-- and role is the faithful record of that: the kaufmann backfill derived it
-- straight from access_tenants.is_admin. Selecting on capabilities instead would
-- miss the admins who carry only reports — of the 44 rows this touches, several
-- are exactly that — and would quietly demote real administrators.
--
-- This is the one place role may be read. It is a display label and a preset,
-- never an authorization input, and nothing here reads it at request time; a
-- one-off migration choosing a starting set is not an authorization decision.
--
-- WHO LOSES ACCESS. Non-admin members keep whatever they had except these three
-- operations, which is the point of the change. At the time of writing exactly
-- one membership is affected — a member of the Kaufmann tenant holding
-- onboard_vehicles and reports. If that turns out to be deliberate, grant them
-- manage_vehicles explicitly rather than widening this rule; an intentional
-- exception should be visible as a row, not as a broader predicate.
--
-- Idempotent: the NOT @> guard means a re-run is a no-op, and it composes with
-- the kaufmann backfill, which merges rather than replaces.
UPDATE memberships
SET permissions = permissions || '["manage_vehicles"]'::jsonb,
    updated_at  = NOW()
WHERE role IN ('owner', 'admin')
  AND NOT permissions @> '["manage_vehicles"]'::jsonb;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL query';

-- Removes the capability wherever it is held. This is wider than the Up: it also
-- drops grants made by hand after the migration ran. That is the honest inverse
-- for a capability introduced here — leaving hand-grants behind would make a
-- rolled-back schema still carry a gate that no longer exists in the code.
UPDATE memberships
SET permissions = permissions - 'manage_vehicles',
    updated_at  = NOW()
WHERE permissions @> '["manage_vehicles"]'::jsonb;

-- +goose StatementEnd
