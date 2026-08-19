# The vehicle set and the gates over it must agree

Status: **written 2026-08-19, nothing started.** Prompted by a live incident:
TRAST (`f004fc62-752b-4d87-9de9-c20c56e67248`) has nine entitled vehicles, nine
active commercial memberships, and showed its admin **zero vehicles** in
fleet-lite. Nothing errored. The diagnosis is in
[What is wrong now](#what-is-wrong-now); it is not the bug it first looks like.

This plan is mostly about fleet-lite-app, and lives here because the fix moves
authority toward this service and because the failure is a property of the
boundary rather than of either side.

## What is wrong now

### The symptom: a correct set, a correct gate, an empty answer

For TRAST, on 2026-08-19:

| | value |
|---|---|
| Active entitlements (this service) | 9 — `193196`, `193491`–`193494`, `193498`, `193499`, `193552`, `193556` |
| Active vehicle memberships (this service) | the same 9, `memberships_enforced = true` |
| Rows in `fleets_lite.vehicles` | **1** — `190171`, last written 2026-08-17 |
| Vehicles shown | **0** |

`VehicleService.ListVehicles` reads local rows and appends
`membershipFilter` — `token_id = ANY('{193196,…}')` — which, by its own comment,
**applies to owners and admins too**. The one local row is `190171`, which is
not in the membership set. `190171` is also the single entitlement that was
*revoked* on 2026-08-18 19:49.

So the local table's only row is one that should not be there, and the gate
correctly hides it. Both halves behaved as designed and the customer saw an
empty garage.

### The proximate cause: the nightly sync skips a whole tenant class

`fleets_lite.vehicles` is a cache of identity-api refreshed by the
`fleet-lite-app-sync-vehicles` CronJob (`0 3 * * *`). For a credential-less
tenant `SyncVehicles` routes to `syncEntitledVehicles`, which opens with:

```go
if s.tenancy == nil || !s.tenancy.Configured() {
    return 0, fmt.Errorf("tenant %s has no DIMO client ID and no tenancy client is configured", tenant.ID)
}
```

`UseTenancy` is called on a `VehicleService` in exactly one place —
`api/internal/app/app.go:118`, the web server. The cron command builds its own
(`api/cmd/fleet-lite-app/sync_vehicles.go:60`) and never calls it. The loop
treats the error as `sync vehicles, skipping tenant` and continues, so the run
reports success.

The data shows it precisely. Every tenant holding its own credential synced at
03:00 today; the one credential-less tenant has not been touched since the
16th:

```
TRAST               credential_less=t    1 row    2026-08-17 14:13
My Test Fleet       credential_less=f   45 rows   2026-08-19 03:00:07
TEST                credential_less=f    7 rows   2026-08-19 03:00:06
Fresh Coast Garage  credential_less=f    6 rows   2026-08-19 03:00:07
```

TRAST's only successful sync was the one-shot `OnMirrorCreated` hook
(`app.go:126`) on 2026-08-16 01:23, when `190171` was its sole entitlement. The
job pods are garbage-collected, so the skip line itself is no longer readable —
the mechanism is from the code and that timing pattern, not from a log.

### The actual cause: a cached set filtered by live gates

Fixing the cron wire would restore TRAST and leave the failure class intact.
The reason zero appeared — rather than one stale vehicle — is that **the set and
the predicates over it are resolved from different places at different times.**

| Input to the vehicle list | Source | Freshness |
|---|---|---|
| The set of rows | `fleets_lite.vehicles` | nightly cron, or never |
| Membership gate | this service, live | 60s (`membershipTTL`) |
| Group scope gate | this service, live | 60s (`groupIndexTTL`), `GROUPS_FROM_TENANCY=true` in prod |

Had all three been stale, TRAST would have shown one wrong vehicle — visibly
wrong, and someone would have asked. Had all three been live, nine correct ones.
**Zero is specifically the artifact of mixing**, and it is the worst of the three
outcomes because it is silent: no error, no warning, an empty list that looks
exactly like a customer who owns nothing.

This generalises, and it is getting worse rather than better. Filtering a cached
set by a live predicate converts staleness into *disappearance*. Two such gates
exist today and both landed recently (memberships in plan 02, the group index in
the groups move's P5). Each one added over the same cached set multiplies the
chance of disagreement, and every disagreement reads to a customer as "my
vehicles are gone."

### What is genuinely duplicated, and what only looks it

Worth stating because it bounds the fix. Comparing the columns:

| Fact | Owner | Cached in |
|---|---|---|
| token id, owner, VIN, definition, `minted_at`, IMEI, plate | chain → identity-api | **`kaufmann.vins` *and* `fleets_lite.vehicles`** |
| onboarding status, vendor connection, SD token, wallet index | kaufmann-oracle | `kaufmann.vins` only |
| entitlement, commercial membership, groups | **this service** | live |
| favourites, geofences, TCO, last location, make/model/year | fleet-lite | `fleets_lite` only |

`kaufmann.vins` is **not** a duplicate roster to absorb. It is device lifecycle,
keyed by VIN and IMEI, and its rows exist before a token does — the same reason
[`06-signer-key-consolidation.md`](06-signer-key-consolidation.md) rejected
moving kaufmann's operations here. The only real duplication is that both apps
independently cache identity-api's projection of a minted vehicle, and that is
not what broke.

## The destination

**The set of vehicles a tenant has, and every gate that narrows it, are answered
by one source at one time.** Whatever caching sits behind that answer is an
implementation detail invisible to the caller, and a cache miss is a hole to
fill rather than a vehicle to hide.

This service already owns three of the four inputs — entitlements, memberships,
groups. Only the materialised intersection escaped into fleet-lite, and it
escaped into the one place that cannot keep it fresh.

The programme's own boundary splits this the right way:

> the chain records what a vehicle is and who owns it; web2 records who may look
> at it
>
> — [`../operator-tenancy/06-onchain-surface.md:125`](../operator-tenancy/06-onchain-surface.md)

*What it is* is identity-api's. *Who may look at it* is this service's — and the
roster is the second thing. Note this is a different question from minting,
which `06`'s closing section rules out and which this plan does not reopen:
minting is the on-chain **operation**, needing SD wallets, vendor onboarding,
VIN attestation and a passkey signature. A roster is not a mint.

## Steps

### 1. Make the refresh trustworthy, and make its failures loud

Three changes, one release:

- Wire `vehicleSvc.UseTenancy(tenancyAPI)` in `sync_vehicles.go`, as `app.go`
  does. Check `UseMemberships` and the group index too — the cron constructs
  services by hand and any other missing wire has the same shape.
- **A skipped tenant must not exit 0.** The run reported success while skipping
  the only tenant that needed it. Count skips, log each with its reason, and
  exit non-zero if any tenant was skipped, so the CronJob shows as failed.
- Add a `vehicles-diff` command in this service, alongside `invitations-diff`
  and `groups-diff`: for every explicit-mode tenant, compare the active
  entitled set against what fleet-lite holds locally, and report
  `agree / missing_local / extra_local`. TRAST would have reported
  `missing_local=9, extra_local=1` from 2026-08-18 onward.

**Cost if wrong:** this is the step that stops the bleeding, and doing only the
first bullet is the trap. A silent skip is what turned a one-line wiring
omission into three days of a customer seeing an empty fleet; leaving the run
green means the next omission — a new tenant class, a new service the cron
forgets to wire — costs the same three days. The diff is what makes the
invariant checkable without waiting for a customer to report it.

### 2. Stop the freshness mixing

Resolve the **set** and its gates together, from this service, per request:
entitled ∩ active memberships ∩ group scope, computed here where all three rows
live. `fleets_lite.vehicles` stops being the authority on membership of the set
and becomes a metadata cache joined by token id.

The inversion that matters: **a token in the resolved set with no local metadata
row must still appear**, with whatever fields are known, rather than being
dropped by the join. That single change is what turns today's failure from an
empty list into nine vehicles with thin metadata.

Serve it from one endpoint here so the three gates cannot be composed
differently by different callers, and so a caller cannot accidentally apply two
of the three.

**Cost if wrong:** an inner join, or any "skip tokens we have no row for", moves
the bug rather than fixing it — and moves it somewhere harder to see, because
the set will then be provably correct while the response is still short. If this
step ships without the missing-row case explicitly tested, it has not been done.

### 3. Fill holes instead of waiting for the cron

With step 2 in place a missing metadata row is visible and harmless, so it can
be repaired opportunistically: on a list request that resolves tokens absent
from the local cache, enqueue a metadata fetch for those tokens rather than
blocking the response.

This is what actually reduces the nightly cron from a correctness mechanism to a
housekeeping one. It stays a reconciliation pass — worth keeping even under
event-driven invalidation — but it stops being the only thing standing between a
customer and an empty fleet.

**Cost if wrong:** a fetch on the request path, done synchronously or without a
bound, turns a slow identity-api into a slow fleet list for every customer. It
must be fire-and-forget with a cap, and a failure to fill must leave the vehicle
listed with thin metadata rather than removing it.

### Direction of travel, not a step

Moving the roster table itself into this service — the identity projection
alongside the entitlements, with fleet-lite keeping only favourites, geofences,
TCO and location — is the coherent end state, and steps 1–3 are prerequisites
for it regardless.

It is deliberately not scheduled. **This service is what both apps fail closed
on.** It has just taken on River, a bundler connection and a gas-spending path
next to the `/v1/authz` hot path; putting fleet-list resolution behind every
fleet page render adds request-rate load to the one service whose outage is a
two-app outage. Step 2 already adds some of that load and should be measured
before more is added. Revisit when there is data on what step 2 costs.

## Considered and rejected

**Fix the cron wire and stop.** Cheapest, and restores TRAST today — it is step
1's first bullet, kept for exactly that reason. Rejected as the whole answer
because it leaves a cached set under two live gates, with a third arriving
whenever the next per-vehicle rule does. The next stale window produces the same
silent empty list from a different cause.

**Make everything stale together: read the gates from local mirrors.** Genuinely
fixes the mixing, and would have shown one wrong vehicle instead of zero. It
inverts every reason the gates were built: the membership gate is the commercial
control and a stale copy sells access that has lapsed, the group scope is an
access boundary and a stale copy shows a limited member vehicles they were just
removed from. Staleness is tolerable in metadata and not in authorization.

**Event-driven invalidation instead of polling.** The right long-term answer to
freshness, and `contract_event_processor` exists in the cluster. Rejected as
this plan's mechanism because none of these three services consumes any stream
today — every one of them polls, including this service's own `*/10` group
attestation publisher — so it is a new capability, not a change to an existing
one. It also would not have prevented this incident: the mixing, not the polling
interval, is what produced zero. Worth doing on its own terms, after step 2
makes correctness independent of it.

**Drop the local cache and query identity-api per fleet-list request.** Removes
the cache and therefore the staleness. Rejected on load and on failure mode: it
puts identity-api on the critical path of every page render, and its outage
becomes an empty fleet list for every customer at once — trading a rare silent
failure for a frequent loud one.

**Have kaufmann push vehicle changes to fleet-lite.** Both already know about the
same vehicles, so a direct notification looks natural. Rejected because it wires
the two apps together in the direction the whole tenancy programme is unwinding,
and because kaufmann does not know a customer's entitled set — this service
does. A push that cannot answer "for which tenant?" is not the fix.

**Move the roster to this service now.** See *Direction of travel*. Not rejected
on merit — deferred until step 2's load is measured, because the blast radius of
being wrong here is both apps.
