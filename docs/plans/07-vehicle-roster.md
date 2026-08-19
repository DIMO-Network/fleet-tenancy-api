# The minted-vehicle roster moves here

Status: **rewritten 2026-08-19, nothing started.** The first draft of this plan
(`07-vehicle-set-coherence.md`, same file, previous commit) recorded the roster
move as *"direction of travel, not a step"* and deferred it on load grounds.
**That deferral is withdrawn.** It was a judgement call about sequencing, and
the evidence gathered since is a stronger argument than the risk it was
weighing: three production vehicles where kaufmann-oracle's stored owner
contradicts the chain, with no mechanism that would ever notice.

The coherence work the first draft described is not discarded — it is steps 1
and 2 here, because it is the prerequisite for the move rather than an
alternative to it.

## What is wrong now

### The incident

TRAST (`f004fc62-752b-4d87-9de9-c20c56e67248`) had nine entitled vehicles, nine
active commercial memberships, and showed its admin **zero vehicles**, silently.
`fleets_lite.vehicles` held one row — `190171`, the entitlement that had been
*revoked* — because the nightly `fleet-lite-app-sync-vehicles` cron builds its
own `VehicleService` and never calls `UseTenancy`
(`api/cmd/fleet-lite-app/sync_vehicles.go:60` against `api/internal/app/app.go:118`),
so every credential-less tenant hits the *"no tenancy client is configured"*
guard and is skipped while the run still exits 0.

Zero rather than one stale vehicle because **the set was a nightly cache and
every gate over it was live**: memberships and group scope both resolve from
this service on 60s TTLs (`membershipTTL`, `groupIndexTTL`,
`GROUPS_FROM_TENANCY=true` in prod). Filtering a cached set by a live predicate
turns staleness into disappearance. Repaired by hand on 2026-08-19 18:03 via
`POST /tenants/{id}/sync-vehicles`; all nine now present, `190171` pruned. **The
cause is untouched** — the cron errors before it writes, so TRAST is correct
until the next entitlement change and then drifts again.

### The copies have already drifted, and one of them is wrong

This is the finding that changed the plan. Comparing `kaufmann_oracle.vins`
against `fleets_lite.vehicles` on the 449 vehicle token ids both hold:

```
imei  missing in fleet-lite   405
vin   missing in fleet-lite   119
plate missing in fleet-lite    57
vin   missing in kaufmann      26
owner DIFFER                    3      <-- not a gap. a contradiction.
```

Tokens `192379`, `192400`, `192401` — Maxus T60s. Kaufmann says
`0xDA13fE288658C594Eac74d41ce9752474d4AD146`; fleet-lite says
`0x97B8bA44C66d2C893925dE41BbDF0eE9b9640E7a`. identity-api, asked directly,
returns `0x97B8bA…` for all three. **Kaufmann is wrong**, and has been since the
transfer.

It is wrong structurally rather than by accident. Every writer of
`vins.owner` is one of kaufmann's own paths — the mint
(`onboard.go:250`), its own transfer workers (`transfer.go:117`,
`transfer_shared.go:155`, the latter after a `time.Sleep(30 * time.Second)`),
and a manual endpoint (`fleet_vehicles.go:1369`). **Nothing reconciles it
against identity-api.** A transfer kaufmann did not perform, or one whose
post-transfer update failed, is permanent divergence with no error, no metric
and nothing that would ever surface it. We found these by diffing two databases
by hand during an unrelated investigation.

### No single service can hold the roster today, and only one could

The 449 shared tokens are not the whole picture:

| | count | meaning |
|---|---|---|
| In kaufmann, not fleet-lite | 27 | onboarded, not (or no longer) in any synced fleet |
| In both | 449 | |
| In fleet-lite, not kaufmann | **178** | vehicles that never came through kaufmann — self-serve tenants' own licenses |

So kaufmann cannot be the roster: it has never heard of 178 vehicles that
exist. fleet-lite cannot be: its table is per-tenant and it is one of several
consumers. This service already holds the tenant-independent facts about who
may see what — entitlements, memberships, groups — for every one of them.

### Where the boundary actually falls

Not "roster versus IMEI mapping". The data draws a sharper line — **device
identity versus vehicle roster**:

```
kaufmann_oracle.vins, by state:
  status  0 (SubmitUnknown)  80 rows   IMEI: 80   token: 0    <- device, no vehicle yet
  status 53 (MintSuccess)   475 rows   IMEI: 475  token: 475  <- both
  status 83 (BurnSDSuccess)   1 row    IMEI: 1    token: 1
```

Eighty rows have an IMEI and no token. They are created **on the hot ingest
path**: an unrecognised device connects, and `coordinator.go:191` inserts a
`vins` row with `OnboardingStatusSubmitUnknown` and no tenant — before anyone
has claimed it, before a vehicle exists. That write cannot be a projection of a
central roster, because at that moment there is nothing to project.

The same path also needs more than IMEI→token→VIN. It reads
`SyntheticTokenID` as the device identifier (`dev := uint64(vehicle.SyntheticTokenID.Int64)`,
`coordinator.go:460`), plus `TenantID`, `Vin`, `VehicleTokenID`, and gates
forwarding on `OnboardingStatus == MintSuccess` (`:292`, `:447`).

And `license_plate` flows the *other* way: `sync_apimaz.go:103` writes plates
from the Chilean registry, so kaufmann is a **source** for that field, not a
consumer of it.

## The destination

Three tables, three owners, no field owned twice.

| | Key | Holds | Written by |
|---|---|---|---|
| `kaufmann.vins` → **device** | IMEI | `vin`, `vehicle_token_id`, `synthetic_token_id`, `tenant_id`, `last_seen`, `onboarding_status` + its workflow columns, `wallet_index`, vendor/connection state, `license_plate` (registry source) | kaufmann, on first contact and through onboarding |
| **this service** → roster | `vehicle_token_id` | `owner`, definition/make/model/year, `minted_at`, `vin`, `license_plate`, alongside the existing entitlements, memberships and groups | reconciled from identity-api; plate fed by kaufmann |
| `fleets_lite.vehicles` | `token_id` | favourites, geofences, TCO, `last_lat/lon/seen` — app-local only | fleet-lite |

Kaufmann keeps exactly what you described plus what the ingest path provably
needs: it drops `owner` and `minted_at` — the two fields it holds that are the
chain's to state and that it has already got wrong — and keeps the device
mapping and its own workflow state.

The key property, and the reason this fixes the incident rather than just
tidying: **the roster and every gate over it are answered by one service at one
time.** They cannot disagree, because there is nothing to disagree with.

This is consistent with the boundary in
[`../operator-tenancy/06-onchain-surface.md:125`](../operator-tenancy/06-onchain-surface.md)
— *the chain records what a vehicle is and who owns it; web2 records who may
look at it*. The roster is a **cache of the chain's answer**, held once and
reconciled, rather than three services each keeping a private guess. It does not
reopen minting, which `06`'s closing section rules out and which stays kaufmann's:
minting is the on-chain operation and needs SD wallets, vendor onboarding, VIN
attestation and a passkey signature.

## Steps

Production changes are acceptable here — load is low and real users are few
(stated 2026-08-19) — so these are sequenced for reversibility rather than for
zero downtime.

### 1. Make the refresh trustworthy, and its failures loud — DONE 2026-08-19

Wire `UseTenancy` into `sync_vehicles.go`; audit the same file for other
by-hand-constructed services missing wires (`UseMemberships`, the group index).
**A skipped tenant must exit non-zero** so the CronJob shows as failed. Add a
`vehicles-diff` command alongside `tenancy-diff` and `groups-diff`, reporting
`agree / missing_local / extra_local` per tenant.

**Cost if wrong:** doing only the wiring is the trap. A silent skip turned a
one-line omission into three days of a customer seeing an empty fleet; leaving
the run green means the next omission costs the same again. This step is also
the only one that helps before anything else lands.

**Shipped in fleet-lite-app#134**, not here — all three changes live in that
repo, `sync_vehicles.go` and the new `vehicles_diff.go`, next to the sibling
diffs. Nothing in this step touched fleet-tenancy-api.

The wiring audit came back negative on purpose: `UseMemberships` and the group
index are read-time filters that the sync path never consults, so wiring them
would be dead weight that reads as coverage. That reasoning is in the code
comment so the next audit need not redo it.

Two things beyond the letter of the step, both needed for the exit code to be
worth anything. The CronJob inherited the chart's 1-hour
`ttlSecondsAfterFinished` — which is *why* the skip was never seen; the pod was
gone before anyone looked — and `backoffLimit: 1`, which would retry a
deterministic skip and double the log. Both now match `groups-diff`: `0` and
three days. And `vehicles-diff` got its own CronJob at 03:30, half an hour after
the sync it gates.

Verified against prod the same day. The bug was reproduced first on the deployed
image — TRAST skipped, `synced=612 skipped_tenants=1`, job `exitCode=0`,
condition `Complete` — then the fixed binary run against prod gave
`synced=621 skipped_tenants=0`, the difference being exactly TRAST's nine, with
`vehicles-diff` clean at `agree=9`. The failure path was confirmed too: a run
made before identity-api was reachable exited 1, logged at error level and named
the tenant. The chart values are the one part still unverified — they cannot be
until it deploys.

### 2. Stop the freshness mixing

Resolve the set and its gates together, here: entitled ∩ active memberships ∩
group scope. `fleets_lite.vehicles` stops being authoritative for set membership
and becomes a metadata cache joined by token id.

**A token in the resolved set with no local metadata row must still appear**,
with whatever is known. That inversion is what turns the incident's empty list
into nine vehicles with thin metadata.

**Cost if wrong:** an inner join, or any "skip tokens we have no row for", moves
the bug somewhere harder to see — the set will be provably correct while the
response is still short. Not done unless the missing-row case has a test.

### 3. Stand up the roster, reconciled from the chain

A `vehicles` table here keyed by `vehicle_token_id`, populated and refreshed from
identity-api, holding owner, definition, `minted_at`, VIN and plate. Reconciliation
is the point: **the owner column must be re-read from identity-api on a schedule,
not written once by whoever performed a transfer.** Seed it from identity-api
directly rather than from either existing table — both are known-wrong.

Report, don't silently fix, on first run: the 3 owner contradictions and the 27
kaufmann-only tokens are diagnostic and should be read by a human once.

**Cost if wrong:** seeding from `vins` imports the three bad owners as truth and
launders a known error into the new source of truth. Seed from the chain, and
diff against both existing tables as a check rather than a source.

### 4. Cut the readers over

fleet-lite's vehicle list resolves set *and* metadata from step 3's endpoint.
Kaufmann's b2b-facing vehicle reads follow. `fleets_lite.vehicles` narrows to
app-local columns.

Do this behind a flag per reader, as the groups move did with
`GROUPS_FROM_TENANCY` — that flag's existence is why the group cutover was
revertible, and it is the pattern to copy.

**Cost if wrong:** this service becomes load-bearing for every fleet page render.
It is already what both apps fail closed on, and it now also holds River and a
gas-spending path. Measure p99 on `/v1/authz` before and after each reader, and
keep the flag until a full release has run clean.

### 5. Shrink `vins` to the device table

Drop `owner` and `minted_at` from `kaufmann_oracle.vins` once nothing reads
them. Drop the read paths first, ship, then drop the columns — the same staging
`06` uses, for the same reason.

Keep `synthetic_token_id`, `wallet_index`, `onboarding_status`, the vendor and
connection columns, `tenant_id`, `vin`, `imei`, `license_plate`, `last_seen`.
Every one has a live reader on the ingest or onboarding path, evidenced above.

**Cost if wrong:** dropping a column the coordinator reads breaks telemetry
ingestion for the whole fleet — the highest-traffic path in the system, and the
one with no user-visible error until data stops arriving. Verify against
`internal/oracle/coordinator.go` field by field, not against this table.

## Considered and rejected

**Keep the roster in kaufmann and have others read from it.** It is where
onboarding happens and it already holds most fields. Rejected on the numbers:
178 vehicles exist that kaufmann has never seen, so it would be a roster with a
permanent hole, and the hole is exactly the self-serve population.

**Reconcile the existing copies instead of centralising** — a job that re-reads
identity-api and repairs `vins.owner` and `fleets_lite.vehicles` in place.
Genuinely cheaper, and it would have fixed the three. Rejected because it makes
the drift a maintained feature: every field added to either table needs another
reconciliation leg, and a reconciler that falls behind produces exactly the
silent disagreement we are trying to remove. It is also strictly more code than
step 3 once step 3 exists.

**Strip `vins` to IMEI + token + VIN, as first proposed.** Rejected on the
ingest path: the coordinator also needs `synthetic_token_id` as the device
identifier, `tenant_id`, and `onboarding_status` to decide whether to forward at
all — and it *inserts* rows for unknown IMEIs before a vehicle exists, which 80
of 556 rows are. The narrower split in *The destination* keeps the intent and
survives the code.

**Make everything stale together — read the gates from local mirrors.** Fixes
the mixing, and would have shown one wrong vehicle rather than zero. Inverts why
the gates exist: memberships are the commercial control and a stale copy sells
lapsed access; group scope is an access boundary and a stale copy shows a
limited member vehicles they were just removed from. Staleness is tolerable in
metadata and not in authorization.

**Event-driven invalidation instead of polling.** The right long-term answer,
and `contract_event_processor` exists in the cluster. Rejected as this plan's
mechanism: none of these three services consumes any stream today — all poll,
including this service's own `*/10` attestation publisher — so it is a new
capability rather than a change to an existing one. It also would not have
prevented the incident, which was caused by mixing rather than by interval.
Worth doing after step 3, where it becomes one writer's concern instead of
three.

**Drop the local caches and query identity-api per request.** Removes staleness
entirely. Rejected on failure mode: it puts identity-api on the critical path of
every fleet render, and its outage becomes an empty fleet for every customer at
once — a rare silent failure traded for a frequent loud one.
