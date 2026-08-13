# Fleet groups move into fleet-tenancy-api

Status: **agreed, not started**. Written 2026-08-12.

## The problem

`fleet-lite-app` and `kaufmann-oracle` each own a `fleet_groups` table, and the
two are near character-for-character identical:

```sql
fleet_groups (id TEXT PRIMARY KEY, name, color VARCHAR(7), tenant_id UUID, ...)
```

They differ only in membership: fleet-lite's `vehicle_fleet_groups` keys on
`(tenant_id, token_id)`, kaufmann's `vin_fleet_groups` keys on `imei`.

Both then synchronise the same `dimo.document.vehicle.groups` CloudEvent
independently — roughly 1,500 lines in fleet-lite (`import_group_attestations`,
`republish_group_attestations`, `group_sync.go`, `attest_service.go`) against
~330 in kaufmann (`vehicle_groups_attest.go`, `groupattest/worker.go`).

**Six of the traps recorded in [`../HANDOFF.md`](../HANDOFF.md) are the same
bug wearing different clothes** — two systems converging on one group through an
eventually-consistent stream:

1. Group metadata rides on every member vehicle's attestation, so two vehicles
   in the same group can disagree about its name, and no copy is authoritative.
2. A first attempt at fixing stale names rewrote one production group's name 40
   times in a single import, the winner decided by processing order.
3. A rename published nothing at all — River deduplicated the republish against
   already-completed jobs, so it never reached the wire.
4. "Re-running a sync cannot fix a stale attestation." Memberships were correct;
   only the name was wrong; no number of re-runs would have changed it.
5. `DROP_FOREIGN_TENANT_GROUPS` would have deleted 370 of 378 group memberships,
   because the entire production group structure was one producer's assertions.
6. The republish exercise, and the meshing bug that made its first attempt
   publish 0 of 287.

None of these arise with one owner.

There is also a live inconsistency in this service: **`memberships.scope_group_ids`,
`vehicle_entitlements.source_group_id` and `invitations.scope_group_ids` are all
bare `TEXT` with no foreign key**, pointing into databases this service cannot
see. It stores group ids three times over and can validate none of them.

## The destination

Groups — the record, not the fleet data — live in `fleet-tenancy-api`:

```sql
fleet_groups        (id, tenant_id, name, color, created_at, updated_at)
vehicle_fleet_groups (tenant_id, vehicle_token_id, fleet_group_id)
```

Keyed by **`vehicle_token_id` throughout**, matching `vehicle_entitlements` and
making the three group-id columns above into real references.

Both apps read groups from here. Membership and metadata have one writer.

### Attestations stop being a synchronisation mechanism

This is the part worth being explicit about, because it is where most of the
complexity goes away rather than moves.

Attestations exist in their current form because two independent systems needed
to converge on the same groups. With one source of truth there is nothing to
converge: publishing becomes an **outward** record for third parties and for
durability, and the import and reconcile halves are deleted rather than
relocated. One publisher, no reconcile, no producer disagreement, no republish
gate, no `DROP_FOREIGN_TENANT_GROUPS`.

Publishing itself stays. `kaufmann-oracle/docs/adr/0001-fleet-group-attestations.md`
makes it a platform-facing commitment, and third parties may consume it.

### What does NOT move

**VIN, licence plate, make, model, IMEI, device state.** Fleet data belongs to
the oracle. The distinction that matters: a *group* is an organising structure
over tenants and vehicles, which is tenancy's domain; a *VIN* is a fact about a
vehicle, which is the oracle's. Copying fleet data here would make this service
a second, staler source of it — the same mistake in the other direction.

## The imei keying is a defect, and fixing it is not the hard part

kaufmann storing group membership against `imei` was raised as a possible
blocker on the grounds that it would allow grouping a vehicle before it is
minted, when no token id exists. **It does not.** `AddVinToFleetGroup` already
takes a token id in the route and resolves it through
`resolveMintedVinByTokenID`, whose own comment reads:

> Resolve the vehicle by token id (minted vehicles only) and use its imei internally.

So the external contract is already token-id-based and already minted-only. The
imei is an internal storage artifact, there are no pre-mint rows to migrate, and
the re-key is mechanical:

```sql
vin_fleet_groups.imei -> vins.vehicle_token_id
```

Anything whose `vehicle_token_id` is NULL has no valid row today and should be
dropped with a count logged, not carried across.

## Phases

Each is independently shippable and revertible.

### P1 — Schema and read path in tenancy

Create the two tables and the endpoints: list a tenant's groups, create, rename,
recolour, delete, add/remove a vehicle. Add the foreign keys from
`scope_group_ids` / `source_group_id` last, once the rows exist.

Nothing reads them yet. **Exit:** endpoints served, no caller.

### P2 — kaufmann's re-key, in place

Change `vin_fleet_groups` to key on `vehicle_token_id` **before** anything
moves. Two changes at once — re-key and relocate — makes a bad migration
impossible to bisect.

**Exit:** kaufmann's groups behave identically, keyed correctly.

### P3 — Backfill, and read from tenancy behind a flag

Copy both apps' groups and memberships into tenancy. Ids are already
`<tenant-uuid>_<slug>` after R1 and tenant uuids are already unified, so
**there is no id migration** — the expensive half is behind us.

Then each app reads groups from tenancy behind a flag, writing locally as well,
and a diff command compares the two the way `tenancy-diff` does for
memberships. **Exit:** zero differences over a sustained window.

### P4 — Cut writes over, delete the import half

Group writes go to tenancy. fleet-lite's `import_group_attestations`,
`group_sync.go` reconcile and `republish_group_attestations` are deleted, along
with `DROP_FOREIGN_TENANT_GROUPS`. Publishing moves to a single publisher.

**Exit:** one writer, one publisher, no importer.

### P5 — Retire the local tables

Written as one line originally. Surveying both repos after P4 shipped showed
it is two phases with a blocker in between, so it is split.

#### P5a — Move the last readers off the local tables

P4 left the local tables load-bearing for one thing: **scope filtering**.
Every "which vehicles may this limited member see" question is still a SQL
join against them, on the hottest paths in both apps.

These become cached, tenancy-backed token-id sets.
`GET /v1/tenants/{id}/vehicle-groups` already returns every group with its
full member set, so one cached call per tenant answers all of it — scope,
counts, and the group names decorating vehicle rows.

| App | What still reads the local tables |
|---|---|
| fleet-lite | `AccessibleTokenIDs`, `allowedGroupsFilter` (a subquery inside the vehicles query), `VehicleInGroups`, `LoadVehicleGroups`, `GroupMemberTokenIDs`, geofence `EffectiveTokenIDs` + its per-geofence count, `normalizeGroups`, `validateGroupIDs` |
| kaufmann | `GetVehiclesOnboarded`'s group join and accessible-group subquery plus its eager load, `mapVehicle`, the console's vehicle decoration, `vinGroupFilterMod` and the group names in the distance and export reports |

Three hazards, all recorded because each has a precedent in this programme:

1. **An empty token-id set must match zero vehicles, never "no filter".**
   `if len(ids) > 0 { apply }` hands a member scoped to empty or
   vehicle-less groups the entire fleet. That is the inversion that gave 131
   memberships a 524-vehicle fleet during the backfill, wearing a new
   costume.
2. **kaufmann's scope source is itself still wrong.** It reads
   `access_fleet_groups` by wallet with **no tenant column**, so scope spans
   every tenant a member belongs to. The authz answer already in request
   locals carries the correct per-tenant `scopeGroupIds` for free, cached
   60s, and `view_all_fleets` — which the shared model deleted — collapses
   into `Unrestricted()`. `tenancy-diff` compares exactly these two and
   currently reports `differ=0`, so the switch is a provable no-op per
   wallet. That is the gate for making it.
3. **The two nil conventions are inverted.** `GetUserFleetAccess` returns
   nil for "no groups"; `AuthzResult.ScopeGroupIDs` nil means
   "unrestricted". A normalisation compensating for the old convention
   already exists in the report worker. It must be re-examined, not copied.

Also: `GetVehiclesOnboarded` applies its group predicate to the paging
`Count` as well as the page, inside one transaction. An in-memory filter has
to keep `totalCount` consistent or paging silently breaks.

**Exit:** nothing but the mirror tooling reads the tables, and both apps'
`groups-diff` still clean.

#### P5b — Drop them

**Blocker found in the survey:** kaufmann's `access_fleet_groups` has an
`ON DELETE CASCADE` foreign key to `fleet_groups`, so that table cannot be
dropped until `access_fleet_groups` itself is retired — which P5a starts by
removing its readers from the scope path, but `account.go` still reads and
writes it for the member-management surface.

Then, and only after the soak: drop `fleet_groups` / `vehicle_fleet_groups`
/ `vin_fleet_groups`, the `mirror-groups` crons and commands, `groups-diff`,
the `GROUPS_FROM_TENANCY` flags and their local branches, fleet-lite's dead
`vehicles.groups_updated_at` and `last_group_sync_at` columns,
`SyncVehicleGroups` with its frontend caller, and the stale `GROUP_SYNC.md`
/ `FLEET_GROUPS_PLAN.md`. Add the deferred references from
`scope_group_ids` / `source_group_id` — both are **arrays**, so that is a
trigger or a check constraint, not a plain foreign key.

**The soak is the point, not a formality.** "A full retention period with no
fallback reads" means: `GROUPS_FROM_TENANCY` on in both apps, `groups-diff`
clean across several days, the publisher converging, and no revert. Reads
flipped 2026-08-13; dropping the same day would discard the only evidence
the move worked, and the tables are the revert path until then.

## Risks

**Availability coupling (R5) gets worse if the publisher shares the hot path.**
`/v1/authz` is called on every request in two apps, and per-vehicle attestation
publishing is heavy background work. It should share the schema but not the
fate of the API: a separate deployable against the same database, not another
goroutine in the API process.

**The migration touches the only structure that has already caused data loss.**
P3's diff is not optional. The 2026-08-06 near-miss — 370 of 378 memberships —
was caught by comparing before enforcing, and this plan should assume the same
discipline.

**`access_fleet_groups` in kaufmann is keyed by wallet with no tenant column**,
so a member's group scope currently spans every tenant they belong to. That is a
separate defect; the shared model fixes it by putting scope on the membership,
and it should be fixed as part of retiring that table rather than carried over.

## Considered and rejected

**Groups in tenancy, membership left per-app.** Cheaper, and it would fix all
six traps, which are about metadata rather than membership. Rejected because it
leaves the membership tables duplicated and the group-id columns still
unvalidated, so the second half would never get done — and because the re-key
turned out to be mechanical, which was the only real argument for stopping half
way.

**Teaching tenancy about IMEIs and VINs so membership can span identities.**
Rejected: it is the oracle-data leak this whole separation exists to prevent,
and the premise it was solving for — pre-mint grouping — does not exist.

**Leaving groups where they are and deduplicating only the attestation
machinery.** Rejected: the duplication is the *data*, and a shared sync layer
over two divergent stores is more moving parts, not fewer.
