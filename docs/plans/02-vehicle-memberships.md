# Vehicle memberships — a per-vehicle, term-based product

Status: **agreed, not started**. Written 2026-08-13.

## What this is

Customers purchase a **membership per vehicle**: a term of 1, 12, 24, 36 or 48
months attached to one vehicle, movable to another vehicle (e.g. when a vehicle
is discontinued). There is **no purchase flow yet** — operators create and
manage memberships from the b2b console. In fleet-lite, a customer sees their
memberships under the settings "Advanced" section, and **vehicles without an
active membership are hidden** — the backend simply does not return them.

Decisions taken 2026-08-13, with the reasoning recorded so they are not
re-litigated:

| Decision | Choice | Why |
|---|---|---|
| Data model | **Separate entity**, not attributes on `vehicle_entitlements` | A membership has its own lifecycle (term, expiry, renewal, moving between vehicles) that is orthogonal to "may this customer see this vehicle". Moving a membership must not destroy entitlement provenance, and a future purchase flow needs a thing to attach to. |
| Naming | **"Membership"** in every UI, **"Membresías"** in the Spanish localization | The user-facing word. "You have 10 memberships across 10 different cars." |
| Table / wire naming | `vehicle_memberships`, `/v1/tenants/{id}/vehicle-memberships` | The bare word collides catastrophically: `memberships` is already the authz core of this service (users in tenants), and "license" already means DIMO developer license and `license_plate` across all four repos. The `vehicle_` prefix disambiguates in code; the UI never needs it. |
| Enforcement | **Per-tenant flag, default off** (`memberships_enforced` on `tenants`) | Fleet-lite also serves self-serve tenants (no operator, no console to manage memberships — My Test Fleet holds 40 vehicles). Enforcing everywhere would blank their fleets; enforcing automatically for explicit-mode customers would blank every managed customer the day it deploys. A flag the operator flips per customer, once memberships are assigned, is reversible and incremental — the same shape as `fleet_lite_enabled`. |
| Expiry | **Auto-hide at expiry**, no grace period | Validity is computed (`starts_at` + term); the read path counts only unexpired memberships, so expiry needs no cron and cannot be missed. The console's job is to surface expiring-soon so operators renew first. |

## Where it lives, and why

**The membership record lives in `fleet-tenancy-api`**, next to
`vehicle_entitlements`. The entitlement answers *may this customer see this
vehicle*; the membership answers *is this vehicle paid for, and until when*.
fleet-lite shows the intersection.

**Enforcement lives in fleet-lite's `VehicleService`**, the same choke point
that already applies group scope. kaufmann needs no read-path change at all:
fleet-lite reads its vehicle set from its own `vehicles` table and its access
answers from tenancy, so a filter applied there hides vehicles everywhere they
surface (list, detail, telemetry, documents, TCO, geofences — every call site
funnels through `VehicleService`).

**Administration flows through the existing proxy chain**: b2b (BFF, no
database, no DIMO license) → kaufmann (`/v1/tenancy/*` proxies, which mint the
operator's developer JWT) → this service. Both hops are route-by-route, so each
new endpoint is registered three times. That is the price of the existing
architecture, not new design.

## Schema (this repo)

```sql
CREATE TABLE vehicle_memberships (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    vehicle_token_id   BIGINT NOT NULL,
    term_months        SMALLINT NOT NULL CHECK (term_months IN (1, 12, 24, 36, 48)),
    starts_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ NOT NULL,   -- starts_at + term, computed at write
    canceled_at        TIMESTAMPTZ,            -- soft cancel / supersede
    created_by_wallet  VARCHAR(43),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one live (non-canceled) membership per vehicle per tenant. NOW() is
-- not immutable, so "unexpired" cannot be in the predicate — expiry is enforced
-- in the service layer, this index is the race backstop, same division of
-- labour as idx_vehicle_entitlements_one_active_holder.
CREATE UNIQUE INDEX idx_vehicle_memberships_one_live
    ON vehicle_memberships (tenant_id, vehicle_token_id)
    WHERE canceled_at IS NULL;

-- Move history: support needs "where has this membership been".
CREATE TABLE vehicle_membership_moves (
    membership_id      UUID NOT NULL REFERENCES vehicle_memberships (id) ON DELETE CASCADE,
    from_token_id      BIGINT NOT NULL,
    to_token_id        BIGINT NOT NULL,
    moved_by_wallet    VARCHAR(43),
    moved_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE tenants ADD COLUMN memberships_enforced BOOLEAN NOT NULL DEFAULT false;
```

Semantics worth pinning down now, because each one has a wrong version that
looks fine in review:

- **Expiry is `expires_at = starts_at + make_interval(months => term)`**,
  computed in SQL at write time so month arithmetic is Postgres's, not Go's,
  and stored rather than derived so renewals can extend it without re-deriving
  history. "Active" is always `canceled_at IS NULL AND expires_at > NOW()` —
  one definition, used by every query.
- **Renewal extends the row**: `expires_at = GREATEST(expires_at, NOW()) +
  term`. Renewing early adds the term to the end; renewing after a lapse starts
  the new term now, not in the past. One row per membership, not a stack of
  rows — a stack is a billing-history feature the purchase flow can introduce
  when there are real purchases to record.
- **Creating over an expired row supersedes it** (sets its `canceled_at`), so
  the partial unique index does not block a fresh start on a vehicle whose old
  membership merely lapsed. Creating over an **unexpired** row is a 409 — the
  operator should renew or move instead, and silently replacing paid time is
  the kind of bug nobody reports until an invoice is wrong.
- **A membership may only be created on / moved to a vehicle the tenant is
  currently entitled to** (active `vehicle_entitlements` row). Refusing is
  cheap here; a membership pointing at a vehicle the customer cannot see is a
  support ticket by construction.
- **Revoking an entitlement does not cancel the membership.** Paid time keeps
  ticking on the row; the console lists memberships whose vehicle is no longer
  entitled as *unassigned in effect* so the operator moves or cancels them.
  Auto-cancel was rejected: entitlement revocation is an access decision, often
  temporary (the discontinued-vehicle case is exactly a revoke-then-move), and
  destroying a commercial record as a side effect of it is the sort of
  irreversible coupling this codebase keeps regretting.
- **Suspended tenants**: nothing extra to do. Suspension already removes all
  access at `/v1/authz` (#23); membership enforcement composes beneath it.

## API surface (this repo)

Same three access layers as everything else on `/v1`; the actor wallet arrives
in `X-Actor-Wallet` exactly as for the customer-management surface.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/v1/tenants/{id}/vehicle-memberships` | Returns `{"enforced": bool, "memberships": [...]}`. One call answers both "is filtering on" and "which token ids", so fleet-lite needs one cached read. Each row carries `id, vehicleTokenId, termMonths, startsAt, expiresAt, canceledAt, status` where `status` is computed: `active` / `expiring_soon` (≤30d) / `expired` / `canceled`. |
| `POST` | `/v1/tenants/{id}/vehicle-memberships` | `{vehicleTokenId, termMonths, startsAt?}`. 409 with the live row when one exists and is unexpired; 422 when the vehicle is not entitled to the tenant. |
| `POST` | `/v1/tenants/{id}/vehicle-memberships/{mid}/move` | `{vehicleTokenId}`. Target must be entitled and hold no live membership; writes a `vehicle_membership_moves` row. |
| `POST` | `/v1/tenants/{id}/vehicle-memberships/{mid}/renew` | `{termMonths}`. Extends as defined above. |
| `DELETE` | `/v1/tenants/{id}/vehicle-memberships/{mid}` | Soft cancel (`canceled_at`), mirroring entitlement revocation. |

Explicit action endpoints (`/move`, `/renew`) rather than a tri-state PATCH —
this programme has been bitten twice by "absent vs empty vs set" JSON
semantics (`scopeGroupIds`, `MergeMemberUpdate`), and move/renew are verbs with
different validation, not field updates.

`memberships_enforced` is exposed on the existing tenant detail and PATCH
(`GET/PATCH /v1/tenants/{id}`), like `fleet_lite_enabled`.

## Step order, and what each costs if it goes wrong

**UI first, against mock data.** The console programme already proved this
works: the entire customer surface was built on `tenancy-stub.ts` (#171)
before the live endpoints existed, and the frontend "already spoke the live
protocol" when the proxies landed (#175). Doing the same here means the UX —
the term dropdown, the move flow, the status badges, the enforcement toggle
copy — gets validated by clicking it, before a migration locks the semantics
in. The wire shapes in "API surface" above are therefore the *contract*: the
stub implements them first, the service implements them identically later.

For the live-wiring steps the deploy rule is the standing one: **this service
first, every time** — a missing route is a 404, kaufmann treats 404 as
failure, so a caller shipped early turns every membership action into a 502.

### 1. b2b console UI, on the stub

Frontend only — no BFF routes, no kaufmann, no tenancy changes:

- `tenancy-service.ts`: `listMemberships / createMembership / moveMembership /
  renewMembership / cancelMembership`, calling the proxied paths that
  steps 4–5 register live.
- `tenancy-stub.ts`: membership fixtures plus the rules, so the demo behaves
  like the real thing will — 409 on an unexpired live row, entitled-vehicles
  only, supersede-on-create over an expired row, renewal extending
  `expiresAt`, an `enforced` flag per stub customer.
- The UI itself, following the customer-detail conventions exactly:
  - **`customer-memberships-panel-element.ts`** — new fourth tab in
    `customer-detail.ts` (`users | vehicles | memberships | settings`).
    Table: vehicle (joined against `GET /fleet/vehicles` for VIN/plate/model,
    the same client-side hydration `listEntitlements()` already does), term,
    starts, expires (`dayjs().fromNow()`), status badge (`active` green,
    `expiring_soon` amber, `expired`/`canceled` grey), actions (move, renew,
    cancel-with-confirm).
  - **Create modal** (imperative idiom, like `provision-user-modal`): vehicle
    picker limited to *entitled* vehicles without a live membership, and a
    plain `<select>` for the term — 1, 12, 24, 36, 48 months.
  - **Move modal**: target picker with the same restrictions; show the
    current vehicle for contrast.
  - **Settings panel**: `memberships_enforced` toggle beside the suspend
    controls, with copy stating the consequence ("customers only see vehicles
    with an active membership in Fleet Lite").
  - Vehicles tab: a membership column/badge on entitled vehicles is cheap
    here and answers "which of these is unpaid" where the operator is
    already looking.
  - All strings through `msg()`; Spanish adds **"Membresías"** to the xliff.

Until the live routes exist, the live path answers the BFF's
`proxy_route_not_registered` 404 — the panel must render that as a calm
"memberships aren't available yet" state, not a broken table, since the UI is
live-by-default and the stub is opt-in (`localStorage.tenancyStub`).

Cost if wrong: nothing — demo mode plus an inert tab. This is where the
cheap iteration happens; changing a modal costs a commit, changing
`vehicle_memberships` semantics after step 3 costs a migration.

### 2. fleet-lite memberships page, mocked

Frontend only, against a mock in the web service module while the backend
endpoint does not exist:

- The dead **Advanced** row in `account-settings.ts` becomes the entry point:
  route `#/:tenantId/memberships`, a read-only `memberships-view` listing the
  tenant's memberships — vehicle, term, expires, status. Copy in the empty
  state explains that vehicles without an active membership are hidden.
  Spanish: **"Membresías"**.
- Ship it dormant — a tenant with no memberships and no enforcement just sees
  the empty state until step 6 wires the real read.

Cost if wrong: cosmetic, customer-visible only as a new settings page.

### 3. fleet-tenancy-api: schema + service + endpoints

Migration, `MembershipService` (next to `EntitlementService`, same
transaction/savepoint discipline), controller, routes, tests including the
supersede/409/entitled-vehicle cases and a clock-boundary test for expiry.
The endpoints must match the contract the stub already implements; where the
two disagree, the divergence is a decision to surface, not to absorb
silently.

Cost if wrong: nothing yet — no live caller until step 4, and `enforced` is
false everywhere. But this is where semantics freeze; it inherits whatever
the stub iteration settled.

### 4. kaufmann-oracle: proxy routes

`internal/gateway/tenancy_memberships.go` + `internal/controllers/` handler +
route registrations, mirroring `tenancy_customers.go` exactly (typed structs,
`doWithActor`, the `fail()` error mapping: 409/400/422 pass through, 403 as
scope fault, `ErrTenancyNotConfigured` → 503, else 502). Routes:

```
GET    /v1/tenancy/customers/:customerID/memberships
POST   /v1/tenancy/customers/:customerID/memberships
POST   /v1/tenancy/customers/:customerID/memberships/:membershipID/move
POST   /v1/tenancy/customers/:customerID/memberships/:membershipID/renew
DELETE /v1/tenancy/customers/:customerID/memberships/:membershipID
```

Gate on `RequireCapability(CapManageMembers)`, consistent with the vehicle
assignment routes it sits beside. (Per-endpoint capability refinement across
this whole surface is already recorded work; do it once, for all of it, not
piecemeal here.)

Cost if wrong: a broken proxy 502s membership admin only — nothing else
touches these routes.

### 5. b2b: BFF routes — the console goes live

Five `oracleApp` entries in `api/internal/app/app.go` (the proxy is
route-by-route; unregistered paths 404 with `proxy_route_not_registered`).
No frontend change needed — step 1's UI already speaks the live protocol, so
registering the routes is the moment the "not available yet" state disappears,
exactly how #175 landed for provisioning.

Deploy order within steps 3–5: tenancy, then kaufmann, then b2b.

Cost if wrong: console-only. Nothing in fleet-lite changes until step 6 *and*
a flag flip.

### 6. fleet-lite-app: the read filter, and the page goes live

Backend:

- `gateway.TenancyAPI.VehicleMemberships(tenant)` → the one-call
  `{enforced, memberships}` read, cached in-process ~60s like authz.
- In `VehicleService`: when `enforced`, intersect with active-membership token
  ids in `ListVehicles`, `GetVehicle` and `AccessibleTokenIDs` — the same
  places `allowedGroupsFilter` applies. **Unconditionally for all members,
  including owners** — group scope is per-member, membership enforcement is
  per-tenant, and applying it only to limited members is the one asymmetry
  that would look correct in every owner-account test.
- An unlicensed vehicle 404s exactly like a nonexistent one (`GetVehicle`'s
  existing convention) — hidden means indistinguishable from absent.
- Fail **closed but flagged**: if the membership read errors while
  `enforced` was last known true, serve the cached set rather than the full
  fleet. Degrading to "show everything" on error is the precise failure shape
  that leaked tenant-wide counts in the geofence bug (#115); not repeating it
  is the point of writing this down.
- `GET /me/access` gains `membershipsEnforced` so the frontend can explain an
  empty garage instead of looking broken.

Frontend: replace step 2's mock with the real read, and invalidate
`FleetCache` (the 24h IndexedDB vehicle cache) when the served vehicle set
shrinks — cheapest correct version: key the cache on the set of token ids
returned, or clear it when `/me/access` reports a change in
`membershipsEnforced`. A stale cache showing hidden vehicles for a day
undermines the entire feature.

Cost if wrong: **this is the expensive step** — it touches the hottest read
path in the customer-facing product, the same territory as the P5a PRs
currently soaking. Everything ships inert behind `memberships_enforced =
false`, so deployment changes nothing; the risk concentrates at flag-flip
time, per tenant, and the revert is flipping the tenant's flag back.

### 7. Turn it on

Per customer, from the console: assign memberships, verify the list, flip the
toggle, confirm in fleet-lite that exactly the membered vehicles remain. First
tenant should be a test tenant (TEST / My Test Fleet pattern), not Kaufmann.

## Sequencing note against current work

Step 6 lands in the same files as the open **P5a** PRs (fleet-lite #115
rewrites `AccessibleTokenIDs` / `allowedGroupsFilter`). Do not develop the
membership filter against pre-P5a code: either land it after P5a merges, or
build it on top of #115's branch. Otherwise the rebase rewrites the exact
lines the soak is meant to validate.

## Considered and rejected

- **Attributes on `vehicle_entitlements`** — rejected as above: conflates
  "may see" with "is paid for", makes moving a membership a revoke+regrant,
  and leaves a future purchase flow nothing to reference.
- **On-chain / attestation representation** — rejected for the same reason
  the `dimo.document.vehicle.tenancy` CloudEvent was dropped from the design:
  it is a record, not a gate, so it buys no isolation; a database row gives
  the same trail more cheaply. Nothing about a membership needs to be visible
  outside these services.
- **Enforcement in kaufmann or in the console** — wrong layer twice over.
  fleet-lite's vehicle set never passes through kaufmann, and the console is
  the *operator's* view, which must show unmembered vehicles precisely so the
  operator can fix them.
- **Hiding via the authz response** — wrong axis. `/v1/authz` answers
  per-wallet questions; membership is per-vehicle, tenant-wide. Bolting token
  id lists onto authz would bloat the hottest cached path with data that
  changes on a different schedule.
- **Membership rows stacked per renewal** — deferred, not rejected forever: a
  purchase flow with real transactions will want purchase records. Until
  money moves, one row with an extending `expires_at` plus the moves table is
  the whole truth.
- **A grace period after expiry** — rejected for v1: a second time constant to
  explain, test, and support. "Expiring soon" in the console is the grace
  period.
- **Backfilling memberships for existing fleets** — rejected: a backfill would
  invent terms nobody chose. The flag default keeps existing tenants exactly
  as they are until an operator makes a deliberate choice per customer.
