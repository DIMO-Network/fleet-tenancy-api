-- +goose Up
-- +goose StatementBegin

-- P5b of the groups move: the deferred references land.
--
-- scope_group_ids / source_group_id were left without integrity while groups
-- still lived in three databases — a reference here could name a row that only
-- existed somewhere else. P3's backfill ended that, and P5b drops the other two
-- copies, so from now on a group id that this database does not hold is a bug,
-- not a timing artifact.
--
-- Three shapes, three mechanisms:
--
--   * vehicle_entitlements.source_group_id is a SCALAR and gets a real foreign
--     key. It is provenance — it names the OPERATOR-side group used to select
--     vehicles at assign time — so it is deliberately NOT paired with the row's
--     tenant_id, which is the customer's. ON DELETE SET NULL: deleting a group
--     must not delete or block anything downstream of a grant it once seeded;
--     the provenance simply ends.
--
--   * memberships.scope_group_ids / invitations.scope_group_ids are ARRAYS, so
--     no FK can express them. A trigger validates every element on write, and
--     the group's tenant must equal the row's tenant — a scope naming another
--     tenant's group is exactly the class of bug this exists to stop.
--
--   * Deleting a group strips its id from every scope array in its tenant,
--     mirroring the ON DELETE CASCADE the per-app membership tables had. The
--     result may be an EMPTY array, and that is preserved, never collapsed to
--     NULL: NULL means unrestricted, so collapsing would ESCALATE a member from
--     "saw one group, now gone" to "sees everything".
--
-- Existing danglers are cleaned first, in the same directions the rules then
-- enforce. Dangling scope ids already resolve to nothing today (the group
-- index has no such group), so stripping them changes no one's effective
-- access; dangling provenance already points at nothing, so nulling it loses
-- nothing that was still true.

UPDATE memberships m
   SET scope_group_ids = (
       SELECT COALESCE(array_agg(g ORDER BY ord), '{}')
         FROM unnest(m.scope_group_ids) WITH ORDINALITY AS u(g, ord)
        WHERE EXISTS (SELECT 1 FROM fleet_groups fg
                       WHERE fg.id = u.g AND fg.tenant_id = m.tenant_id))
 WHERE m.scope_group_ids IS NOT NULL
   AND EXISTS (SELECT 1 FROM unnest(m.scope_group_ids) AS u(g)
                WHERE NOT EXISTS (SELECT 1 FROM fleet_groups fg
                                   WHERE fg.id = u.g AND fg.tenant_id = m.tenant_id));

UPDATE invitations i
   SET scope_group_ids = (
       SELECT COALESCE(array_agg(g ORDER BY ord), '{}')
         FROM unnest(i.scope_group_ids) WITH ORDINALITY AS u(g, ord)
        WHERE EXISTS (SELECT 1 FROM fleet_groups fg
                       WHERE fg.id = u.g AND fg.tenant_id = i.tenant_id))
 WHERE i.scope_group_ids IS NOT NULL
   AND EXISTS (SELECT 1 FROM unnest(i.scope_group_ids) AS u(g)
                WHERE NOT EXISTS (SELECT 1 FROM fleet_groups fg
                                   WHERE fg.id = u.g AND fg.tenant_id = i.tenant_id));

UPDATE vehicle_entitlements
   SET source_group_id = NULL
 WHERE source_group_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM fleet_groups fg
                    WHERE fg.id = vehicle_entitlements.source_group_id);

ALTER TABLE vehicle_entitlements
    ADD CONSTRAINT fk_vehicle_entitlements_source_group
    FOREIGN KEY (source_group_id) REFERENCES fleet_groups (id) ON DELETE SET NULL;

CREATE OR REPLACE FUNCTION check_scope_group_ids() RETURNS trigger AS $$
DECLARE
    bad TEXT;
BEGIN
    IF NEW.scope_group_ids IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT u.g INTO bad
      FROM unnest(NEW.scope_group_ids) AS u(g)
     WHERE NOT EXISTS (SELECT 1 FROM fleet_groups fg
                        WHERE fg.id = u.g AND fg.tenant_id = NEW.tenant_id)
     LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'scope_group_ids references % which is not a fleet group of tenant %',
            bad, NEW.tenant_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER memberships_scope_groups_exist
    BEFORE INSERT OR UPDATE OF scope_group_ids ON memberships
    FOR EACH ROW EXECUTE FUNCTION check_scope_group_ids();

CREATE TRIGGER invitations_scope_groups_exist
    BEFORE INSERT OR UPDATE OF scope_group_ids ON invitations
    FOR EACH ROW EXECUTE FUNCTION check_scope_group_ids();

CREATE OR REPLACE FUNCTION strip_deleted_group_from_scopes() RETURNS trigger AS $$
BEGIN
    UPDATE memberships
       SET scope_group_ids = array_remove(scope_group_ids, OLD.id)
     WHERE tenant_id = OLD.tenant_id AND OLD.id = ANY (scope_group_ids);
    UPDATE invitations
       SET scope_group_ids = array_remove(scope_group_ids, OLD.id)
     WHERE tenant_id = OLD.tenant_id AND OLD.id = ANY (scope_group_ids);
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER fleet_groups_strip_scopes
    AFTER DELETE ON fleet_groups
    FOR EACH ROW EXECUTE FUNCTION strip_deleted_group_from_scopes();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Removes the rules, not the cleanups: danglers stripped by the Up were
-- already dead references, and recreating garbage is not a rollback.

DROP TRIGGER IF EXISTS fleet_groups_strip_scopes ON fleet_groups;
DROP FUNCTION IF EXISTS strip_deleted_group_from_scopes();
DROP TRIGGER IF EXISTS invitations_scope_groups_exist ON invitations;
DROP TRIGGER IF EXISTS memberships_scope_groups_exist ON memberships;
DROP FUNCTION IF EXISTS check_scope_group_ids();
ALTER TABLE vehicle_entitlements
    DROP CONSTRAINT IF EXISTS fk_vehicle_entitlements_source_group;

-- +goose StatementEnd
