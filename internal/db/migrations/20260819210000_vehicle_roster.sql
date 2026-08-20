-- +goose Up
-- +goose StatementBegin
SELECT 'up SQL query';

-- The minted-vehicle roster: what a vehicle IS and who owns it, held once for
-- the whole platform instead of guessed separately by every service.
--
-- Plan 07 step 3. The reason this table exists is a contradiction, not a
-- tidiness argument: on 2026-08-19 three production vehicles (192379, 192400,
-- 192401 — Maxus T60s) had kaufmann_oracle.vins.owner disagreeing with the
-- chain, and had done since a transfer, with no error, no metric and nothing
-- that would ever have surfaced it. They were found by diffing two databases
-- by hand during an unrelated investigation.
--
-- KEYED BY vehicle_token_id ALONE, not by (tenant, vehicle). A vehicle's owner
-- and definition are properties of the vehicle, not of anybody's relationship
-- to it — that is precisely the confusion that let two services hold two
-- answers. Which tenants may SEE a vehicle stays in vehicle_entitlements,
-- where it belongs.
--
-- This is a CACHE OF THE CHAIN'S ANSWER, reconciled, not a record we author.
-- See ../operator-tenancy/06-onchain-surface.md:125 — the chain records what a
-- vehicle is and who owns it; web2 records who may look at it. Nothing here is
-- written by a transfer path, an onboarding step, or a user action.
CREATE TABLE IF NOT EXISTS vehicles (
    vehicle_token_id BIGINT PRIMARY KEY,

    -- Owner is the whole point of the table, and the one column with a history
    -- of being wrong.
    --
    -- IT IS RE-READ FROM identity-api ON EVERY RECONCILE RUN, never written by
    -- whoever performed a transfer. That is the difference between this and
    -- vins.owner: kaufmann's copy is written once by its own transfer workers,
    -- so a transfer kaufmann did not perform — or one whose post-transfer
    -- update failed — is permanent divergence. A column that is re-read cannot
    -- drift for longer than the reconcile interval, whatever happened on chain
    -- and whoever did it.
    owner VARCHAR(43),

    -- Definition, mirrored from identity-api's vehicle node.
    definition_id TEXT,
    make          TEXT,
    model         TEXT,
    year          SMALLINT,

    minted_at TIMESTAMPTZ,

    -- VIN and plate are the two fields another service is allowed to be the
    -- source for: kaufmann writes plates from the Chilean registry
    -- (sync_apimaz.go:103), so for license_plate this table is a consumer and
    -- must not overwrite a known plate with an empty read. See the reconcile
    -- service, which only ever fills these forward.
    vin           TEXT,
    license_plate TEXT,

    -- Provenance of the last successful read, so a stale roster is visible as
    -- a timestamp rather than inferred from absence. reconciled_at moves on
    -- every run that saw the vehicle; first_seen_at never changes.
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reconciled_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The last run in which identity-api did NOT return this vehicle for any
    -- license we hold. Set rather than deleted: a vehicle vanishing from every
    -- privileged set usually means an SACD was revoked or a tenant's licence
    -- rotated, not that the vehicle stopped existing, and deleting the row
    -- would discard the only record that we ever knew it. Nothing reads it as
    -- a gate yet; it exists so the first person who needs to ask "when did we
    -- lose sight of this?" has an answer.
    unseen_since TIMESTAMPTZ,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Finding every vehicle a wallet owns is the roster's second question after
-- "who owns this one", and it is how an owner-change sweep is checked.
CREATE INDEX IF NOT EXISTS idx_vehicles_owner ON vehicles (lower(owner));

-- Serves the staleness query the reconcile job and any future alert both want:
-- which rows has nothing refreshed lately.
CREATE INDEX IF NOT EXISTS idx_vehicles_reconciled_at ON vehicles (reconciled_at);

-- Owner changes are recorded rather than only applied.
--
-- Without this the reconcile job is silent by construction: it would correct
-- kaufmann's three wrong owners and leave nothing to show it had happened, so
-- the next unexplained transfer would be as invisible as this one was. A row
-- here is also the evidence for the diagnostic the plan asks a human to read
-- once — and, later, the natural trigger for "a vehicle you can see changed
-- hands".
CREATE TABLE IF NOT EXISTS vehicle_owner_changes (
    id               BIGSERIAL PRIMARY KEY,
    vehicle_token_id BIGINT NOT NULL REFERENCES vehicles (vehicle_token_id) ON DELETE CASCADE,
    -- NULL previous_owner is the first observation, not a transfer from nobody.
    previous_owner   VARCHAR(43),
    new_owner        VARCHAR(43) NOT NULL,
    observed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_vehicle_owner_changes_token
    ON vehicle_owner_changes (vehicle_token_id, observed_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 'down SQL query';

DROP TABLE IF EXISTS vehicle_owner_changes;
DROP TABLE IF EXISTS vehicles;

-- +goose StatementEnd
