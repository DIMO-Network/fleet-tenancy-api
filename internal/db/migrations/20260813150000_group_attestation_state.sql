-- +goose Up
-- +goose StatementBegin

-- P4 of the groups move: this service becomes the single publisher of
-- dimo.document.vehicle.groups attestations, and this table is the publisher's
-- memory — what it last said about each vehicle, so a scan can publish exactly
-- the vehicles whose current group set no longer matches.
--
-- The digest covers the whole published payload (group ids, names, colours),
-- not just membership: a rename must republish every member vehicle, which is
-- the fan-out both source apps performed on the write path. Publishing is
-- deliberately scan-based rather than enqueued per write — the River
-- unique-states incident (kaufmann-oracle#192) was an enqueue-based publisher
-- silently coalescing a rename into completed jobs; a scan against current
-- state has no jobs to coalesce and converges on whatever is true now.
--
-- A vehicle whose last group is removed keeps its row with the digest of the
-- empty set: the retraction (empty groups) is published exactly once, and the
-- row records that it was.

CREATE TABLE IF NOT EXISTS vehicle_group_attestation_state (
    tenant_id        UUID   NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    vehicle_token_id BIGINT NOT NULL,
    published_digest TEXT   NOT NULL,
    published_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, vehicle_token_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS vehicle_group_attestation_state;
-- +goose StatementEnd
