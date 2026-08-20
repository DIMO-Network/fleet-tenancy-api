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

Verified against prod the same day, before and after release. The bug was
reproduced first on the deployed image — TRAST skipped,
`synced=612 skipped_tenants=1`, job `exitCode=0`, condition `Complete` — and
then, on `v0.16.0` in-cluster on the real CronJob,
`TRAST synced=9, skipped_tenants=0, synced=621`, job `Complete`, with
`vehicles-diff` clean at `entitled=9 local=9 agree=9`. The failure path was
confirmed too: a run made before identity-api was reachable exited 1, logged at
error level and named the tenant.

**The chart values are verified, by a real failure rather than a synthetic one.**
Merging shipped the chart to prod while leaving the binary behind (see below), so
for half an hour prod ran the new CronJob against an image with no `vehicles-diff`
subcommand. It exited 2, the job went `Failed`, **exactly one pod was created** —
`backoffLimit: 0` holding — and it persisted on the three-day TTL instead of
being collected within the hour. That is precisely the property the incident
lacked.

**Merging does not deploy this app, and that is a trap worth knowing before the
next change that touches both code and chart.** `values.yaml` is bumped by
`buildpushdev` on merge to main, but prod's image tag lives in
`values-prod.yaml` and only moves when a `v*` tag is pushed
(`.github/workflows/buildpushprod.yml`). `cronJobs` has no prod override, so
merging shipped the *chart* to prod at once while the *binary* stayed on the
previous release — a `vehicles-diff` CronJob firing nightly at 03:30 against an
image that had never heard of it. Released as `v0.16.0` to close the gap.

One more, for whoever edits the chart next: the version-bump workflow
round-trips `values.yaml` through a YAML parser and writes it back, **stripping
every comment in the file**. Two explanatory comments were gone one commit after
they landed. Durable rationale belongs in `templates/cronjobs.yaml`, which is
not rewritten (fleet-lite-app#135).

### 2. Stop the freshness mixing — DONE, released `v0.17.0` 2026-08-20

Resolve the set and its gates together, here: entitled ∩ active memberships ∩
group scope. `fleets_lite.vehicles` stops being authoritative for set membership
and becomes a metadata cache joined by token id.

**A token in the resolved set with no local metadata row must still appear**,
with whatever is known. That inversion is what turns the incident's empty list
into nine vehicles with thin metadata.

**Cost if wrong:** an inner join, or any "skip tokens we have no row for", moves
the bug somewhere harder to see — the set will be provably correct while the
response is still short. Not done unless the missing-row case has a test.

**Shipped in fleet-lite-app#136**, released as `v0.17.0` on 2026-08-20. No new endpoint was needed here:
all three gates were already exposed and already called, and the mixing existed
only because the set came from a different place than the gates. So this too
landed entirely in fleet-lite-app.

`mergeResolvedVehicles` is a pure function precisely so the missing-row case is
a test rather than an intention — `TestMergeResolvedVehiclesMissingRow`, plus
the all-missing case a freshly-entitled customer hits before any sync has run.
`GetVehicle` got the same treatment; its 404 on an entitled-but-uncached token
was the single-vehicle face of the same bug.

**One thing this step nearly got wrong, worth carrying forward.** The membership
gate and group index are both cached for 60s. Reading the entitled set live
against them would have reintroduced the identical mixing with the staleness on
the other foot — a fresh set filtered by stale gates. The entitled read is
therefore cached at the same TTL, and `entitledTTL` is asserted equal to
`membershipTTL` in a test rather than left to a comment. Any future leg of this
intersection must age with the others.

Verified against prod: for TRAST, `RESOLVED_COUNT=9` with
`MEMBERSHIP_ENFORCED=true ACTIVE_COUNT=9` — the intersection genuinely runs
rather than passing through — and an empty group scope resolved to 0 against
real group data.

### 3. Stand up the roster, reconciled from the chain — DONE, LIVE IN PROD 2026-08-20

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

**Released as `v0.15.0`, 2026-08-20 02:05 UTC, and the roster is populated.**
`vehicles` keyed by `vehicle_token_id`, `vehicle_owner_changes` beside it, a
`reconcile-vehicles` command and a 04:00 CronJob.

**The population is the union of privileged sets over every licence in
`tenant_credentials`** — not `vehicle_entitlements`. Entitlements cover
explicit-mode tenants only and would have left out the 178 vehicles belonging to
self-serve tenants' own licences: a roster with a permanent hole, which is
exactly what disqualified kaufmann from holding it. Sweeping licences also
self-heals as tenants come and go, and needs no cross-database access.

Three decisions the plan did not spell out, each with a test:

- **Owner is re-read and compared every run**, and a change is written to
  `vehicle_owner_changes` as well as applied. Without the log the job would
  silently correct the three known-wrong owners and leave nothing to show it
  happened, so the next unexplained transfer would be as invisible as this one.
- **VIN and plate are filled forward, never cleared.** identity-api serves
  neither, and kaufmann writes plates from the Chilean registry — so for that
  field this table is a consumer. A wholesale upsert would blank a known plate
  nightly: this plan's own failure mode, pointed the other way.
- **A partial sweep never marks anything unseen.** If a licence fails, the
  vehicles behind it are indistinguishable from vehicles nobody can see any
  more, and stamping them would record an identity-api outage as a fleet
  change. The run exits non-zero instead. Vehicles genuinely gone are stamped,
  never deleted — losing sight of one is usually a revoked SACD, and the row is
  the only record we ever knew it.

**The diagnostic could not live in the service.** Each service's DB user is
scoped to its own schema, so `fleet_tenancy_api` cannot read `fleets_lite` or
`kaufmann_oracle` — a deliberate isolation property not worth weakening for a
one-time read. It is `scripts/roster-diagnostic.sh`, run from a workstation
holding all three credentials; it writes nothing and prints the contradictions,
the kaufmann-only tokens and the roster's coverage gaps with the expected counts
inline.

**Also fixed here:** this chart's `cronjobs.yaml` still used Helm's `default`
for numeric overrides, so `backoffLimit: 0` would have been silently replaced by
1 — the same trap fleet-lite's chart documents. Ported the `hasKey` form and the
reasoning with it.

**Coverage, measured rather than assumed.** The sweep was run at full prod scale
— prod's ten real licences against prod identity-api, writing to a local
database — and the result compared against both source tables:

| | |
|---|---|
| roster | **619** |
| union of `vins` + `fleets_lite.vehicles` | 655 |
| in the union, not the roster | **45** (26 kaufmann-only, 16 fleet-lite-only, 3 in both) |
| in the roster, not the union | 9 |

The 45 are vehicles **no licence we hold is privileged on** — the plan's own
description of the kaufmann-only 27 is "onboarded, not (or no longer) in any
synced fleet", and that is what this measures. It is a different thing from
kaufmann's hole: kaufmann structurally could not see 178 vehicles that a
customer was actively using, whereas these are vehicles nobody's credential can
currently read. **None of them is entitled to anybody** — all nine active
entitlements are covered — so no customer is affected today.

It is bounded rather than closed, and deliberately: identity-api answers
`vehicle(tokenId:)` for any token without privilege, so any of the 45 is
*reachable*; what is missing is a way to *learn* their token ids, since only
kaufmann's table names them and this service cannot read it. If that matters
later, the honest fix is kaufmann publishing its onboarded token ids, not this
service reaching into another schema.

**What is guaranteed:** an active entitlement's vehicle is always in the roster.
The reconcile fills any entitled token the sweep could not enumerate via a
single lookup, because once readers cut over in step 4, an entitled vehicle
missing from the roster IS the empty-fleet incident again, one layer down.

Verified before deploying: the migration applies and reverses cleanly; twelve
service tests run against a real database; and the identity-api query was run
against prod through the gateway, returning **553 vehicles over six pages** with
owner, `mintedAt` and definition parsed for every one, no repeated token ids,
and an empty client id refused. 553 is the same count fleet-lite's sync reports
for that licence, so the pagination is complete rather than truncated.

**The diagnostic was run, 2026-08-19 ~22:00 UTC, and the plan's numbers hold:**
3 owner contradictions (192379, 192400, 192401 — kaufmann `0xda13fe28…`, chain
`0x97b8ba44…`), 27 kaufmann-only, and 179 fleet-lite-only against a documented
178 — one vehicle more than the morning's count, which is what a live platform
does between two measurements. Treat the contradiction count as the assertion
and the population counts as context.

**And the roster corrects them.** After a full prod-scale reconcile, all three
T60s read `0x97B8bA44C66d2C893925dE41BbDF0eE9b9640E7a` — the chain's answer, not
kaufmann's. A second run reported `inserted=0 updated=619 owner_changes=0`, so
the steady state is quiet and a real transfer will not hide in noise.

### Run in production, 2026-08-20

The migration applied on boot. A `-dry-run` job was read first, as this step
asks: `licences=10 vehicles_seen=619 inserted=619`, no licence failures, exit 0
— matching the local prod-scale rehearsal exactly. Then the real run:

```
first  : licences=10 vehicles_seen=619 inserted=619 updated=0   owner_changes=0 entitled_filled=0
second : licences=10 vehicles_seen=619 inserted=0   updated=619 owner_changes=0 entitled_filled=0
```

619 rows, every one with owner, `minted_at` and definition; `unseen_since` null
throughout. `vehicle_owner_changes` holds 619 first observations and **0
transfers** — the history starts where we started looking, which is the honest
place for it to start.

**In prod, `fleet_tenancy_api.vehicles` now says `0x97B8bA44…` for 192379,
192400 and 192401.** `kaufmann_oracle.vins` still says `0xDA13fE28…` and was not
touched: this service reconciles its own table and does not write across a
schema boundary. Step 5 removes that column once nothing reads it.

`entitled_filled=0` — all nine active entitlements were covered by the licence
sweep, so the individual-lookup path did not fire. It is insurance for step 4
rather than something prod needs today.

**Nothing reads this table yet.** It is populated and reconciling nightly at
04:00; the reader cutover is step 4.

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

#### The endpoint exists — `POST /v1/tenants/{tenantId}/vehicle-metadata`

**Released as `v0.16.0`, 2026-08-20, and running in prod. No caller yet.** Step 3 stood the table up and the
plan then said "step 3's endpoint" as though there were one; there was not.
This is it, and it is the whole of this repo's share of step 4 — the cutover
itself is a flag and a code path in each reader.

It answers **one** question: given token ids, what are these vehicles? It does
**not** resolve the set. That is deliberate and it is the load-bearing decision
of this step:

- The intersection — entitled ∩ active memberships ∩ group scope — already runs
  in fleet-lite against three endpoints of this service, shipped as step 2 in
  `v0.17.0`, with the equal-TTL property tested. Re-implementing it behind a new
  endpoint would put a second opinion about group scope in the codebase, which
  is the duplication this plan exists to remove — and it would be a *silent*
  second opinion, since the two would only disagree for members whose scope had
  just changed.
- So step 4 changes the **metadata source only**: fleet-lite keeps resolving the
  set exactly as it does today and swaps `fleets_lite.vehicles` for this call as
  the thing it joins against. That is a strictly smaller change than the plan's
  sentence implies, and it is the one the flag can revert cleanly.

Shape, and the reasoning that is not obvious from it:

- **POST with `{"tokenIds": [...]}`, because a real fleet does not fit in a
  query string.** 619 token ids is several kilobytes of request line and fiber
  refuses it while reading the request — before any handler or gate runs, with
  an error that names the read buffer rather than the URL. Same reasoning as
  `shareable-owners`, and asserted in a test
  (`TestVehicleMetadataAsQueryStringWouldNotFit`) because "make it a GET, it's a
  read" is the obvious review note and this is the answer to it.
- **A token with no roster row is absent from the response, not an error.** The
  caller must keep its left join: absence means the roster has not seen the
  vehicle yet — a customer entitled minutes ago, before the 04:00 reconcile —
  and dropping it from the rendered list would be the empty-fleet incident again
  one layer down, which is trap 2 of this step. Both the missing-one and
  missing-all cases are tests here as well as in fleet-lite.
- **The tenant in the path authorizes the caller, in the ordinary way; it is not
  a per-vehicle filter.** The roster is keyed by token id alone precisely
  because owner and definition are properties of a vehicle, not of anybody's
  relationship to it, so there is no tenant column to filter on and no honest
  way to invent one. What bounds the endpoint is that the caller must **name**
  the tokens: no listing, no wildcard, no cursor, so nothing is learnable here
  that was not already known somewhere it was gated. Worth stating plainly
  because "tenant-scoped" reads like a data filter and here it is not.
- Owner comes back EIP-55 checksummed whatever case the row holds, so a caller's
  string comparison against its own stored address cannot silently miss.
  `reconciled_at` and `unseen_since` are served too: staleness should be a
  timestamp the caller can show, not something inferred from absence.
- 5000 token ids per request, far above the whole 619-row roster — a bound on
  one request becoming an unbounded query, not a constraint on real use.

**Not done and deliberately not started here:** the readers. fleet-lite behind
its flag, then kaufmann's b2b-facing reads, then `fleets_lite.vehicles` narrowed
to app-local columns. Nothing calls this endpoint yet, so releasing it changes
no behaviour anywhere — which is the point of shipping it separately from the
first cutover.

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
