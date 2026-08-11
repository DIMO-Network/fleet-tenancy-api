# Handoff — operator-managed multi-tenancy

Written 2026-08-06. Read this plus
`fleet-lite-app/docs/operator-tenancy/` (the full design set — it is gitignored
in this repo until the weaknesses it documents are fixed).

## The goal in one paragraph

Two unrelated multi-tenant systems (`fleet-lite-app`, `kaufmann-oracle`) become
one operator-managed model: an operator configures customer tenants from
`b2b-fleet-mgr-app`, end customers use `fleet-lite-app`, and this service owns
tenants, users, memberships, delegations and vehicle entitlements. Nine
decisions are locked — see `README.md` in the design set.

## Do these first

**R1 is complete.** `DROP_FOREIGN_TENANT_GROUPS` is `true` in prod as of
2026-08-06, verified: a full sync afterwards reported `checked=576, changed=0`
with zero reconcile events, and the 287 grouped-vehicle memberships are intact.
The pre-flight was a dry run with the flag forced on — `reconcile` computes the
whole add/remove set before the `dryRun` check, so it previewed the change
exactly rather than approximately.

### DEPLOYED 2026-08-10 — the service is live in prod

ArgoCD `fleet-tenancy-api` is Synced/Healthy, 2/2 pods, zero restarts, no errors
of its own. `20260805120000_init.sql` applied cleanly on first boot and the
second pod correctly found nothing to do.

**There is no URL.** The chart ships no ingress on purpose, so nothing is
publicly reachable. In-cluster it is:

```
http://fleet-tenancy-api.prod.svc.cluster.local:8084
```

Verified from inside the cluster: `/health` returns `{"status":"up"}`,
`/version` returns the deployed commit, and `/v1` returns 401 to an
unauthenticated caller.

**Minor, worth fixing when convenient:** `ErrorHandler` logs every non-404 at
error level, so each 401 on `/v1` appears in the error stream. Expected client
errors will look like server errors once callers integrate, and will feed any
error-rate alerting. 4xx belongs at warn or debug.

The pre-flight below is kept because it records what was checked and why.

Pre-flight done 2026-08-07, all green:

| Check | Result |
|---|---|
| All four secrets exist at the exact `remoteRef` names | ✅ |
| `tenant_secret_enc_key` is 64 hex chars, no trailing whitespace | ✅ |
| Prod image published, and `values-prod.yaml` pins it | ✅ `e8fb4ea` (`v0.1.1`). It pinned a non-existent `0.1.0` until `v0.1.0` was cut, then the superseded distroless build until `v0.1.1` — **a values-only merge does not repin prod; only a `v*` tag does** |
| App role can connect to `fleet_tenancy_api` | ✅ |
| `CREATE SCHEMA` + `CREATE EXTENSION pgcrypto` + `gen_random_uuid()` | ✅ — tested as the exact sequence migrate runs, in a rolled-back transaction |

The pgcrypto one was worth checking rather than assuming: the role is **not**
`rds_superuser` and the extension was not installed, which on older Postgres
would have failed the very first migration statement. It works because pgcrypto
is a *trusted* extension on PG 13+ and the role holds `CREATE` on the database.
If a future environment runs older Postgres, or the role loses `CREATE`, that is
where the deploy will break — and it breaks in the migrate init container, so
`kubectl logs <pod> -c fleet-tenancy-api-migrate` is the first place to look.

The Application spec mirrors `fleet-lite-app`: `path: charts/fleet-tenancy-api`,
`valueFiles: [values-prod.yaml]`, namespace `prod`, automated sync with prune.
Note that automated sync means it deploys the moment it is registered.

### Reference: the AWS secrets

The service is deployable but **not deployed**, and deliberately so: nothing
exists in ArgoCD for it yet, so merging its chart deploys nothing. An image does
now exist — `buildpushdev` builds one on every merge to main, and the first is
`dimozone/fleet-tenancy-api:5b7ee68`. A `v*` tag builds the prod image and
rewrites `values-prod.yaml`, exactly as in the sibling repos.

Three secrets, at `prod/fleet-tenancy-api/`:

| Key | Note |
|---|---|
| `db_user`, `db_password`, `db_host` | The prod database is already provisioned |
| `tenant_secret_enc_key` | **Treat as permanent** — see below |

All three must exist **before** the chart is applied: a missing `remoteRef`
fails the whole ExternalSecret, not just that key, so the pod gets no database
credentials either and never starts.

`tenant_secret_enc_key` deserves a moment's thought rather than a quick
`openssl rand`. It is the key every tenant credential is encrypted under. The
service refuses to boot outside local if it is empty — deliberately, because
`sha256("")` is a valid AES-256 key, so an unset value does not fail, it
encrypts everything under a constant anyone can compute. That exact failure
reached production in fleet-lite-app. And once the backfill has written
credentials under it, changing it means re-encrypting every row.

## Backfill — RUN AND VERIFIED 2026-08-10

Production now holds 15 tenants (11 operator, 4 self-serve), 15 credentials, 153
users and 164 memberships. All 15 tenants' secrets decrypt under the key the pod
actually reads — verified with the value from the k8s secret, not AWS.

It took three runs, because the first exposed two ways the migration was not
faithful to its sources. Both were latent: nothing consumes `/v1/authz` yet.

**Group scope was never migrated.** `access_fleet_groups` had zero references in
`backfill.go`, so every kaufmann membership was written `scope_group_ids = NULL`
— which here means *unrestricted*. In kaufmann a member sees every fleet only
with `view_all_fleets`, and otherwise sees exactly their assigned groups, often
none. That silently granted **131 of 159 memberships** the whole 524-vehicle
fleet. Fixed in `#10`; the mapping is now

```
view_all_fleets        -> NULL          unrestricted
groups assigned        -> {those ids}   restricted to them
no view_all, no groups -> {}            restricted to nothing
```

`{}` is not `NULL`. `Unrestricted()` tests for nil, so the empty array is "sees
nothing" and nil is "sees everything" — the inversion that made the omission
dangerous rather than merely incomplete.

**Capabilities were copied verbatim**, so 28 memberships carried
`manage_admin_users` rather than `manage_members` and would have been refused
member management, and 25 carried the dead `view_all_fleets`.

**A membership can exist in both sources** — the Kaufmann tenant does, now that
the uuids are unified — and whichever wrote last won outright, demoting a real
kaufmann admin to `role=member` with no capabilities. `#11` reads both sources,
merges per (tenant, wallet) and writes once: capabilities union, scope takes the
more permissive side, the higher role label wins, latest login survives.

Merging happens in memory rather than in `ON CONFLICT` deliberately. A SQL union
would depend on write order and would accumulate across runs, never shedding a
capability removed at the source. As built, a re-run converges on whatever the
sources currently say.

`#12` then fixed a regression `#11` introduced: `roleRank("")` equals
`roleRank("member")`, so the strict `>` left 150 rows with an empty role. Found
by diffing production before and after, not by a test.

Net effect of the merge, against the pre-merge state: exactly **4 rows**, all in
the Kaufmann tenant, all gains, no losses. Final distribution is 120 member / 36
admin / 8 owner, and 33 unrestricted / 12 group-restricted / 119 restricted to
nothing.

**The lesson worth carrying:** every one of these was found by diffing production
against its sources, not by reading the code or trusting the summary counters.
The counters were themselves misleading — they counted rows *processed*, not
rows *written*, so 169 was reported where 164 existed. They now report merged
counts and an explicit `overlapping` figure.

### Still to do

- ~~Decide whether `/v1` enforces caller scope~~ — **done**, `#13` / `v0.1.2`.
  See "Caller scope" below.
- `ErrorHandler` logs every non-404 at error level, so ordinary 401s land in the
  error stream and will feed alerting once callers integrate.
- Nothing consumes `/v1/authz` yet — fleet-lite, kaufmann and b2b all still use
  their own edge checks. Cutover is the next real milestone.

## Done on 2026-08-06

### Conexo2's DIMO client id — CLEARED

`Conexo` and `Conexo2` in `kaufmann_oracle.tenants` shared client id
`0x6A1C063751415231C9A41C64aEEd8FD061bc9807`. Cleared on Conexo2 (`UPDATE 1`,
guarded so it aborted unless exactly one holder remained). Conexo keeps the id;
Conexo2's encrypted secret was left in place, inert without a client id.

Two corrections to what the previous note claimed:

- **Conexo2 was not memberless.** It had 0 vehicles but **1 member**,
  `0x998DD233Bb9729cB22cCf8351D9b5827843926C6` — who is already an admin on
  Conexo, so nobody lost reach. Clearing a client id does not touch membership
  anyway.
- **There is a second duplicate group**, and it is benign: `Hayati`,
  `Hayati Test` and `james test tenant` all carry `dimo_client_id = ''` (empty
  string, not NULL). The backfill already handles it on both sides — it skips
  empty ids in the duplicate check (`backfill.go`, the `seenClient` loop) and
  writes `NULLIF($2,'')`, so they land as NULL and cannot collide under the
  unique index. No action needed, but don't "fix" it into a real duplicate.

### "DIMO Build" vs "TEST" — a third duplicate, CLEARED

The first dry-run surfaced one the earlier survey missed, because it **crosses
databases** and so no single-database query would show it. kaufmann's
`DIMO Build` and fleet-lite's `TEST` both held
`0xE40AEc6f45e854b2E0cDa20624732F16AA029Ae7`, and both belong to the same wallet
`0xCAA591fA19a86762D1ed1B98b2057Ee233240b65` — one person's developer license
used to create a tenant in each system.

| | kaufmann `DIMO Build` | fleet-lite `TEST` |
|---|---|---|
| vehicles / groups | 0 / 0 | 6 / 2 |
| last activity | 2026-05-08 | login 2026-07-18, updated 2026-08-05 |

Cleared on kaufmann's side, mirroring the Conexo2 decision — the empty, stale
holder gives it up, and the side actually running vehicles keeps it. `TEST` now
migrates as a self-serve tenant holding the credential.

**The lesson for anyone auditing this again:** duplicate-credential checks must
run across both databases at once. Two more single-database sweeps would not
have found this one.

### A clean `backfill -dry-run` against prod — PASSED

Exit 0, after the above was cleared:

```
kaufmann_tenants=11  selfserve_fleetlite_tenants=4  dry_run=true
  "verification complete"
```

15 distinct tenants, which reconciles with the 16 counted across both systems
minus the single Kaufmann overlap. Every credential in scope decrypted cleanly —
the dry-run aborts before writing anything if even one does not.

Self-serve tenants are `0x0065fa40…`, `Fresh Coast Garage`, `My Test Fleet` and
`TEST`.

### The group-attestation republish — DONE and VERIFIED

R1 §5 specifies a `republish-group-attestations` CLI, but **it had never been
built**, so the rollout's step 4 had no tool to run. Written, reviewed and
shipped as `fleet-lite-app#100`, released `v0.6.8` (`29a4619`), then run as a
one-off Job.

Result: **287 published, 0 failed, 0 skipped**, and the published set is an
**exact match** for the 287 distinct `(tenant, vehicle)` pairs in
`vehicle_fleet_groups` — verified by diffing the job's per-vehicle log against
the database, not by comparing counts. 285 Kaufmann + 2 TEST.

That is §6's verification gate: every grouped vehicle now carries fleet-lite's
own assertion, so enforcing tenant-matching can no longer strip one.

**The first attempt failed completely — 0 of 287 — for the identity-api reason
in Traps below.** The Job initially copied the chart's `linkerd.io/inject: disabled`, hit
the identity-api 403, and died on `Unregistered redirect_uri` for every vehicle.
Nothing partial was published, so a retry was clean. Anyone writing another job
against this app needs to mesh it.

### The cronjob meshing bug — FIXED

`fleet-lite-app#101`, released `v0.6.9` (`3345b04`). The nightly
`import-group-attestations` had been green and idle for as long as the cronjobs
existed — unrelated to this programme, found while running the republish.

The chart set `linkerd.io/inject: disabled` for a real reason (an injected proxy
outlives the main container, so the job never completes), but **identity-api
authorizes on mesh identity and 403s unmeshed callers**. The developer-license
lookup failed, `DIMORedirectURI` was never resolved, and every developer-JWT
call died on `Unregistered redirect_uri` — logged at debug, exit 0 regardless.

The fix meshes the pods and shuts the proxy down explicitly. **The wrapper lives
in the template, not in values**, so a cronjob added later cannot forget it and
reintroduce the hang; values supplies `script` and the template owns the `sh`
wrapper and the exit code.

Verified in production by triggering the real cronjob: it now runs 3m45s instead
of 18s, with **0** license-resolution failures, **0** `Unregistered redirect_uri`,
and it actually cached the backlog — **22 license plates and 17 VINs**.

`import-group-attestations` also now tracks attempts and failures and exits
non-zero when *every* attempted vehicle fails. Nothing else distinguished
"converged, no changes" from "could not talk to anything" — both reported
`changed=0`. Deliberately not applied to `-vin-only`, where many vehicles
legitimately have no VIN VC and 100% "failure" is normal.

### 26 sync failures are now visible, and are benign

The new counters immediately surfaced something that was always happening and
never observable: `sync_attempted=564, sync_failed=26`.

All 26 are token-exchange 403s — `lacks permissions`, split 15 under Kaufmann's
license (`0xCa977Abb…`) and 11 under My Test Fleet's (`0xb92d74B4…`). These are
vehicles whose owners have not granted, or have revoked, the SACD privileges the
asset JWT needs. It is a data condition, not a fault, and 26 of 564 is ~4.6%.

**This does not contradict the republish succeeding on all 287.** They use
different credentials: the republish *publishes* with the tenant's developer JWT
and signing key, while the import *fetches* per-vehicle attestations with an
asset JWT obtained by token exchange, which is what the owner's grant gates.

If `sync_failed` ever jumps toward `sync_attempted`, that is the systemic case
the non-zero exit now catches.

Two things that shaped the republish command, both worth keeping if it is ever
rewritten:

- It **skips rather than asserts empty** when a vehicle's groups vanish between
  the query and the publish. Asserting "in no groups" on a stale read would
  retract a real membership — the exact failure the republish exists to prevent.
- Skips and failures make it **exit non-zero**. A partial republish is not a
  success, because each unpublished vehicle is one the foreign-drop then strips.

Two notes for the real run:

- The dry-run used a **throwaway** `TENANT_SECRET_ENC_KEY`, passed by env and
  deliberately not written to `settings.yaml`. Nothing is written on a dry-run,
  so it does not matter there — but the real run must use this service's actual
  production key, which does not exist yet because the service is not deployed.
  Do not reuse the throwaway.
- Wallet casing differs between the sources (`0xCAA591fA…` in kaufmann,
  `0xcaa591fa…` in fleet-lite). This is handled: `upsertUser` and both
  membership inserts all normalize through `common.HexToAddress(w).Hex()`, so
  one person yields one row. Worth re-checking if that code is ever touched.

### R1 PRs — merged and deployed

| PR | Merge sha | Released as |
|---|---|---|
| `fleet-lite-app#99` | `d77c9a8` | `v0.6.7` |
| `kaufmann-oracle#185` | `7e97f2e` | `v1.34.0` |

Verified before merging, against production:

- The uuid migration's claim that only `geofence_passes` and
  `geofence_scan_coverage` carry `tenant_id` without a foreign key is **exactly
  right** — 11 tables have the column, those 2 lack the FK, and both are updated
  explicitly. They held 26 and 160 Kaufmann rows that would otherwise have been
  silently orphaned.
- Preconditions held: old uuid present, target uuid free.
- Neither PR touches `charts/`, so the chart/image split below could not bite.
  `DROP_FOREIGN_TENANT_GROUPS` is a `bool` absent from the chart, so unset
  yields `false` — the safe default holds with no chart change.

Both migrations applied in the intended order (`20260805170000` re-key, then
`20260805180000` group ids; kaufmann `20260805180500`), both rollouts completed,
and both services logged **zero errors** in the ten minutes after.

## Current state

| | |
|---|---|
| Merged + deployed | `fleet-lite-app#97` (encryption fix), `#99` (R1) — `v0.6.7` |
| Merged + deployed | `#100` republish CLI `v0.6.8`, `#101` cronjob meshing `v0.6.9`, `#102` R1 step 6 |
| Merged + deployed | `kaufmann-oracle#185` (R1) — `v1.34.0` |
| Merged | design docs in all three repos |
| This repo | **DEPLOYED 2026-08-10** — authenticated `/v1` (`authz`, `resolve/client-id`), chart, CI, busybox image. Backfill written but never run for real |

`fleet-lite-app#98` and various dependabot PRs are unrelated.

**R1 is now complete through step 5.** The republish has run and been verified,
so `DROP_FOREIGN_TENANT_GROUPS` is unblocked — it is still `false`, and flipping
it is the next action. The trap below explains what it protects against; read it
before flipping.

## Caller scope on `/v1` — settled 2026-08-10

A caller may ask about a tenant when that tenant's **effective credential is the
caller's**: itself, a child that is parented to it *and* holds no license of its
own, or a tenant it holds a delegation over. Anything else is 403.

**The rule is deliberately not "caller must equal subject".** That version passes
today and breaks later, which is the worst failure mode available. The
architecture's resolution rule says a tenant's effective credential is its own if
it has one and otherwise its parent's — so an operator-managed customer holds no
license and is reached with its operator's. Equality works only while every
tenant is unparented, which is exactly the situation today, and fails on the
first operator-managed customer. Scope mirrors credential resolution so there is
one notion of whose license reaches which tenant rather than two that can
disagree.

**What this closed.** Of the eight credentials that can authenticate, four belong
to *customer* tenants whose developer licenses are held by outside companies. Any
of them could read any tenant's authorization data, Kaufmann's 149 memberships
included. No ingress made that unreachable, not unauthorized.

**`tenant_credentials.is_service_caller`** lifts the scope check for a shared
proxy that legitimately acts across tenants. It is `false` for all 15 credentials
and should stay that way unless a caller genuinely needs it; granting it is a row
change, so it is visible.

**b2b-fleet-mgr-app cannot authenticate to `/v1` at all**, and this is unrelated
to scope. Its `CLIENT_ID` `0x51dacC…` is the Login-with-DIMO *app* id, shared
with fleet-lite, and is not a registered tenant credential. Whoever integrates
b2b has to decide whether it gets its own registered credential marked
`is_service_caller`, or presents each operator's license per request via the
token minter. The first is simpler; the second is tighter.

## NEXT SESSION: wire `X-Tenancy-Key` into fleet-lite and b2b

Nothing calls `/v1` yet, so nothing is broken today. This is the prerequisite
for cutover. The service is deployed, backfilled and gated; the callers simply
do not send the header.

**Target:** `http://fleet-tenancy-api.prod.svc.cluster.local:8084` (no ingress,
by design). Every `/v1` request needs **both**:

| Header | Value |
|---|---|
| `X-Tenancy-Key` | the app's pre-shared key |
| `Authorization: Bearer …` | a DIMO developer-license JWT for the tenant it acts as |

Keys already exist in AWS Secrets Manager, verified identical to the entries in
the set the service verifies against:

| App | Secret |
|---|---|
| fleet-lite-app | `prod/fleet-lite-app/tenancy_api_key` |
| kaufmann-oracle | `prod/kaufmann-oracle/tenancy_api_key` |
| b2b | `prod/fleet_onboard/tenancy_api_key` (also at `prod/fleet-onboard-app/…`) |

### fleet-lite-app — the straightforward one

1. `charts/fleet-lite-app/templates/secret.yaml`: add a `remoteRef` for
   `{{ .Release.Namespace }}/fleet-lite-app/tenancy_api_key` →
   `secretKey: TENANCY_API_KEY`, and a `TENANCY_API_URL` in `values-prod.yaml`.
2. Add both to `config.Settings`, and send the header on every tenancy call.
3. It already mints per-tenant developer JWTs via
   `TenantService.GetDeveloperJWT(tenant)` — that is the `Authorization` header,
   and it makes fleet-lite's caller identity the *subject* tenant, so
   `CallerMayAccess` passes on the "self" branch with nothing further needed.

### b2b — has a real gap to close first

**b2b cannot authenticate to `/v1` at all yet**, and this is separate from the
key. Its `CLIENT_ID` `0x51dacC…` is the Login-with-DIMO *app* id, shared with
fleet-lite, and is **not** a registered `tenant_credentials.dimo_client_id`. The
PSK gets it past layer 1; layer 2 still rejects it.

Two ways to close it — this is a decision, not a detail:

- **Register a credential for b2b and set `is_service_caller = true`.** Simplest.
  b2b then reaches every tenant, which is what an operator console spanning many
  operators arguably needs. The flag exists for exactly this.
- **Have b2b present each operator's license per request**, obtained via the
  token minter (`GET /v1/tenants/{id}/dimo-token`, not built yet). Tighter, since
  scope is then enforced per request, but it needs the minter first and pushes
  work into b2b.

Note its chart uses `prod/fleet_onboard/<name>` with an **underscore**, unlike
the other repos — the key is stored under that convention so the new `remoteRef`
matches its neighbours.

### Verifying a caller once wired

From inside any pod, a request with a valid key but no JWT returns 401 from the
JWT layer and logs **no** `unrecognised trusted-caller key` warning. That absence
is how you tell layer 1 passed — the status code alone cannot, since all three
failure modes are 401.

## /v1 access control — three layers, settled 2026-08-10

| Layer | Question | Mechanism |
|---|---|---|
| 1 | is this a trusted application? | `X-Tenancy-Key` pre-shared key (`v0.1.3`) |
| 2 | which tenant is it acting as? | developer-license JWT, DIMO JWKS |
| 3 | may that tenant see the one asked about? | `CallerMayAccess` (`v0.1.2`) |

Keys live at `prod/fleet-tenancy-api/trusted_caller_keys` as `name:key,…`, with
each caller's own copy at `prod/<app>/tenancy_api_key`. One key per caller, so
one can be rotated without a coordinated redeploy of the others. The service
refuses to boot outside local without them — an unset value is not "no gate", it
is an open `/v1`.

**Callers still have to send the header.** fleet-lite, kaufmann and b2b each need
their key wired into config and sent as `X-Tenancy-Key`. Nothing calls `/v1` yet,
so nothing is broken by this; it is a prerequisite for cutover.

### Do not reach for linkerd policy here

An `AuthorizationPolicy` was attempted first and **took readiness probes down
across the whole `prod` namespace for about a minute**. The namespace-wide
`Server/http-port` has an **empty `podSelector`** — it selects every pod in the
namespace — and in linkerd, once any `HTTPRoute` is attached to a `Server`,
requests matching no route get a **404 from the proxy**. So a route scoped to
`/v1` made `/health` 404 for every service in `prod`, not just this one.

A server-side dry run passed: it validates schema and conflicts, not blast
radius. If mesh policy is ever revisited, this service must first be moved off
the shared Server by renaming its container port, and the policy attached to a
dedicated `Server` with a `podSelector` — not to an `HTTPRoute` on the shared one.

## Traps — things that will bite you

**`DROP_FOREIGN_TENANT_GROUPS` — the republish gate, now satisfied.** Enabling it
*before* fleet-lite republished its own group attestations would have deleted 370
of 378 group memberships: 0 of 287 grouped vehicles had ever been edited locally,
so fleet-lite had never published a group CE, the entire production group
structure was kaufmann's assertions, and `removalAllowed` was open on all 287.

The republish ran on 2026-08-06 and covers all 287, so the flag is now safe to
enable. **It is still `false`.** If you ever re-derive this situation — a new
tenant, a restored database, a fleet-lite install that has not republished — the
original rule applies again: republish first, verify the set matches, then flip.

**A meshed pod is required to reach identity-api.** It returns 403 to unmeshed
callers, which silently costs you `DIMORedirectURI` and turns every
developer-JWT call into `Unregistered redirect_uri`. The chart's cronjobs are
fixed (`v0.6.9`), but **any new Job or one-off pod must be meshed too**, and
must then shut the proxy down or it will never terminate:

```sh
wget -q -O- --post-data='' http://localhost:4191/shutdown >/dev/null 2>&1 || true
```

For chart cronjobs this is already handled — the template wraps `script`, so
adding one cannot reintroduce either half of the bug.

**Chart changes deploy independently of image changes.** ArgoCD auto-syncs from
`main` (`automated.enabled=true`) while prod images build on `v*` **tags**. That
gap caused a real ~10-minute production breakage during the #97 rollout: the
ExternalSecret landed before the image that could read it. If a change needs
config and code together, they must land together.

**The image-bump workflow rewrites `values-prod.yaml` and strips comments.**
Rationale belongs in docs, not inline there.

**Claude Code reads `CLAUDE.md`, not `AGENTS.md`.** All four repos now have a
one-line `@AGENTS.md` importer. kaufmann-oracle previously gitignored
`CLAUDE.md`, so its standards were never loading for anyone.

**Schema names vary.** fleet-lite and kaufmann put tables in a schema named
after the database — `fleets_lite` in prod but `fleet_lite_app` locally. Any
cross-database tool needs `search_path` in the DSN; kaufmann's is fixed and is
qualified inline.

**`IsLocal()` is `"local"` exactly.** Anything else, including an unset
`ENVIRONMENT`, fails closed. I briefly made empty count as local here, which
would have skipped the encryption-key guard in a real deployment — fixed, with
tests over the ambiguous cases.

## Production facts, measured

- 16 tenants: 11 in kaufmann, 5 in fleet-lite, **one overlap** ("Kaufmann").
  Both sides now agree on `7be1ab9e…`; fleet-lite's `9708b213…` was re-keyed by
  #99 on 2026-08-06, so this is no longer a pending divergence
- fleet-lite: 576 vehicles, 82 groups, 378 memberships, 10 members
- Kaufmann is 524 of those vehicles; the other four tenants hold 52 between
  them and are **real** — "My Test Fleet" logged in the same day with 40
  vehicles. They migrate as **self-serve** tenants (no parent, own credentials,
  implicit entitlements)
- All three encryption keys differ, so the backfill decrypts per source and
  re-encrypts. All 11 kaufmann credentials decrypt cleanly with the real key

## Next, in order

1. Flip `DROP_FOREIGN_TENANT_GROUPS` — the republish gate is satisfied, so this
   is the last step of R1
2. Decide whether `/v1` should enforce **caller == subject**. Authentication now
   identifies which tenant is calling, but no handler restricts a caller to
   asking about its own tenant, so any registered developer license can query
   `/v1/authz` for any tenant id. Tolerable only because the surface is
   cluster-internal with no ingress. Which way to go depends on which license
   the callers actually present: fleet-lite holds per-tenant credentials and
   could present the subject tenant's, which would make enforcement natural.
   `app.CallerFrom` already exposes the caller, so it is a handler change, not
   plumbing
3. The DIMO token minter (`GET /v1/tenants/{id}/dimo-token`), so credentials
   never leave this service
4. `/user/v1` management surface, then the b2b operator console

## Running things

```sh
make migrate && make run          # :3010, local brew postgres
go test ./...                     # skips cleanly if postgres is down
```

Local database `fleet_tenancy_api` is owned by `dimo`; create it with the
`jreate` superuser (the `dimo` role cannot `CREATE DATABASE`).

Backfill connection details come from the environment — see
`backfill.go`'s usage text. Always dry-run first, and force
`options='-c default_transaction_read_only=on'` on both source DSNs.

### Reaching production

The tunnel has now dropped mid-session twice, both times silently — a query just
gets `connection refused`. Bring it back with `ssh dimo-database-prod` (its
`~/.ssh/config` entry forwards **localhost:5430** to the prod RDS via
cloudflared) and re-check `lsof -nP -iTCP:5430 -sTCP:LISTEN` before trusting it.

Credentials come from the cluster, not from any local file:

```sh
kubectl get secret kaufmann-oracle-secret -n prod -o jsonpath='{.data.DB_USER}' | base64 -d
kubectl get secret fleet-lite-app-secret  -n prod -o jsonpath='{.data.DB_PASSWORD}' | base64 -d
```

Databases are `kaufmann_oracle` (schema `kaufmann_oracle`) and `fleets_lite`
(schema `fleets_lite`, so the DSN needs `search_path`).

**You often don't need the tunnel at all.** To confirm a migration applied, read
the init container instead — it is faster and needs no forwarding:

```sh
kubectl logs -n prod <pod> -c fleet-lite-app-migrate   # kaufmann's is named 'migrate'
```

## Outstanding decisions

- **Rotate the five DIMO developer licenses?** Re-encryption protects them going
  forward but does not undo prior exposure under the known key.
- **Delete the `AllowLegacyEmptyEncKey` shim** in fleet-lite — prod is through
  the re-encryption, so it is now dead weight that keeps the weak key readable.
- **Publish the design docs** — gitignored here pending the group-id and
  encryption fixes. The encryption one is done; R1 is not deployed.
