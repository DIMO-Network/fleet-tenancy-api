-- +goose Up
-- +goose StatementBegin

-- Fleet groups move here from fleet-lite-app and kaufmann-oracle — the record,
-- not the fleet data. See docs/plans/01-groups-into-tenancy.md. This is P1:
-- schema and endpoints, no caller yet, no backfill yet.
--
-- Shapes mirror fleet-lite's tables (the closer of the two sources) with the
-- differences that matter here:
--
--   * membership keys on vehicle_token_id, never imei — matching
--     vehicle_entitlements, and the reason kaufmann's P2 re-key happens before
--     any data moves;
--   * no FK to a vehicles table, because this service deliberately has none —
--     entitlements are the only vehicle-shaped rows and exist only for
--     explicit-mode tenants;
--   * the membership FK carries tenant_id so a row cannot point at another
--     tenant's group — cross-tenant leakage is the class of bug this service
--     exists to prevent, so the schema enforces it rather than trusting the
--     service layer.
--
-- Ids are <tenant-uuid>_<slug>, the R1 convention both sources already use, so
-- P3's backfill is a straight copy with no id migration. UNIQUE (tenant_id,
-- name) is exact-case to match what the sources enforce — a stricter
-- lower(name) unique could refuse rows the sources legitimately hold.
--
-- scope_group_ids / source_group_id deliberately gain NO foreign keys yet:
-- those columns reference groups that still live in the source databases until
-- P3's backfill runs. The plan adds the FKs last, once the rows exist.

CREATE TABLE IF NOT EXISTS fleet_groups (
    id         TEXT PRIMARY KEY,       -- <tenant-uuid>_<slug>
    tenant_id  UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    color      VARCHAR(7) NOT NULL,    -- HTML hex color like #FF5733
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name),
    UNIQUE (id, tenant_id)             -- composite target for the membership FK
);

CREATE INDEX IF NOT EXISTS idx_fleet_groups_tenant ON fleet_groups (tenant_id);

CREATE TABLE IF NOT EXISTS vehicle_fleet_groups (
    tenant_id        UUID   NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    vehicle_token_id BIGINT NOT NULL,
    fleet_group_id   TEXT   NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, vehicle_token_id, fleet_group_id),
    FOREIGN KEY (fleet_group_id, tenant_id)
        REFERENCES fleet_groups (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_vehicle_fleet_groups_group
    ON vehicle_fleet_groups (fleet_group_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS vehicle_fleet_groups;
DROP TABLE IF EXISTS fleet_groups;
-- +goose StatementEnd
