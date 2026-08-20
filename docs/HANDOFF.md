# Handoff — operator-managed multi-tenancy

Written 2026-08-06. Read this plus
`fleet-lite-app/docs/operator-tenancy/` (the full design set — it is gitignored
in this repo until the weaknesses it documents are fixed).

**Latest session handoff is at the end of this file** — *PICK UP HERE, 2026-08-20 04:30 UTC*.
Start there; this file is long and appends newest-last.

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

Written while the service was still undeployed; kept for the secret names and
the build mechanics, both of which still hold. (It *is* deployed now — see
above.) `buildpushdev` builds an image on every merge to main; a `v*` tag builds
the prod image and rewrites `values-prod.yaml`, exactly as in the sibling repos.

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
  error stream and will feed alerting once callers integrate. Note `tenancy-check`
  makes deliberate failing calls, so a run of it lands in that stream too.
- ~~Nothing consumes `/v1/authz` yet~~ — fleet-lite and kaufmann can now
  authenticate and are verified end to end by `tenancy-check`, but **no request
  path calls it**: all three apps still use their own edge checks. Cutover is
  the next real milestone.

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
| Merged + deployed | `fleet-lite-app#103` tenancy client — `v0.6.10`; `kaufmann-oracle#186` tenancy client — `v1.35.0`. Both verified in prod by `tenancy-check` |
| This repo | **DEPLOYED 2026-08-10** — authenticated `/v1` (`authz`, `resolve/client-id`), chart, CI, busybox image. Backfill run and verified: 15 tenants, 153 users, 164 memberships |

`fleet-lite-app#98` and various dependabot PRs are unrelated.

**R1 is complete.** The republish ran and was verified, and
`DROP_FOREIGN_TENANT_GROUPS` was flipped to `true` on 2026-08-06 (`#102`). The
trap below is kept because it applies again to any new tenant, restored
database, or fleet-lite install that has not republished.

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

## `X-Tenancy-Key` in the callers — DEPLOYED AND VERIFIED IN PROD 2026-08-11

Both callers that hold a developer license now authenticate to `/v1` in
production. Nothing calls it on a request path yet — that is cutover, and
deliberately still ahead. What exists is the plumbing plus a command that proves
it works, and it has now been run against the live service.

| | |
|---|---|
| `fleet-lite-app#103` | `v0.6.10`, image `b40b2ce` |
| `kaufmann-oracle#186` | `v1.35.0`, image `1.35.0` |

**Verified in prod, from inside each app's own pod:**

- fleet-lite `tenancy-check -all` → `checked=5, failed=0`
- kaufmann `tenancy-check -all -resolve` → `checked=4, failed=0`, and every
  resolve reported **same tenant id on both sides** — R1's uuid unification
  confirmed live, across both databases, rather than inferred from the migration
- A real membership resolves correctly: the TEST owner returns `via=direct`,
  `role=owner`, `permissions=["manage_members","manage_settings"]`,
  `scopeGroupIds=null`. Note `manage_members`, not `manage_admin_users` — the
  backfill's capability rename is what production actually serves
- Layer classification confirmed against the real service: a deliberately wrong
  key returned `layer=trusted-caller-key` / `invalid X-Tenancy-Key`, and an
  empty key was caught before the wire as `ErrTenancyNotConfigured`
- The service logged **exactly one** rejection across the whole exercise — the
  deliberate one. Both apps: zero errors, zero restarts after rollout

**Coverage caveat, worth resolving.** `-all` only runs tenants holding a usable
`dimo_client_id`, so it covered **4 of kaufmann's 11** operator tenants. Some of
the gap is known and benign (three carry `dimo_client_id = ''`, and Conexo2's
was deliberately cleared), but that does not obviously account for all seven, and
the prod DB tunnel was unavailable to confirm. **Check whether the remaining
kaufmann tenants have a credential row with a NULL client id** — such a row
holds an encrypted secret but cannot authenticate, which would mean those
tenants are only reachable via their operator's license. That is exactly the
effective-credential case the scope rule was built for, so it is a cutover
question, not a wiring bug.

**Target:** `http://fleet-tenancy-api.prod.svc.cluster.local:8084` (no ingress,
by design). Every `/v1` request needs **both**:

| Header | Value |
|---|---|
| `X-Tenancy-Key` | the app's pre-shared key |
| `Authorization: Bearer …` | a DIMO developer-license JWT for the tenant it acts as |

Keys exist in AWS Secrets Manager (re-verified 2026-08-11), matching the set the
service checks against:

| App | Secret | Wired |
|---|---|---|
| fleet-lite-app | `prod/fleet-lite-app/tenancy_api_key` | ✅ |
| kaufmann-oracle | `prod/kaufmann-oracle/tenancy_api_key` | ✅ |
| b2b | `prod/fleet_onboard/tenancy_api_key` (also `prod/fleet-onboard-app/…`) | deferred — see below |

### What landed in each repo

Identical shape in both: `TENANCY_API_URL` + `TENANCY_API_KEY` in
`config.Settings` and `settings.sample.yaml`, a `remoteRef` in the chart's
`secret.yaml`, the URL in chart values, and `internal/gateway/tenancy_api.go`.

`TenancyAPI` sends both headers and mints the JWT from the **subject tenant's
own** license via `DimoAuthProvider.GetDeveloperJWT`, so caller and subject are
the same tenant and `CallerMayAccess` passes on its self branch. kaufmann's
client also has `ResolveClientID`; fleet-lite has no use for it.

Neither app's boot depends on the new settings. That is a deliberate asymmetry
with `TENANT_SECRET_ENC_KEY`, which *must* fail boot: an unset encryption key
silently encrypts under a public constant, whereas an unset tenancy key costs a
401 on a call neither app yet makes. The client reports it as
`ErrTenancyNotConfigured` when something does call.

**The nil-vs-empty scope trap is carried into the clients.** `ScopeGroupIDs`
decodes `null` to nil (unrestricted) and `[]` to empty (sees nothing), and both
clients expose `Unrestricted()` so no call site is tempted to test `len()`.
Tests pin all four cases including an absent field. This is the same inversion
that silently granted 131 memberships the whole fleet during the backfill.

### Verifying a caller — `tenancy-check`

Each repo has a subcommand that runs the real thing:

```sh
kubectl exec -n prod deploy/fleet-lite-app -- \
  /fleet-lite-app tenancy-check -all
kubectl exec -n prod deploy/kaufmann-oracle -- \
  /kaufmann-oracle tenancy-check -all -resolve
```

It authenticates as each tenant holding a client id and asks `/v1/authz` about
the zero address — a wallet that is a member of nothing, so it needs no
knowledge of who belongs where and still exercises all three layers, answering
`via=none`. `-tenant-id` narrows it to one; kaufmann's `-resolve` additionally
dereferences the tenant's own client id and reports whether both sides agree on
the uuid.

**It names the layer that rejected the call.** This supersedes the old trick of
inferring layer 1 from the *absence* of an `unrecognised trusted-caller key`
warning in the service's logs. The service's error bodies are stable strings, so
`gateway.TenancyError` maps them:

| Layer reported | Meaning |
|---|---|
| `trusted-caller-key` | `X-Tenancy-Key` absent or unrecognised (layer 1) |
| `developer-license-jwt` | key fine; JWT bad, or its client id is registered to no tenant (layer 2) |
| `caller-scope` | both credentials fine; that tenant is out of scope (layer 3) |

Classification degrades to the bare status code if a message is ever reworded —
it is a diagnostic, so a wrong guess must never be worse than no guess.

Remember `tenancy-check` mints a developer JWT, so **it needs a meshed pod**:
identity-api 403s unmeshed callers, which costs `DIMORedirectURI` and turns
every mint into `Unregistered redirect_uri`. Running it in the deployment's own
pod is meshed already; a one-off pod is not, and must also shut its proxy down.

### b2b — deferred, and for a better reason than the one recorded before

The earlier note framed b2b's problem as an unregistered client id needing an
`is_service_caller` row. That understated it, and the fix was aimed at the wrong
repo.

**b2b has no developer-license JWT at all.** `api/internal/service/dimo_jwt.go`
(`DIMOJWTService`) has **zero call sites**, and `DIMO_CLIENT_ID` /
`DIMO_CLIENT_SECRET` are not in `values-prod.yaml`. It is also hand-rolled —
it generates a throwaway P-256 keypair per call and puts the public key in
`kid`, which the DIMO JWKS cannot verify. It is dead code, not a head start.
`CLIENT_ID 0x51dacC…` is the *frontend* Login-with-DIMO app id
(`controllers/settings.go:47` says so), and its private key belongs to
fleet-lite. b2b has never signed anything with it.

**And b2b does not need one, because it authorizes nothing.** It is a BFF proxy:
`oracleApp` forwards the *user's* JWT and `Tenant-Id` to kaufmann, and
kaufmann's `NewAccessMiddleware` → `Access.ResolveTenantAccess` does the actual
check. Every tenancy-shaped answer b2b serves already comes from kaufmann. Since
kaufmann is now wired, b2b inherits the tenancy service through it with no new
credential, no new license, and no `is_service_caller` row.

So the decision is **deferred, not open**: revisit it when b2b has something to
call directly, which means when `/user/v1` exists. At that point the options are
unchanged (its own license + service-caller flag, or the token minter), but with
one correction — "register a credential" also means deciding **which key b2b
signs with**, since it holds none.

### Which b2b → kaufmann hops can retire

Recorded so this isn't re-derived. Only one is movable today.

| b2b route | kaufmann | Can move to `/v1`? |
|---|---|---|
| `GET /oracle/:id/permissions` | `/v1/permissions` → `Access.GetUserPermissions` | **Yes** — same question as `/v1/authz?wallet=&tenant_id=`, same `[]string` shape |
| `GET /oracle/:id/tenants` | `/v1/tenants` | No — "list tenants for a wallet" does not exist in this service |
| `/tenant`, `/tenant/settings`, `/user-profiles`, `/accounts/admin` | various | No — management surface, i.e. `/user/v1` |

Two mismatches to handle when `/permissions` does move:

- `ResolveTenantAccess` requires `access_tenants.is_admin = true`. The shared
  model has **no equivalent** — it gates on `permissions` only. Decide which
  capability stands in for that gate rather than assuming one does.
- `manage_admin_users` was renamed `manage_members` and `view_all_fleets` was
  dropped by the backfill, so a naive string comparison against today's
  responses will disagree.

## CUTOVER COMPLETE — 2026-08-11

**fleet-tenancy-api is now the only source of "may this wallet act in this
tenant".** Both callers authorize from `/v1/authz` in production, the flag that
guarded the switch is gone, and so are their local authorization paths.

| | Cutover | Flag removed |
|---|---|---|
| fleet-lite-app | `#105` `v0.7.0` | `#107` `v0.7.1` |
| kaufmann-oracle | `#188` `v1.37.0` | `#190` `v1.38.0` |

Shipped flag-off, flipped by a chart-only change, verified, then the flag and
the local path were deleted together. Leaving both would have meant two answers
to one question — and a fallback consulted only during an incident is a
fallback nobody has verified.

**What the middlewares do now.** `NewTenantMiddleware` and
`NewAccessMiddleware` load the tenant (needed to authenticate *as* it), call
`/v1/authz`, and refuse anything that is not `via=direct` with membership. Both
fail **closed** on a tenancy outage — 503, not 403, because a dependency
failure is not an authorization decision. Answers are cached for the advertised
60s, so revocation is eventually consistent by that window; failures are never
cached, so a fixed key works immediately.

**fleet-lite additionally refuses `via=delegation`.** A delegation is an
operator's management right, never a fleet-lite session: operator staff are
b2b-only and there is no impersonation. That is why `Via` is checked and not
`Member` alone.

### The is_admin replacement — the one judgement call

kaufmann's middleware required `access_tenants.is_admin`, and the shared model
has **no equivalent**: it holds capabilities, and role is a display label that
must never be an authorization input.

It now gates on **membership plus at least one capability**. That was chosen by
measurement, not taste: it reproduces `is_admin` for **151 of 153** production
memberships. The alternative — gate on membership alone — would have admitted
**120** currently locked-out members to the operator API on cutover day.

Two memberships still disagree, both in the Kaufmann tenant:

| Wallet | Local | Effect |
|---|---|---|
| `0x27268E98DEc237158e0354a9bDEa5Cf9697152D5` | admin, no capabilities | **lost** access |
| `0xd3D2B67ea1F654A34209CD39c080BB425089809e` | member, has `onboard_vehicles`,`reports` | **gained** access |

**Both were reviewed and accepted as-is on 2026-08-11** — the lost access is
known and deliberate, not an oversight to correct. Do not "fix" it without
asking; someone already decided. If it is ever revisited, both are one edit to
that row's capabilities in `memberships`, and the prod DB tunnel is needed
(`ssh dimo-database-prod` was returning `Permission denied (publickey)`).

The capability rule is a proxy. The proper fix is per-endpoint capability checks
— `onboard_vehicles` for onboarding, `reports` for reports — which is the shape
the shared model is actually built for, and which would make the gate exact
rather than approximate.

### How the cutover was proven live

Neither app had user traffic, so "watch it work" was not available. Two probes
established it instead, using a developer JWT minted by `get-dimo-token`:

- **Known tenant, non-member wallet → 403.** The tenancy service answered.
- **Unknown tenant uuid → 500, where the local path gives 403.** The local path
  checks the wallet before ever loading the tenant; the tenancy path must load
  the tenant first to authenticate as it. That difference is only possible if
  the tenancy path is the one running.

That second probe was also a bug: an unknown `Tenant-Id` should never have been
a 500. Both apps now answer 403 — the caller named something that does not
exist, which is not a server fault. 403 rather than 404 so a caller cannot probe
which tenant ids are real.

### Still logging ordinary 403s at error level

fleet-lite's `ErrorHandler` has the same problem this service had before
`v0.1.4`: a non-member's 403 lands in the error stream. Now that 403 is the
normal answer for a non-member, that will feed error-rate alerting. kaufmann's
equivalent should be checked too. One-line fix, same shape as `#15` here.

## The operator console — MERGED AND DEPLOYED 2026-08-13

`b2b-fleet-mgr-app` is now the operator console, with this service serving the
surface behind it. All three stacks merged bottom-up on 2026-08-13 and deployed
in the load-bearing order (this service first, verified live, then the callers):

| Repo | PRs | Released as |
|---|---|---|
| this | #22 (`v0.2.0`), #23, #24 (re-opened as #26 after an auto-close), #25 docs | `v0.3.0`, image `912b04a` — rollout verified, both migrations applied (`20260812230000`) |
| kaufmann | #197 write-through, #198 customer proxy, #199 entitlement proxy | `v1.44.0` — rolled out, 0 restarts, only the known-benign SACD 403 in the error stream |
| b2b | #171 console+stub, #173 live proxy routes, #174 Vehicles tab | `v1.6.16` (image `0980003`) — rolled out |

**`tenancy-diff` re-run after the write-through deployed, as required.**
fleet-lite: `differ=0, missing_remote=0`; its `remote_extra` grew 4→9, all
"extra capabilities remotely: manage_vehicles" — that is #197's derived
capability existing only in the shared model, by design. kaufmann:
`differ=0` but **one `missing_remote`**: wallet
`0xDA13fE288658C594Eac74d41ce9752474d4AD146` in the Kaufmann tenant, local
`role=member` with no capabilities and no groups, remote no access. It was
granted locally in the window between cutover (2026-08-11) and the write-through
deploy — a live instance of the reported-success-conferred-nothing bug this
release closes. Under the current gates the row confers nothing anywhere, so
nobody is blocked who would otherwise act; left unfixed **deliberately, pending
review** rather than silently replayed. To fix: re-grant through the console
(now write-through), or PUT the membership to `/v1` with `role=member`,
`permissions=[]`, `scopeGroupIds=[]`.

Merge mechanics worth knowing for the next stack: all three repos squash-merge,
so a stacked PR must be re-based (`git rebase --onto origin/main <old-base>`)
and re-targeted after the one under it merges. Retarget **before** deleting the
merged branch — deleting first auto-closes the stacked PR, and a closed PR whose
head was then force-pushed cannot be reopened (that is how #24 became #26).
`gh pr edit --base` fails on a Projects-classic GraphQL deprecation; use
`gh api repos/<owner>/<repo>/pulls/<n> -X PATCH -f base=main`. And CI does not
re-run when a branch was pushed before its PR was retargeted to main — push an
empty commit to trigger it.

**Deploy order is load-bearing.** This service first, every time: an
unrecognised route is a 404, which kaufmann treats as a failure, so shipping a
caller ahead of its endpoints turns every affected write into a 502.

### Provisioning — DONE END TO END, 2026-08-13

The console is fully live; the stub is a demo mode, no longer the default.

| Repo | PRs | Released |
|---|---|---|
| this | #27 provisioning + minter, #28 provision answers with the member | `v0.4.1`, image `239a4b2`, rollout verified |
| kaufmann | #200 provision/PATCH/DELETE member proxies | `v1.45.0`, rolled out clean |
| b2b | #175 proxy routes + `STUB_BY_DEFAULT = false` | `v1.6.17` |

`POST /v1/tenants/{id}/members/provision` (accounts-api lookup-or-create by
email → user + membership, answering with the written member) and
`GET /v1/tenants/{id}/dimo-token` (minted developer JWT for the effective
credential). CredentialService is the only code that decrypts a key at runtime,
and the key goes exactly one place: a cached dimoauth AuthService, fingerprinted
so rotation rebuilds it. Effective-credential resolution uses CallerMayAccess's
exact expression so scope and minting cannot disagree. The new settings
(`DIMO_AUTH_URL` etc., chart values.yaml) are deliberately not boot-required —
authz stays available when the minter is unconfigured.

kaufmann's PATCH proxy does the read-modify-write (the tenancy PUT replaces
wholesale, deliberately), via `MergeMemberUpdate` — every field tri-state,
nil-vs-empty scope preserved. b2b's routes are plain proxy entries; no view
changes were needed, the frontend already spoke the live protocol.

**Nothing has exercised provisioning against real accounts-api yet** — that
needs a real email and creates a real DIMO account, so it was left for the
first deliberate console use rather than a synthetic prod probe. First-use
checklist: watch this service's logs for `member provisioned`, and if creation
fails check the effective credential has a `signer_address` (ErrNoSignerAddress
is a 409) and that the license is allowlisted with accounts-api (a lookup that
answers without a wallet is a 502 naming the client id).

The minter has no caller yet; it exists for b2b's deferred identity question
("present each operator's license per request via the token minter") and any
future caller that would otherwise want the key. Worth knowing before first
prod use: minting reaches identity-api, which 403s unmeshed callers — the
service pods are meshed, so this only bites a future unmeshed Job.

**Groups move here** — agreed, not started. See
[`plans/01-groups-into-tenancy.md`](plans/01-groups-into-tenancy.md). Both apps
keep near-identical `fleet_groups` tables and both synchronise the same
CloudEvent independently, which is the single cause behind six of the traps
below. It also makes `scope_group_ids` and `source_group_id` real references
instead of bare text pointing into databases this service cannot see.

**Agreed ordering (2026-08-13): provisioning first, then the groups phases.**
Provisioning finishes the console programme where it left off and touches
nothing the groups plan touches; starting the multi-repo groups rework with the
console one endpoint short would leave both half-done. After provisioning:
P1 (schema + read path here) and P2 (kaufmann's imei→token-id re-key, purely
internal to kaufmann) are independent and can proceed in parallel; P3's
backfill-and-diff, P4's write cutover and P5's table drops are strictly ordered
behind them. b2b's Vehicles-tab drift computation (#174) is deleted in P4's
wake, not before — it is the stopgap the plan replaces.

**P1 — DONE, deployed 2026-08-13.** #29, `v0.5.0`. `fleet_groups` +
`vehicle_fleet_groups` (keyed by vehicle_token_id, membership FK carries
tenant_id so a row cannot cross tenants) and the full CRUD + membership
surface under `/v1/tenants/{id}/groups`, no caller. Migration `20260813120000`
verified applied in prod. Ids are the R1 `<tenant-uuid>_<slug>` convention,
minted by a slugify matching fleet-lite's byte for byte; rename keeps the id.
The name-uniqueness rule is exact-case per tenant, deliberately matching what
the sources enforce so P3's backfill cannot be refused rows.

**P2 — DONE, deployed 2026-08-13.** kaufmann #201, `v1.46.0`, migration
`20260813123000` applied in prod. `vin_fleet_groups` keys on
`vehicle_token_id`; rows whose vin had no token id (unminted, unreachable
through the API) were dropped by the migration. The RAISE NOTICE with the
dropped count was **not captured** — goose does not surface NOTICEs — but the
loss is bounded by construction (only NULL-token rows, each API-invisible),
and two prod dry-runs verified the re-keyed paths end to end:
`import-group-attestations -dry-run` → `checked=462, changed=0` (the table
agrees with published foreign attestations exactly), and
`resync-group-attestations -dry-run -skip-empty` → `total=462,
skipped_empty=127`, exercising the rewritten LoadVehicleGroups per vehicle
with zero errors. 335 vehicles sit in at least one group post-re-key.
fleet-lite's 06:00 UTC warm import is the next natural cross-check. The CE
wire format is untouched; ADR 0001's "storage stays IMEI-keyed" is marked
superseded.

**P3 — BUILT, PRs open, not yet deployed.** Three PRs, one per repo, written
2026-08-13: this repo #30 (`backfill-groups` + `GET
/v1/tenants/{id}/vehicle-groups`), fleet-lite #112 and kaufmann #202 (both:
`GROUPS_FROM_TENANCY` flag flipping the display reads to the new endpoint, plus
a `groups-diff` command shaped like `tenancy-diff`). **Deploy order is the
usual one: this service first** — both callers' diffs and flagged reads 404
into failure against a service without the endpoint. Both flags ship `'false'`.

What the backfill does and why: metadata from the newer side (the #111 rule),
memberships unioned, and the write **replaces the tables wholesale** so a
re-run converges on what the sources currently say — which is also the
recovery for any local group write made during the P3 window, since writes
stay local until P4. `groups-diff` is asymmetric exactly like `tenancy-diff`:
`remote-extra` is the union working (the other source asserted it),
`differ`/`missing-remote` fail the run.

A `backfill-groups -dry-run` against prod (2026-08-13, through the tunnel) was
already clean: 87 merged groups (81 kaufmann + 84 fleet-lite, 78 overlapping),
567 merged memberships (535 + 552, 520 overlapping), **zero metadata
disagreements**, no name collisions, all tenants present, and **zero dangling
`scope_group_ids` / `source_group_id` references** — so the deferred FK
migration is unblocked once the backfill has run for real.

Deliberately not flipped by the flags (P4/P5 work, listed so nobody thinks it
was missed): all group writes, fleet-lite's `LoadVehicleGroups` /
`VehicleInGroups` / `AccessibleTokenIDs` scope SQL and geofence/invitation
validation, kaufmann's attestation worker reads, `GetVehiclesOnboarded` joins
and report filters. The attestation publishers must keep describing what was
actually written locally.

After deploy, the sequence is: run `backfill-groups` (dry-run, then real), run
both apps' `groups-diff`, flip `GROUPS_FROM_TENANCY` per app by chart-only
change, re-run `groups-diff` over a sustained window — exit is zero
differences. kaufmann's diff names its uncredentialed-tenant coverage gap out
loud; those tenants' groups are unverifiable through `/v1` until the effective-
credential question is settled. Remember the plan's risk note: the backfill
touches the structure that already caused the 370-of-378 near-miss — diff
before enforcing, always.

**P4 — BUILT, PRs open, stacked on the P3 PRs.** Written 2026-08-13: this repo
#31 (on #30), fleet-lite #113 (on #112), kaufmann #203 (on #202). All three
repos squash-merge, so remember the stacked-PR mechanics recorded above:
retarget before deleting the merged branch, rebase `--onto`, empty commit if
CI does not re-run.

What P4 is, in one paragraph each side:

- **This repo becomes the single publisher.** `publish-group-attestations`
  (a chart CronJob every 10 minutes, meshed with the proxy-shutdown wrapper —
  the template owns it) scans the group tables against a new
  `vehicle_group_attestation_state` table (migration `20260813150000`) and
  publishes `dimo.document.vehicle.groups` for every vehicle whose document
  digest changed. Scan-based deliberately — kaufmann's per-write queue is what
  once coalesced a rename into completed jobs (#192). The wire contract is
  unchanged (same type, subject DID, payload, ERC-191 signature over the data
  bytes, signed with the tenant's EFFECTIVE license via CredentialService — so
  operator-managed customers publish under their operator's license, which
  neither source app could do). Only `producer` changed: "fleet-tenancy-api",
  deliberately distinct from both retired producers. New settings
  ATTEST_API_URL and CHAIN_ID.

- **Both callers write through.** Every group mutation goes to this service
  FIRST, then mirrors into the local tables — remote-first so a half-failure
  leaves the authority right and the mirror behind (a retry or mirror-groups
  converges), never a reported success that did not reach the owner. The
  local tables stay as mirrors ONLY for the scope-filtering SQL joins
  (vehicle listing, reports, geofences) until P5 rewrites those; each app
  gained a `mirror-groups` daily cron that reconverges its mirror from
  `GET /v1/tenants/{id}/vehicle-groups` — replacing the deleted CE import by
  pulling from the single owner instead of reconciling a peer's stream.

- **Deleted:** fleet-lite's `import_group_attestations`,
  `republish_group_attestations`, `group_sync.go`, the controller republish
  fan-out, `AttestVehicleGroups`, and `DROP_FOREIGN_TENANT_GROUPS` (gone —
  the trap above about its republish gate is now historical); kaufmann's
  `groupattest` River worker, `vehicle_groups_attest.go`,
  `import-group-attestations` and `resync-group-attestations`. Roughly 1,900
  lines of convergence machinery across the two apps.

**P4 deploy order and gates.** P3's gate comes first and is unchanged
(backfill, diff, flag). Then: this service (P3+P4 together is fine — the
publisher reads tables the backfill fills; before the backfill it publishes
nothing), verify a `publish-group-attestations -dry-run` in prod, then the
callers. The publisher's first real run publishes every grouped vehicle
(~370) under the new producer — one-time, expected. After the callers
deploy, GROUPS_FROM_TENANCY should be flipped ON promptly: the CE import
that used to reconcile cross-app writes for the shared Kaufmann tenant is
gone, so flag-off display reads of that tenant go stale between mirror-groups
runs.

### P3 + P4 ROLLED OUT AND VERIFIED — 2026-08-13

All six PRs merged and deployed (`v0.5.2` here, fleet-lite `v0.7.6`, kaufmann
`v1.48.0`), migrations applied through `20260813150000`, and the whole gate
sequence ran:

| Step | Result |
|---|---|
| `backfill-groups -dry-run` | 87 groups (81 kaufmann + 84 fleet-lite, 78 overlapping), 567 memberships, no metadata disagreements, no name collisions, **no dangling refs** |
| `backfill-groups` (real) | **87 groups, 567 memberships written** |
| fleet-lite `groups-diff` | 87 groups, 78 agree, 9 remote-extra, **differ=0, missing_remote=0** |
| kaufmann `groups-diff` | 85 groups, 65 agree, 20 remote-extra, **differ=0, missing_remote=0** |
| publisher, first populated run | `checked=357, failed=0` — 357 CEs, one per grouped vehicle |
| publisher, next run | `checked=357, unchanged=357, published=0` — converged |

Every `remote-extra` is in the shared Kaufmann tenant and is the union
working as designed. `skipped_tenants=0`, which answers part of the kaufmann
coverage caveat from the other direction: every tenant *holding grouped
vehicles* resolves a usable effective credential, because the parent
fallback covers the ones with no license of their own.

**The backfill was run as an in-cluster Job, not through the tunnel** — it
only talks to Postgres, so it is deliberately *unmeshed* (no proxy to
outlive the container, no shutdown wrapper needed). The manifest is worth
recreating rather than remembering: it takes `envFrom` this service's config
and secret, plus `DB_HOST`/`USER`/`PASSWORD` from the other two apps'
secrets via `secretKeyRef`, and composes the two `BACKFILL_*_DSN` strings in
the container command with `default_transaction_read_only=on` forced on both
sources.

**The order mattered more than expected.** P4 deployed with the tenancy group
tables still empty, and P4 makes every group write go to tenancy first — so
between the caller deploy and the backfill, any edit to an *existing* group
would have 404'd. Nothing was attempted in that window, but the lesson
generalises: **the backfill is not a follow-up to a write cutover, it is a
precondition for it.** If this is ever re-derived (a new environment, a
restored database), run the backfill before the callers ship, not after.

**The flag flips are merged and live** — fleet-lite #114, kaufmann #204, both
chart-only, both pods rolled by the chart's `checksum/config` annotation and
both logging `GROUPS_FROM_TENANCY is on` at boot. **Reads now come from this
service in production.**

Proven rather than assumed, on both sides, using the same trick each way:
ask an app for a group its own database has never held.

- **kaufmann** — `/api/v1/fleet-groups` with a real developer JWT returns
  **85** groups including `…_test-luis-saez`, `…_verify-qa`, `…_yy` and
  `…_3Ervzaaxvr…`, four groups **fleet-lite** created. Before the flip it
  could only have returned its local 81.
- **fleet-lite** — signed into `fleets.dimo.co` as the Kaufmann tenant, the
  Groups screen reports **85 groups** (its own table holds fewer) and
  `GET /fleet/groups` answers 200. Searching surfaces `DEMOS MAXUS / MD…`,
  `Eduardo Rodríguez /…` and `Felipe Briceño / DEA…` — precisely the three
  its own `groups-diff` had listed as *"group only in tenancy"*, i.e. groups
  **kaufmann** created that fleet-lite's table has never held.

Both apps now display each other's groups, which neither could do before,
and which no local mirror can explain. The per-vehicle read
(`VehicleGroupsMapView`) is best-effort by design — it logs and degrades
rather than failing the page — so it was checked in the logs instead: zero
group-related errors across the session. fleet-lite's only errors in that
window were 12 pre-existing SACD `lacks permissions` token-exchange 403s,
the same benign condition recorded above.

**One bug fell out of the first publisher run** — `published=714` against
`checked=357`, exactly double, because the plan and the publish loop both
incremented the same counter. Nothing published twice (the next run's
`unchanged=357` proves it); only the accounting lied, and it would have lied
worst on a partly-failed run by inflating the success count. Fixed in `#32`,
released `v0.5.3` (image `a1a53ce`), which also makes the command fail when
the counts do not reconcile. Verified live: the run after the rollout reports
`checked=357, planned=0, unchanged=357` with the new fields and no imbalance.
Third time this programme has been bitten by a counter that measured the
wrong thing — after the backfill's rows-processed-not-written and the sync's
`changed=0`-means-two-things. **Counters here are load-bearing evidence;
give them an invariant that can fail.**

Post-deploy state, 2026-08-13 19:41 UTC: both `groups-diff`s still
`differ=0, missing_remote=0`; zero errors in this service and fleet-lite;
kaufmann's only two error lines are the pre-existing benign SACD
token-exchange 403 and a ruptela ingest packet. Zero restarts anywhere.

### P5 — what remains

- Move the scope-filtering SQL and the vehicle/report group joins onto
  tenancy-backed token-id sets. This is the real work: fleet-lite's
  `AccessibleTokenIDs` / `allowedGroupsFilter` (`vehicle.go`) and
  `geofence.go`'s `EffectiveTokenIDs`, kaufmann's `GetVehiclesOnboarded`
  group joins and the reports filters.
- Then drop the local tables, the mirrors, the `mirror-groups` crons and the
  `GROUPS_FROM_TENANCY` flags.
- Add the deferred FKs from `scope_group_ids` / `source_group_id`. Note they
  are **arrays**, so this is a trigger or a check constraint, not a plain FK
  — decide which when the rows are stable.
- Delete `SyncVehicleGroups` together with its frontend caller, and
  fleet-lite's now-unused `vehicles.groups_updated_at` /
  `last_group_sync_at`, plus the orphaned `LicensePlateSyncService.SyncVINOnly`.
- Docs left stale on purpose until the code moves: `docs/GROUP_SYNC.md`,
  `docs/FLEET_GROUPS_PLAN.md` in fleet-lite.

### Decisions taken while building, worth not re-litigating

- **Suspension now removes access** (#23). `tenantStatus` had ridden along on
  every authz response since the first release and no caller ever read it, so
  suspending a tenant was decorative. Enforced in `Authorize`, which makes it
  true everywhere with no caller change. Delegated management is refused too —
  otherwise suspending a customer locks its operator out of the screen used to
  un-suspend it. Safe to deploy because the only `INSERT INTO tenants` is the
  backfill, which never sets `status`.
- **Exclusivity is enforced twice** (#24). The service applies the rule as
  specified, per operator, with the holder's name for the console; a partial
  unique index enforces the stricter one-active-holder-per-vehicle as a
  backstop, because the service check reads before it writes and the failure
  mode of that race is one customer seeing another's vehicle.
- **`manage_vehicles` is derived from `is_admin`** on every kaufmann membership
  write (#197). It exists only in the shared model, and since a write replaces
  the capability set, sending only the translated list would have stripped it
  from every admin the console touched.
- **Drift is computed in b2b, not served** (#174). It needs the oracle's current
  group membership and this service's entitlements, and only the console sees
  both. The spec's `GET /vehicles/drift` was dropped rather than built. This
  changes if groups move here.
- **New routes gate on `manage_members`**, not the access middleware's "at least
  one capability" approximation, which exists to reproduce the old `is_admin`
  on routes that already existed.

### Local development note

`b2b-fleet-mgr-app`'s `api/settings.yaml` had `MONITORING_PORT: 3010`, which
collides with this service's local `make run`. Changed to 3011 locally; that
file is gitignored. `make dev` in b2b also leaked its backend on Ctrl-C — `go
run` was in the foreground and the compiled binary outlived the wrapper, keeping
port 3007 — fixed in b2b #171.

## The write half was never cut over — FIXED 2026-08-12

Cutover moved the **read**. Every membership **write** stayed in kaufmann's
`access_tenants`, which nothing consults any more. So `POST /v1/accounts/admin/grant`
wrote a row into a table that no longer decides anything, `NewAccessMiddleware`
asked this service whether that wallet could act, and the answer was no. **The
grant reported success and conferred nothing.** Nothing reconciles the two on a
schedule — `backfill` is a one-off command and the chart ships no cronjob — so
the divergence only ever grew.

The reconciliation below was clean because it ran on 2026-08-11, *before* any
post-cutover grant. It is not evidence the two stay converged.

**What landed.** `PUT` and `DELETE /v1/tenants/{id}/members/{wallet}`, behind the
same three layers as the rest of `/v1`, and kaufmann now calls them from
`GrantAdminAccess`, `UpdateAdminAccess`, `RevokeAdminAccess` and the customer-user
path in `CreateAccount`.

Four things worth knowing about how it is wired:

- **A failed tenancy write fails the request** (502), rather than logging and
  returning 200. Returning success is what produced the original problem.
- **Grants write locally first, revocations write here first.** Either order can
  fail half-way; these are the orders where a half-failure leaves the person
  with *less* access than intended. Both writes are idempotent, so a retry
  converges.
- **Revoking empties the membership rather than deleting it.** Deleting would
  also end that person's fleet-lite access, and the endpoint revokes admin
  rights in the operator console, not the person. A membership with no
  capabilities already fails kaufmann's "member plus at least one capability"
  gate — exactly what `is_admin = false` used to mean.
- **`scopeGroupIds` is `json.RawMessage` end to end**, and an absent field is a
  400. As a `[]string`, "omitted" and "explicitly unrestricted" both arrive as
  nil, so a caller that forgot the field would grant the whole fleet. That is
  the inversion that handed 131 memberships a 524-vehicle fleet during the
  backfill; it is now the one input the service refuses to guess at.

**DEPLOY THIS SERVICE FIRST.** kaufmann now fails a grant when the tenancy write
fails, and an unrecognised route is a 404, which is a failure. So shipping
kaufmann against a tenancy service that lacks these endpoints turns **every**
grant, update and revoke into a 502 until the other side catches up. Same shape
as the chart-before-image breakage during the `#97` rollout, and the same rule:
if a change needs both sides, land the dependency first.

**A second bug fell out of this.** `RevokeAdminAccess` only ever set
`is_admin = false` and left `permissions` intact. That was harmless while the
gate *was* `is_admin` — but since cutover the gate is "holds at least one
capability", so **a revoked admin kept getting in**, and `Access.CheckPermission`
kept answering yes for every capability they held. Revocation now clears
`permissions` too. This is a behaviour change to an existing endpoint, made
deliberately.

### Are kaufmann's old tenancy tables gone? No, and they can't be yet

Asked and answered on 2026-08-12. `tenants`, `access_tenants`,
`access_fleet_groups` and `user_profiles` are all still live, and dropping any of
them today would break the oracle:

| Table | Still doing what |
|---|---|
| `access_tenants` | Backs `Access.CheckPermission` — the per-endpoint capability checks in `vehicle.go`, `fleet_vehicles.go`, `account.go` and `reports/worker.go`. Also the `/accounts/admin*` read path |
| `access_fleet_groups` | Backs `Access.GetUserFleetAccess`, the group filter on fleet vehicle listing and reports |
| `user_profiles` | Live feature (`/v1/user-profiles`), plus the government-id columns that stay here by design |
| `tenants` | Keeps oracle-specific columns (Kore, command password, signer) and is loaded to authenticate *as* the tenant |

Only the **gate** moved to `/v1/authz`. The capability checks are still local,
which is the "move kaufmann off the capability proxy" item below — that is the
work that frees `access_tenants`, and `access_fleet_groups` goes with it. Note
that table is keyed by wallet alone with no tenant column, so a member's group
scope is currently shared across every tenant they belong to; the shared model
fixes that by putting scope on the membership.

## Reconciliation — RUN 2026-08-11, and it is clean

**163 memberships compared against `/v1/authz`, zero narrowings.** This is the
evidence cutover was waiting on: the backfilled data agrees with both source
systems everywhere it matters.

| | fleet-lite | kaufmann |
|---|---|---|
| checked | 10 | 153 |
| agree | 6 | 149 |
| remote-extra | 4 | 4 |
| **differ** | **0** | **0** |
| **missing-remote** | **0** | **0** |

`differ` and `missing-remote` are the two that matter — remote granting *less*
than local, or no access at all. Both are zero, so **nobody loses access at
cutover**.

All 8 `remote-extra` rows are in the **Kaufmann tenant**, the one that exists in
both source systems, and every one is explained by the merge:

- fleet-lite's owners gain `onboard_vehicles` and `reports` from kaufmann's side
- kaufmann's admins gain `manage_settings` from fleet-lite's side (and read as
  `owner` rather than `admin`, the higher role label winning)
- one wallet gains the group `…_gerencia-primera-linea`, which only fleet-lite
  asserted

That is the merge working as designed, not drift. It is also why the comparison
is deliberately asymmetric: had `remote-extra` been treated as failure, the
overlap would have produced 8 false failures and the command would have been
ignored — which is how a real narrowing gets missed.

### Why this was a batch job and not a shadow read

The conventional approach is to call `/v1/authz` alongside the local check in
the request path and log disagreements until confidence accumulates. **That
produces nothing here**, because the product has no users yet: shadow logging
only covers paths traffic actually takes. The batch walk covered every row
immediately instead. If a shadow pass is ever wanted, it is worth remembering it
measures traffic, not data.

Re-run both after any membership change, and before cutting a call site over:

```sh
kubectl exec -n prod deploy/fleet-lite-app  -- /fleet-lite-app  tenancy-diff
kubectl exec -n prod deploy/kaufmann-oracle -- /kaufmann-oracle tenancy-diff
```

Both exit non-zero on any `differ` or `missing-remote`.

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

**fleet-lite and kaufmann now send the header** (2026-08-11) — see the wiring
section above. b2b does not and does not need to; it reaches tenancy answers
through kaufmann. Nothing calls `/v1` on a request path yet, so nothing is
broken by any of this; it is the prerequisite for cutover.

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

**Group metadata rides on every member vehicle's attestation, so vehicles
disagree.** A group's name and colour are copied onto each member's
`dimo.document.vehicle.groups` CloudEvent. Members attested before a rename and
after it therefore carry different names for the same group, and there is no
single authoritative copy to read.

That makes any "adopt the incoming name" rule ordering-dependent unless it
compares timestamps. A first attempt at fixing stale group names did exactly
that and rewrote one production group's name 40 times in a single import,
alternating between the two, with the surviving value decided by whichever
vehicle happened to be processed last — one group ended up on the wrong name.
The rule is now: adopt metadata only from an attestation **newer than the group
row's `updated_at`** (`fleet-lite-app#111`). Anything that reads group metadata
from attestations needs the same guard.

**A rename can silently publish nothing at all.** kaufmann enqueues a
re-publish per member vehicle on a group edit, but the job was unique
`ByArgs` with River's default states — which include `completed`. A group
renamed shortly after its vehicles were added matched the just-finished jobs
and every insert was discarded, so the rename never reached the wire and no
downstream reconcile could ever correct it. Fixed in `kaufmann-oracle#192` by
restricting the unique states to in-flight ones. If you add a River job whose
purpose is "state changed, republish", check its unique states first: coalescing
against finished work is data loss, not deduplication.

**Re-running a sync cannot fix a stale attestation.** Both bugs above presented
as "the group did not sync". It had: memberships were correct (164 of 165, the
gap being a vehicle without SACD grants) and the reconcile honestly reported
`changed=0`. Only the name was wrong, and no number of re-runs would have
changed it. Diff the two databases before re-running a job.


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

## Session handoff, 2026-08-14 ~03:00 UTC — SUPERSEDED, kept for the record

**P5a is complete, deployed to both apps, and its exit criterion is met.**
Everything the previous handoff listed as open or unverified is resolved.
Nothing is half-applied and no PR is waiting for review.

### Shipped tonight — five prod releases, each verified in the cluster

| Release | Contents |
|---|---|
| fleet-lite **v0.8.0** | P5a (#115), the group **Sync** button removed (#116), the sync route dropped (#118), #114 |
| fleet-lite **v0.8.1** | group counts stale after a write + VIN/plate in the vehicle picker (#119), members-first sort (#120) |
| kaufmann **#205** | P5a — scope rewrite plus the scope *source* fix |
| kaufmann **#206** | device-type filter on the pending-onboard list |
| b2b **v1.8.0** | device-type filter UI (#177), onboarding list refresh after minting (#178) |

Every rollout was checked for image, pod readiness, restart count and ArgoCD
health. **Zero restarts across all five.** Both apps log `GROUPS_FROM_TENANCY
is on` at startup.

### P5a's exit criterion — MET, measured 2026-08-14 03:02 UTC

`groups-diff` re-run in both apps *after* P5a shipped. The previous `differ=0`
predated the read-path rewrite and no longer proved anything:

| App | tenants | groups | agree | remote-extra | **differ** | **missing-remote** |
|---|---|---|---|---|---|---|
| fleet-lite | 5 | 87 | 82 | 5 | **0** | **0** |
| kaufmann | 4 | 85 | 66 | 19 | **0** | **0** |

`remote-extra` is the expected direction (tenancy holds the union of both
sources) and disappears with the tables. The two failure verdicts are zero.
The other half of the exit — "nothing but the mirror tooling reads the local
tables" — is in code: kaufmann's `FleetGroupsView` records that every
request-path reader of `vin_fleet_groups` now resolves through `GroupIndex`.

### The P4 write path is verified — this closes the last handoff's open item

A real group write reached this service at **01:06:32 UTC**:
`vehicles added to fleet group`, `group_id …_test-luis-saez`, `token_ids 1`.
The publisher's repaired counters got their first genuine run on the next
tick: `checked 357→358, planned 1, published 1, unchanged 357, failed 0`.

The reason it had never happened is now removed: **SINCRONIZAR was a no-op
that reported success**, summing `added + removed` from an endpoint that
hardcodes both to 0. Any write test through it would have looked successful
having written nothing. Button and endpoint are both gone (#116, #118) — the
endpoint was on *P5b's* drop list, so that item is already done.

### What is left of P5 — P5b only, still correctly blocked

1. **`access_fleet_groups`** — `ON DELETE CASCADE` FK to kaufmann's
   `fleet_groups`, so that table cannot be dropped while it exists.
   `GetUserFleetAccess` is now called *only* from the member-management
   surface (`account.go` `:345`, `:578–592`, `:686`, `:736`), which is exactly
   where P5a was meant to leave it. **Retiring that surface is the next real
   piece of work**, and it is not gated on time.
2. **The soak** — reads flipped 2026-08-13 and P5a shipped tonight, so the
   clock has barely started. The tables are the revert path until it elapses.
3. **Then the drop**: `fleet_groups` / `vehicle_fleet_groups` /
   `vin_fleet_groups`, the `mirror-groups` crons and commands, `groups-diff`,
   the `GROUPS_FROM_TENANCY` flags and their local branches, fleet-lite's dead
   `vehicles.groups_updated_at` / `last_group_sync_at`, and the stale
   `GROUP_SYNC.md` / `FLEET_GROUPS_PLAN.md` (all still present). Plus the
   deferred references from `scope_group_ids` / `source_group_id` — both are
   **arrays**, so a trigger or check constraint, not a plain FK. Not started.

### kaufmann's "merge is a release" hazard is fixed

Both workflows used to write `charts/kaufmann-oracle/values.yaml`, which the
prod ArgoCD app tracked at `HEAD` with automated sync — every merge to `main`
was an unreviewed prod release, and the only way to stage a risky change was
to not merge it. That is why #205 sat for a day.

`values-prod.yaml` now exists (`cp` of `values.yaml`, so no transcription
drift), `buildpushtagged` writes it, and the Argo app was patched to
`valueFiles: ["values-prod.yaml"]`. **Verified against the live cluster**: a
merge bumped `values.yaml` to `f09dd46`, Argo synced past that commit
(`rev=c39e89f`), and prod stayed on `c145ecd` with the same pods. Releasing
kaufmann now means cutting a `v*` tag, as in the other two repos.

A CI gate (`values.yaml and values-prod.yaml agree`) diffs the two files with
the `image.tag` line stripped, and fails if there is ever not exactly one such
line rather than silently comparing less than it claims to.

### Do these first

1. **Read `mirror-groups`' output** (06:30 UTC kaufmann, 06:15 fleet-lite) —
   not just its exit code. It is the first unattended run, and the first with
   P5a live on *both* sides.
2. **The first `v*` release of kaufmann through the new tagged path.** That
   workflow now writes a file it has never written before. One line, and CI
   lints the file, but it is unexercised — watch it rather than assume it,
   exactly as the P4 write path taught.
3. **Start retiring `access_fleet_groups`** if you want P5b to move; it is the
   only part of the blocker that is not just waiting.

### Known-open, none blocking

- **`TenantService.UpdateMemberAccess` (fleet-lite) validates group ids not at
  all**, so an owner can scope a member to nonexistent groups — asymmetric
  with the invite path, which validates. Flagged across several sessions now
  and still outside every phase's scope. Worth a ticket rather than another
  mention.
- The values-parity CI gate exists **only in kaufmann**. fleet-lite and b2b
  have the same two-file structure and no check on it.
- `groups-diff` has no cron in either app; it is manual, which is why its
  result went stale across P5a. Consider scheduling it for the soak.

### Bugs found while doing P5a — all fixed unless noted

- **fleet-lite's geofence screens leaked tenant-wide vehicle counts to
  limited members.** `GetGeofences` / `GetGeofence` left the unrestricted
  count in place when the per-geofence recount errored (`if err != nil {
  continue }`). Fixed in #115. Same family as everything else here: an error
  quietly degrading to "unrestricted".
- **Group counts went stale after a write** because `invalidateGroupIndex` is
  per-process and fleet-lite runs two replicas — a write served by one pod
  left the other's index stale for up to the 60s TTL. It presented as "the
  frontend needs to refresh"; the frontend was already refreshing. The
  management reads now read through and repair the cache (#119).

  **kaufmann does not have this bug**, checked rather than assumed: it has the
  same per-process `indexCache` and also runs two replicas, but its management
  reads (`ListGroups` / `GetGroup`) go through `listRemote`, which calls
  tenancy directly and never consults the index. Its scope filter uses the
  cache; its screens do not. If anyone later "optimises" `listRemote` onto
  `GroupIndex`, that introduces fleet-lite's bug — the fix there is
  `groupIndexFresh`, which reads through *and repairs* the entry rather than
  bypassing it.
- `AddVehicle` read "was this new?" *after* the remote write — right only
  because the mirror lagged. Fixed in #115.
- `c.Status().JSON()` returns nil, so returning it as a helper's error yields
  a **200** carrying a 502 body unless the caller compensates. Fixed in #205;
  the same shape exists in P4's `groupWriteTenant`, which does compensate.

### Reproducing the backfill

`docs/backfill-groups-job.yaml` is the in-cluster Job — deliberately
**unmeshed** (it only talks to Postgres, so there is no proxy to outlive the
container). It composes three databases' credentials and forces both sources
read-only; that is why it is a file rather than a paragraph.

## Next, in order

Items 1 and 2 of the previous list are done — `DROP_FOREIGN_TENANT_GROUPS` was
flipped (`#102`, R1 complete) and caller scope was settled by `#13` / `v0.1.2`.
What remains:

1. ~~Run `tenancy-check` in prod~~ — **done 2026-08-11**, both callers green.
   Re-run it after any credential rotation; it is the cheapest possible
   discovery that a key or a license is wrong. Also settle the kaufmann coverage
   caveat above (4 of 11 tenants hold a usable client id)
2. ~~Cutover~~ — **done 2026-08-11**, both apps, flag removed. The write half
   was missed and was fixed on 2026-08-12 — see "The write half was never cut
   over" above; it is not deployed yet, and **`tenancy-diff` should be re-run
   right after it is**. What cutover left behind, in priority order:
   - **Stop logging expected 4xx at error level in fleet-lite and kaufmann.**
     Both still do `logger.Err` for every non-404, and 403 is now the normal
     answer for a non-member, so this will feed error-rate alerting. Same
     one-line fix as `#15` here
   - **Move kaufmann off the capability proxy** to per-endpoint capability
     checks, which makes the `is_admin` replacement exact rather than 151/153
   - **Decide on kaufmann's `Access.GetWalletsWithAccess`.** Cutover ended its
     only call path, so the one-off wallet-checksum repair it carries no longer
     runs. Left in place deliberately rather than deleted as a side effect
   - ~~Fix the two Kaufmann-tenant memberships~~ — reviewed and accepted 2026-08-11
3. ~~The DIMO token minter~~ — **done 2026-08-13** (#27, `v0.4.1`). No caller
   yet; see the provisioning section above
4. The b2b operator console — **live 2026-08-13**, reached through kaufmann's
   proxies rather than a `/user/v1` surface. `/user/v1` remains unbuilt and is
   now driven by fleet-lite's needs (invitations, self-serve member CRUD), not
   the console's; b2b's own identity question stays deferred until something
   needs b2b to call this service directly
5. Groups move here — the plan in `plans/01-groups-into-tenancy.md`, agreed
   ordering recorded above (P1 ∥ P2, then P3 → P4 → P5)

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
  forward but does not undo prior exposure under the known key. **This is the
  only one of the three still open**, and removing the legacy shim below does
  not settle it: the shim controlled whether *this code* would read a
  weak-key row, not whether anyone else already had.
- ~~Delete the `AllowLegacyEmptyEncKey` shim~~ — **done 2026-08-12**
  (`fleet-lite-app#108`). `decryptSecret` now has no fallback key at all. A
  straggler row written under the empty key fails to decrypt at runtime rather
  than being silently readable; recovery is `reencrypt-tenant-secrets
  -from-empty-key`, which reads through `DecryptSecretWith` and depends on no
  app setting.
- ~~Publish the design docs~~ — **done 2026-08-12**, now at
  `docs/operator-tenancy/`. Both preconditions were met (group-id collision
  fixed by R1 and deployed; encryption fixed). Worth knowing: the gitignore was
  protecting nothing — a byte-identical copy has been public in `fleet-lite-app`
  since that repo's `#96`, and `fleet-lite-app` is a public repo too. Scanned
  before committing: no keys, no credentials, no wallet addresses.

## VEHICLE MEMBERSHIPS — steps 1–5 SHIPPED AND DEPLOYED, 2026-08-14

A new programme, planned and half-built in one session. The plan, the state and
the next steps live in
[`plans/02-vehicle-memberships.md`](plans/02-vehicle-memberships.md) — **start
there**, not here. This section exists so nobody rediscovers the feature by
finding an unfamiliar table.

**What it is.** Customers buy a **membership per vehicle**: a term of 1, 12, 24,
36 or 48 months, movable to another vehicle when one is discontinued. Operators
create and manage them from the b2b console; there is no purchase flow yet. When
an operator turns enforcement on for a customer, fleet-lite stops returning that
customer's vehicles that have no active membership.

**It is deliberately not part of `vehicle_entitlements`.** The entitlement
answers *may this customer see this vehicle*; the membership answers *is it paid
for, and until when*. Folding them together would make moving a membership a
revoke-and-regrant, discarding the entitlement's provenance as a side effect of
a commercial action — and it is what lets an entitlement be revoked without
destroying paid time, which is exactly the discontinued-vehicle case.

| Repo | PR | Released |
|---|---|---|
| this | #37 — schema, `MembershipService`, `/v1/tenants/{id}/vehicle-memberships` | `v0.6.0`, image `c4000ee` |
| kaufmann | #208 — five proxy routes | `v1.49.0` |
| b2b | #179 console UI, #180 BFF routes | `v1.9.0` |
| fleet-lite | #121 — read-only Memberships page in Account | `v0.9.0` |

Deployed in the load-bearing order, each verified before the next was tagged.
Migration `20260813160000` applied cleanly; all four services rolled with zero
restarts and no errors of their own.

**NOTHING IS ENFORCED.** `tenants.memberships_enforced` is `false` for every
tenant in production and fleet-lite does not read memberships at all yet, so no
customer's fleet has changed. That is the intended intermediate state.

### Things about it worth knowing before touching anything

- **Status is computed in SQL on every read**, never stored and maintained by no
  job. An expiry that depends on something having run is an expiry that silently
  does not happen the day that thing breaks.
- **The partial unique index is on `canceled_at IS NULL`, not "unexpired"** —
  `NOW()` is not immutable so an expiry test cannot appear in an index predicate
  at all. The service refuses the unexpired case; the index is only the
  read-then-write race backstop, the same split as
  `idx_vehicle_entitlements_one_active_holder`.
- **The insert's `::int` and `::timestamptz` casts are load-bearing.** Each of
  `$3` and `$4` appears in two type contexts, and without the casts Postgres
  rejects the statement at *runtime* with "inconsistent types deduced for
  parameter". It compiles fine either way.
- **`MembershipService.ActiveTokenIDs` has no HTTP route yet** and is reached
  only by its tests. Step 6 decides whether fleet-lite filters the list endpoint
  or gets a dedicated one.
- kaufmann's membership controller has its **own `fail()`** passing 404 and 422
  through as well as 409/400; the customer controller's narrower set was
  deliberately left alone rather than widened underneath existing endpoints.

### P5a went out with it

**kaufmann `v1.49.0` also carried P5a (#205)**, because a tag ships all of main.
Raised and taken deliberately rather than discovered. It rolled clean — zero
restarts, and the only errors in the following twelve minutes were the
pre-existing ruptela unknown-IMEI ingest failures. fleet-lite's half had been
live since `v0.8.0` the same day, so **P5a is now fully deployed on both sides
and its soak is running in production rather than pending.** The P5b blockers
recorded above are unchanged.

## FLEET-LITE OPENS OPERATOR-MANAGED TENANTS — built 2026-08-15, not yet merged

The first real provisioning run (TRAST under Kaufmann, 2026-08-14) proved the
console half works end to end and exposed that the fleet-lite half did not
exist: the provisioned member logs into fleets.dimo.co and gets the self-serve
onboarding screen. The plan, decisions and registration SQL are in
[`plans/03-fleet-lite-operator-tenants.md`](plans/03-fleet-lite-operator-tenants.md)
— **start there.**

Built, in dependency order:

| Repo | Branch | What |
|---|---|---|
| this | `feat/wallet-tenants` | `GET /v1/tenants?wallet=&surface=` (scope-filtered per row with the CallerMayAccess expression) + `POST /v1/tenants/{id}/members/{wallet}/login` |
| fleet-lite | `feat/operator-tenants` | app identity (LWD license as registered service caller) in the tenancy client; `GET /tenants` unions local + tenancy; middleware mirrors unknown tenants locally; DimoAuthProvider mints managed tenants' JWTs via the tenancy minter; explicit-mode entitlement sync **with deletion** |
| fleet-lite | `feat/member-write-through` (stacked) | the divergence fix: member/invitation writes go through to `PUT/DELETE /v1/.../members/{wallet}` (grants local-first, revokes remote-first, read-modify-write so console-granted capabilities survive), and the five owner gates become capability checks |

**Found while planning, worth knowing even if this programme stalls:** the
"both write memberships here (2026-08-12)" line earlier in this file was wrong
about fleet-lite — only kaufmann got the write half. Every fleet-lite member
write since cutover has reported success and conferred nothing. The stacked
branch is the fix; until it deploys, fleet-lite member management remains
decorative.

**Deploy order is the usual one, plus a data step in the middle:** this
service first, then the registration SQL from the plan appendix (fleet-lite's
LWD license `0x51dacC…` as a `tenant_credentials` row with
`is_service_caller=true`, no key material), then the fleet-lite branches. The
fleet-lite changes are inert until both exist — managed tenants keep 403ing
exactly as today.

**The registration ran on 2026-08-15** (through the tunnel, before either PR
merged — the row is inert until a caller presents that license, so order with
the code deploys does not matter for this step alone): tenant
`bf1aafcc-2dde-47ef-a52b-5bdb11dd82df`, and a re-run proved the guard
(`INSERT 0 0`, still exactly one `is_service_caller` row).

### DEPLOYED AND VERIFIED END TO END — 2026-08-15/16

All three PRs merged and released in order: #43 → `v0.9.0` (`cf1694c`,
rollout verified, zero errors), then fleet-lite #125 + #126 → `v0.11.0`
(`c4932be`, same). **The Phase 3 exit criterion is met**: the TRAST member
logged into fleets.dimo.co, landed in the tenant, and sees exactly vehicle
190171 (Mercedes-Benz GLA 200, synced through the minter + entitlement path
at 01:23:55Z) — no database touched. Both kaufmann diffs re-ran clean.

Two things the verification surfaced:

- **First-visit race, cosmetic:** opening a freshly provisioned tenant is
  what triggers the mirror + initial sync, so the very first garage render
  can beat the sync by a few seconds and shows empty until a refresh. Only
  affects a managed tenant's first-ever visit.
- **`POST /tenants` is the last un-cutover write, and it now BREAKS its own
  tenants.** "DIMO Test Fleet" (`c8726e5f…`, created locally 2026-08-14
  17:37, license `0x65e7Dd…`) exists only in fleet-lite; tenancy has never
  seen its license, so layer 2 rejects every authz call made as it and its
  owner gets 503s. Before this programme that was a listing gap; since
  cutover it is a broken tenant. Every self-serve tenant created in
  fleet-lite since the 2026-08-10 backfill has this problem. fleet-lite's
  `tenancy-diff`/`groups-diff` fail loudly on it (good), which also means
  they cannot run clean until it is fixed. Options: re-run the backfill
  (converges, picks it up), register it by hand (ciphertext copies straight
  across — the enc key is shared), or delete it if it was throwaway. The
  real fix is self-serve creation write-through — see next steps below.

**Verification gate:** `tenancy-check` still clean (the bounded path is
untouched); then the real thing — jreate@me.com into fleets.dimo.co, lands in
TRAST, sees exactly vehicle 190171, telemetry loads; then `tenancy-diff` and
`groups-diff` re-run clean. **Met 2026-08-16** — see the deployment section
above for the two findings.

### Self-serve creation write-through — DONE, deployed 2026-08-16

`#44` here (`v0.10.0`, `43a7a5b`) and fleet-lite `#127` (`v0.12.0`,
`28bd80b`), both rolled out clean. `POST /v1/tenants` creates tenant +
credential + owner membership in one transaction (service callers only) and
mints the id; fleet-lite creates remote-first and materialises its local row
under that uuid. `PUT /v1/tenants/{id}/credentials` is the mint-validated
rotation path — and the managed-customer graduation path. **Every write path
is now cut over.**

The two casualties of the old gap were resolved the same day:

- **DIMO Test Fleet healed** by id-preserving registration (tenant +
  ciphertext credential copy + owner membership, original timestamps —
  the backfill's rule applied to one tenant, by hand through the tunnel).
- **Wallet `0x264BC4…` in the Kaufmann tenant** had been granted fleet-lite
  `owner` locally on 2026-08-14 — inside the inert-write window, so it
  conferred nothing. Reviewed 2026-08-16: his remote admin capabilities are
  the intended access, so the LOCAL label was aligned to `admin` rather than
  replaying the owner grant (which would have added manage_settings on the
  production operator tenant). Effective access unchanged.

**Both fleet-lite diffs now run fully clean** — `differ=0, missing_remote=0`
— for the first time since the write gap opened.

### What remains of this programme, in priority order

0. **RESOLVED 2026-08-17 — fleet-lite's groups cronjobs recovered on their
   own.** Both succeeded that morning (`mirror-groups` 06:15, `groups-diff`
   06:45 — its first recorded success ever) and ArgoCD returned to Healthy.
   The cause of the earlier failures was never established and the evidence
   is gone; if it recurs, catch a run in the act rather than inferring from
   an empty job list. Original write-up kept below.

   **FOUND 2026-08-16 — fleet-lite's groups cronjobs are failing
   and nobody would know.** ArgoCD shows `fleet-lite-app` **Degraded** since
   06:15 UTC (the `mirror-groups` schedule). `mirror-groups` last succeeded
   **2026-08-14**; `groups-diff` has **never** recorded a success since its
   cronjob was created. Both run fine when invoked by hand — `groups-diff`
   in the app pod exits 0 with `87 groups, 82 agree, 5 remote-extra,
   differ=0, missing_remote=0`, and a manually-triggered Job from the same
   cronjob template completes — so the group data is NOT drifting and the
   P3/P4 gate still holds. The failure is environmental and specific to the
   scheduled runs.
   **The blocker to diagnosing it: fleet-lite's cronjob Jobs vanish.**
   `failedJobsHistoryLimit=3` and `ttlSecondsAfterFinished=3d` should retain
   them, and kaufmann's identical cronjobs keep theirs for days, but
   fleet-lite's leave nothing behind — so every failure erases its own
   evidence. The two cronjob specs are otherwise identical (concurrency,
   deadline, ttl, mesh annotations, proxy-shutdown wrapper). Leading
   hypothesis is ArgoCD's automated prune removing Jobs that carry the app's
   tracking labels but do not exist in git; kaufmann's surviving is the fact
   that argues against it and needs explaining. **Next step: catch a failing
   run in the act** — watch the 06:15/06:45 UTC window, or suspend the
   cronjob and run the Job manually on that schedule — rather than inferring
   from an empty job list.

1. **Invitations move to tenancy** — **P1 DEPLOYED 2026-08-16** (#45,
   `v0.11.0`, image `35364e8`): migration `20260816120000`,
   `InvitationService`, the `/v1/tenants/{id}/invitations` CRUD + resend,
   `POST /v1/invitations/accept` (accept grants the membership and marks the
   row in one transaction), Postmark send with locale templates, and
   `POST /webhooks/postmark` — verified live from inside the pod (`/version`
   = the deployed sha, new routes 401 without a key, webhook 403 without the
   secret, zero errors after rollout). The proposed decisions were adopted
   as written; the plan records the naming details settled during the build
   and the deploy snag (an out-of-band draft `invitations` table sat in both
   local and prod, empty; dropped so the migration could apply — the
   crashlooping migrate container's "column does not exist" was IF NOT
   EXISTS skipping the CREATE and the index statements hitting the old
   table). `prod/fleet-tenancy-api/postmark_webhook_secret` exists in AWS
   and the ExternalSecret synced it (64 chars, no whitespace).
   [`plans/04-invitations-into-tenancy.md`](plans/04-invitations-into-tenancy.md)
   — **P2 is next**: id-preserving backfill + flagged fleet-lite cutover
   (outstanding links must survive — the token hashes copy), then console
   proxies + UI (P3), then the local table drops with Phase 5. Note P2's
   webhook repoint needs an ingress for exactly `/webhooks/postmark` — this
   chart deliberately has none. Probing tip re-learned during verification:
   the pods are meshed, so an unmeshed curl pod gets linkerd's empty-body
   403 for every path except `/health`; probe via `kubectl debug
   --target=fleet-tenancy-api` and curl localhost instead.
2. **Collapse `GET /tenants` to tenancy-only** — every tenant now writes
   through, so the local-list union is only a soak-period safety. After a
   quiet window, drop it.
3. **Managed tenants read "cold" to the group-sync tiering** —
   `HasRecentLogin` reads local `tenant_users`, which managed tenants never
   have; their `last_login_at` lives on the shared membership (written by
   the new login touch). Move the tiering read to tenancy or accept the
   weekly-pass cadence for managed tenants.
4. **`prune-unshared-vehicles` predates explicit mode** — it fetches by the
   tenant's own license and would error-skip a managed tenant. Harmless
   (the entitled sync prunes for them), but it should skip explicitly.
5. **R6 scale tiering** (master-pass per operator, batched upserts) — still
   deferred until an operator with a large fleet onboards a fleet-lite
   customer; the seam is `syncEntitledVehicles`.
6. Then the old decommission list (migration-plan Phase 5): local
   `tenant_users`/`invitations` become read caches and drop, alongside the
   groups-move P5 work already tracked above.

### The webhook ingress, and the Linkerd trap it walked into (2026-08-16)

`POST /webhooks/postmark` is this service's only public surface. It is served
by a **separate Fiber app on its own port** (`WEBHOOK_PORT`, 8087,
`internal/app/webhook_app.go`) holding that one route plus `/health`, with the
chart's ingress targeting that port by name — so `/v1` is unreachable from the
internet because the process does not serve it there, not because an ingress
rule says so. A test asserts every internal route 404s on that app. Live at
`fleet-tenancy-webhooks.dimo.co` (#47, `v0.13.0`).

**It shipped broken, and the way it broke is the part worth keeping.** DNS,
TLS and routing were all correct; every request still died at the mesh with

```
HTTP/2 403, content-length: 0
l5d-proxy-error: server: 10.0.8.201:8087: unauthorized request on route
```

An **empty-bodied 403 is indistinguishable from the handler correctly refusing
bad credentials** — which is exactly what this endpoint is supposed to return
to an unauthenticated caller. Only the missing JSON body and the
`l5d-proxy-error` header separate "working as designed" from "silently
dropping every Postmark event". The in-cluster probe passed throughout.

The cause: the prod namespace sets `default-inbound-policy: deny`, and this
cluster authorizes **by port name** — namespace-wide `Server`s (`http-port`,
`https-port`, `grpc-port`, `metrics-port`) select every pod and match the port
names `http`, `https`, `grpc`, `mon-http`. A pod cannot have two ports named
`http`, so the new one is named `webhook`, matched nothing, and fell through
to deny. That is also why `/v1` on 8084 never had this problem.

Fixed in #48 with a `Server` + `ServerAuthorization` pair, scoped tighter than
the shared `http-port-access` (which admits every prod workload): only the
ingress controller's identity, since nothing in-cluster should call this port.

**Any future non-standard port name in any chart here needs the same pair, or
it fails identically and misleadingly.**

**Verified end to end from the public internet, 2026-08-17:**

| Request to `fleet-tenancy-webhooks.dimo.co` | Result |
|---|---|
| `POST /webhooks/postmark`, no credentials | 403 `{"code":403,"message":"invalid webhook credentials"}` — our handler, JSON body, no `l5d-proxy-error` |
| `POST /webhooks/postmark`, wrong password | 403 |
| `POST /webhooks/postmark`, real secret, unknown message id | **200 `{"ok":true}`** — the delivery path works |
| `GET /v1/authz`, `/version`, `/health` | nginx's HTML 404 — never routed, never reached the pod |

The authenticated probe used an unknown message id, which the handler ignores
by design: the 14 invitation rows were unchanged and none carried the probe id
afterwards. Rejections log at **warn**, so a scanner hitting this endpoint
cannot feed error-rate alerting.

**Postmark's webhook URL still points at fleet-lite.** Repointing it to
`https://fleet-tenancy-webhooks.dimo.co/webhooks/postmark` is Postmark-side
config and the last step of P2; both receivers tolerate unknown message ids
silently, so it needs no coordination with the flag flip.

> **Superseded 2026-08-17 — there is no repoint.** The two apps use *different*
> Postmark servers, so each has its own webhook and fleet-lite's keeps pointing
> at fleet-lite for its own inert history. See the cutover section below.

### Invitations P2 — backfilled and diffed, flag still OFF (2026-08-16)

| Step | Result |
|---|---|
| `backfill-invitations -dry-run` | 14 source invitations (3 pending, 7 accepted, 4 revoked), 0 already here, `pending_and_unexpired=0` |
| `backfill-invitations` (real) | **14 written**, counter reconciled against the source count |
| Cross-database fingerprint | `md5(id||token_hash||status||epoch(expires_at))` over all rows, computed independently on each side: **`0aa609d94f2e4ac2c412bd87c49b1c19`, 14 rows, identical** |
| fleet-lite `invitations-diff` | 6 tenants, 14 invitations, **14 agree, differ=0, missing_remote=0** |

The fingerprint is the check worth repeating if this is ever re-run: it proves
the fields that decide whether an emailed link resolves — id, token hash,
status, expiry — match exactly, which neither the row counts nor the diff can
show (the diff cannot see the hash, by design).

**No live accept links exist in production right now.** All three pending
invitations expired, the newest on 2026-08-07. The outstanding-link guarantee
therefore has nothing to protect today, and real data will never exercise it —
the deliberate before/after test is the only proof there will ever be. Do not
skip it because the flip now looks safe.

**The sequence this section prescribed — ALL DONE 2026-08-17.** Kept because
the reasoning behind step 2 is the part worth re-reading if this is ever run
in another environment. The cutover it led to is recorded below.

1. **Send a test invitation** from fleet-lite while the flag is still off.
2. **RE-RUN `backfill-invitations`.** With the flag off, fleet-lite writes new
   invitations to its OWN table only, so anything created after the first
   backfill — the test invitation included — does not exist here. After the
   flip, `Accept` resolves the token against THIS service, finds no matching
   hash, and answers 410. The test would fail, and it would fail looking
   exactly like the cutover having broken the outstanding-link guarantee it
   was written to prove. The backfill upserts by id, so a re-run is safe and
   converges; run it as late as possible before the flip, and remember this
   applies to every invitation a customer creates in that window, not just
   the test one.
3. **`invitations-diff`** — must be clean before going further.
4. **Flip `INVITES_FROM_TENANCY`** (chart-only, fleet-lite `values-prod.yaml`).
5. **Accept the test invitation.** This is the only proof the outstanding-link
   guarantee ever gets, since no real live links exist (see above).
6. ~~**Repoint Postmark's webhook URL**~~ — **void, never done and never will
   be.** The premise was wrong: the two apps use different Postmark servers, so
   there is nothing to repoint and no coordination window. Step 6's reasoning is
   kept below only because the delivery-tracking principle in it still holds.

On step 6's timing: do it just AFTER the flip, not before. Delivery tracking
resolves only at the receiver that holds the row, so whichever service is
SENDING should be the one receiving. Before the flip fleet-lite sends, so
repointing early means new invitations lose their delivery badges; after the
flip this service sends, so repointing late costs the same in the other
direction. The window is what matters, not the order — keep it short. Nothing
breaks either way: tracking is advisory, both receivers ignore unknown message
ids silently, and a resend re-establishes tracking on any invitation that
missed its events.

### INVITATIONS CUTOVER COMPLETE — 2026-08-17

`INVITES_FROM_TENANCY` is **on** in prod (fleet-lite #129). This service now
mints every invitation token, sends every invitation email, receives the
delivery webhooks, and writes the membership at accept. fleet-lite's local
`invitations` table is inert and drops in P4.

**The outstanding-link guarantee is proven, not assumed.** An invitation was
created in fleet-lite before the flip, backfilled, and accepted after it:

| Check | Result |
|---|---|
| `sha256(token from the email)` vs stored hash | identical in BOTH databases, verified before flipping |
| `POST /invitations/accept` after the flip | 200, resolved to the right tenant |
| Membership written | `role=member`, `permissions=[]`, scope as invited |
| `accepted_at` vs membership `created_at` | **identical to the microsecond** — the single-transaction write, visible in the data |
| fleet-lite's local row | untouched, still `pending` — correctly inert |

A second invitation was then created through this service and accepted by a
real user through fleets.dimo.co with a passkey — the whole post-flip path,
including `scopeGroupIds: []` landing as `{}` rather than NULL.

**Two things the plan got wrong, both found by testing rather than reading:**

1. **The two apps use DIFFERENT Postmark servers.** The plan assumed the
   template aliases were shared server-side assets and only the config moved.
   Ours held only `fleet-access-granted`, so with the flag on every invitation
   send would have failed template lookup — recorded, `emailSent=false`, no
   email. Caught minutes after the flip with zero invitations created in the
   window. Templates ported and now in-repo (#49).
   **Consequence: there is no webhook "repoint" and no coordination window.**
   fleet-lite's Postmark server keeps its own webhook for its inert history;
   this service's server got its own, pointing at the ingress.

2. **A webhook configured after a send loses that send's events.** Postmark
   fired Delivered and Opened one second after the test send and had nowhere
   to POST them. Not a bug — but configure the webhook BEFORE the first send
   in any new environment. Replaying a real event afterwards upgraded the row
   `sent → delivered`, which is what proves the receiver end to end.

**What remains:** P4 (drop fleet-lite's table, service, webhook route, its
Postmark webhook secret, and the flag). P3 — the console — shipped later the
same day; see the section below. `invitations-diff` still runs, but post-flip
its value is only confirming the backfilled rows still agree — new invitations
exist solely here and count as `remote-extra`.

### Invitations P3 — MERGED AND RELEASED 2026-08-17

The operator console can now invite a customer's users by email. It could
already **provision** a person — creating a DIMO account and wallet on their
behalf — but not invite one, because the invitation records lived in fleet-lite
where kaufmann cannot reach them. D4 keeps both paths deliberately: invitation
is the one for when creating an account for somebody would be presumptuous,
which is most of the time for a customer's own staff.

| Repo | PRs | Released as |
|---|---|---|
| kaufmann | #216 chart drift fix, #215 invitation proxies | `v1.51.0` — `values-prod.yaml` verified repinned to `1.51.0` |
| b2b | #183 console UI | `v1.10.0` — `values-prod.yaml` verified repinned to `0f8eedc` (a short SHA, not a version — see below) |

Nothing was needed in this service: the records and the email dispatch have
lived here since P2. kaufmann's four handlers authorize, forward and translate,
gated on `manage_members`, with `invitedByWallet` folded in from the
authenticated user rather than the request body — so the audit trail and the
email's "invited by" line record who really sent it.

**Still to verify in the console**, not done at time of writing: open a
customer's Users tab, send an invitation, confirm it renders with "sent by you"
and a Delivery column, and that the invitee can accept in fleet-lite. That is
the first exercise of the post-flip send path through the console rather than
through fleet-lite.

#### Two release mechanics worth not rediscovering

**The two repos pin prod differently.** kaufmann's tagged build uses
`strip_v: true` and writes the *version* into `values-prod.yaml` (`1.51.0`);
b2b's computes `BUILD_TAG` as the short commit SHA and writes *that*
(`0f8eedc`), so a b2b version number never appears in its chart at all — the
tag only triggers the build. Seeing a SHA in b2b's `values-prod.yaml` is
correct, not a failed release. b2b also has no dev environment (the workflow
says so: Login-with-DIMO makes one impossible), so its tagged build goes
straight to prod.

**Read the tags, never the prose, before cutting a release.** An earlier draft
of this file was stale on both repos. b2b in particular left the 1.6.x line
entirely — 1.6.17 → 1.7.0 → 1.8.0 → 1.9.0 → 1.10.0 — so guessing "v1.6.18" from
a stale note would cut a tag sorting below four existing releases. Use
`git tag --sort=-v:refname | head -3`. Both repos use **annotated** tags.

#### The image-version step is the single point where a release half-lands

Both repos' build workflows end with the same `yaml-update-action` step, and it
is the only thing that moves an environment — the image push before it is inert
on its own. During the GitHub incident of 2026-08-17 that step failed alone,
repeatedly, on both repos, while every step before it succeeded. Two
consequences, both of which cost time here:

- **A red build does not mean a missing image.** Both post-merge dev builds
  showed `failure` with the image already pushed to DockerHub; only the chart
  commit was lost. Re-running with `gh run rerun <id> --failed` re-ran just that
  step. It also means dev silently ran two merges behind for several hours.
- **A green build is not proof prod moved.** The check that matters is the file:
  `values-prod.yaml` for a tagged build, `values.yaml` for a dev one. Verify the
  content, not the run's conclusion.

The same incident produced one ambiguous failure worth handling differently from
the rest — a TLS handshake timeout on tag-ref creation, where the write may or
may not have landed. The clean 503s could be retried blindly; that one had to be
checked first (`gh api repos/<owner>/<repo>/git/ref/tags/vX.Y.Z`) to avoid a
duplicate or confusing tag in a deploy-on-tag repo. It had not landed.

### Invitations P4 — DECOMMISSIONED 2026-08-17

fleet-lite no longer has an invitation implementation. Everything that made it
a second home for invitation records is gone, and this service is the only one.

| Repo | PRs | Released as |
|---|---|---|
| fleet-lite | #130 code + flag + Postmark, #131 the table drop | `v0.14.0` — `values-prod.yaml` verified repinned to `f8cab9a` |

**Shipped as two PRs on purpose.** #130 is reversible — it deletes the local
path, `INVITES_FROM_TENANCY` and its branching, the `/webhooks/postmark`
receiver, `invitations-diff`, and six settings with their chart values and
secret refs. #131 is not: it drops the table. Keeping them apart meant merging
the first did not commit anyone to the second, and the irreversible change is
one revert away from being isolated in history.

**Postmark left fleet-lite entirely, which the plan's one-liner understated.**
It named "webhook route, Postmark webhook secret", but Postmark existed in that
repo *only* for invitations — the only templates were `invitation.*` — so the
gateway, both CLI commands (`push-postmark-templates`,
`configure-postmark-webhook`) and the server token went too. Removing just the
webhook would have stranded all of it wired to nothing.

**The drop's down migration restores shape, not rows.** That is the honest
recovery story: the rows are here, backfilled by id with a matching
cross-database fingerprint before the flip. Worth repeating if this is ever
re-derived — the down migration was WRONG on first writing, and the way it was
caught generalises: build a reference database from the original migrations,
roll the new one forward and back, and diff the two schemas. That surfaced a
missing `ON UPDATE CASCADE`, added to the foreign key by
`20260805170000_unify_kaufmann_tenant_uuid` so a re-keyed tenant uuid carries.
A rollback would have restored a table whose FK silently blocks the very re-key
that migration exists to allow — invisible to any test, and to review.

**`make sqlboiler` in fleet-lite does not reproduce its committed models.** The
shared where-helpers `whereHelpernull_String` and `whereHelpernull_Time` both
lived in the generated `invitations.go`; regeneration relocated the first and
dropped the second, leaving three files using a type nothing defines. This is
not drift caused by the schema change — a control run against the *pre-drop*
schema also fails to emit it while the committed file defines it. The helper was
moved by hand to keep the package compiling. Anyone regenerating those models
for an unrelated reason will hit this; it deserves its own fix.

### Still outstanding after P4

- **The two AWS secrets still exist**: `prod/fleet-lite-app/postmark_server_token`
  and `prod/fleet-lite-app/postmark_webhook_secret`. The chart no longer
  references them, so they are inert — delete them only once the chart without
  those `remoteRef`s is confirmed running, never before. A missing ref fails the
  whole ExternalSecret and takes the DB credentials with it.
- **Postmark-side**: fleet-lite's own Postmark server and its webhook can be
  retired once its delivery history stops mattering. Remember there is no
  "repoint" — the two apps always had separate servers.
- ~~Not verified in-cluster~~ — **verified 2026-08-17 20:20 UTC.** All three of
  the day's releases are live with 2/2 pods and zero restarts: fleet-lite
  `f8cab9a` (`v0.14.0`), kaufmann `1.51.0` (`v1.51.0`), fleet-onboard-app
  `0f8eedc` (`v1.10.0` — the deployment is `fleet-onboard-app-prod`, not
  `fleet-onboard-app`). **The drop ran**: fleet-lite's migrate init container
  reports `current version: 20260817180000`, and the second pod correctly found
  nothing to do. Error streams since the rollout: fleet-lite 0, this service 0,
  kaufmann 2 — both the pre-existing benign SACD token-exchange 403s under
  `0xCa977Abb…`, unrelated to any of this.

  Worth keeping for next time: the drop runs in the migrate init container, so a
  failure means the pod never starts and
  `kubectl -n prod logs <pod> -c fleet-lite-app-migrate` is the first place to
  look, not the app logs. Note kaufmann's app image is the *second* container
  (`tls-offload` is first), so `containers[0].image` reports ghostunnel.
- **The P3 console verification never ran** — send an invitation from a
  customer's Users tab, confirm "sent by you" and the Delivery column, and that
  the invitee can accept in fleet-lite. P4 shipped ahead of it, which is out of
  order against the plan's own gate ("after a soak with the flag on and the diff
  clean"); the flag flipped the same day. Nothing has exercised the console send
  path end to end.

## Vehicle sharing — RELEASED 2026-08-19, UNEXERCISED

A fleet-lite customer shares a vehicle with a 0x wallet: an on-chain SACD
grant, signed server-side by the operator's signer on the vehicle owner's
kernel account. The owner keeps the NFT and never signs. Same mechanism
kaufmann uses to re-share a transferred vehicle, pointed at a grantee the
customer chooses.

This service became an on-chain **writer** for the first time. That is the fact
worth carrying: it now holds River, a bundler connection and a code path that
spends gas, alongside the `/v1/authz` hot path both apps fail closed on.

| Repo | PRs | State |
|---|---|---|
| this | #53 prod values, #50 tx plumbing, #56 chart secrets, #52 foundations, #54 write path | `v0.14.0`, image `2b24cb7`, in prod |
| fleet-lite | #132 api + `canShare`, #133 list-view button and modal | `v0.15.0`, image `49aafdb`, in prod |
| kaufmann, b2b | none — nothing was needed | — |

```
POST /v1/tenants/{id}/vehicles/{tokenId}/share         -> 202 {jobId}
GET  /v1/tenants/{id}/vehicles/{tokenId}/share/status  -> {isSuccessful}
POST /v1/tenants/{id}/shareable-owners                 -> the display gate
```

### Live, but nothing has sent a UserOp yet

Both services are in prod and healthy (2/2, zero errors on rollout). Every layer
is unit-tested, the queue is proven against a real database (migrations apply,
the pgx pool resolves `search_path` to the `fleet_tenancy_api` schema, the
client starts and registers in `river_queue`), and all three routes refuse an
unauthenticated caller. **No share has been made.** The first one is still the
test that matters — a healthy rollout proves the queue starts, not that a
UserOp lands:

```sh
kubectl -n prod logs -l app.kubernetes.io/name=fleet-tenancy-api \
  --all-containers -f | grep -iE 'share|sacd'
```

Success is `vehicle share granted on chain` with a `tx_hash`. The share button
in a real fleet list is also unexercised — only the modal was, in isolation.

Config went to prod ahead of the code, which is the ordering the chart/image
split exists to give us. `values-prod.yaml` carries `SACD_ADDRESS` and the five
keys #53 restored; ASM holds `prod/fleet-tenancy-api/{rpc_url, bundler_url}`,
copied from `prod/kaufmann-oracle/web3/{rpc,bundler}`. As of `v0.14.0`
`SharingConfigured()` is true, the queue is running, and `Settings.Validate`
enforces all-or-nothing on the sharing settings.

### Who can actually share — the caveat to know

Sharing needs a signer key on the tenant's **effective** credential.

| Tenant kind | Signer | Sharing |
|---|---|---|
| Operator | backfilled from kaufmann | yes |
| Managed customer | none of its own; `Effective()` resolves to the operator's | yes, inherited |
| Self-serve | own credential, **no signer** | no |

The backfill and `CreateSelfServeTenant` write only `dimo_client_id` and
`dimo_api_key_enc` for self-serve tenants, so those tenants get `canShare`
false everywhere and a direct call answers 409 "this tenant has no signer
configured". It fails closed and legibly, and it matches who the feature was
designed for — customers whose vehicles arrived through kaufmann's
email-transfer flow — but it is stated nowhere else. Giving self-serve tenants
a signer is a real change, not an oversight to patch quietly: kaufmann's
`backfill-tenant-signers` generates and encrypts a keypair for tenants missing
one and would port directly.

`SetCredentials` was checked and is safe: it updates only the client id and API
key on conflict, so re-keying a license from fleet-lite's Settings does not
clear the signer and silently disable sharing.

### Decisions worth not re-litigating

- **The authorization chain runs twice, and the worker's run is the one that
  matters.** The handler checks so the customer gets a synchronous answer; the
  worker re-resolves entitlement, owner and signing authority immediately
  before the call, because a job can sit in the queue while the vehicle is
  transferred or the owner revokes our signer. The worker trusts nothing from
  its own row except which tenant asked for what.
- **`manage_vehicles` is checked at the HTTP boundary and deliberately not in
  the worker.** Capability is a property of the request; re-reading it at
  execution time would let a membership edit between submit and run cancel work
  that was permitted when it started. It is checked against this service's own
  authz rather than trusted from fleet-lite — the same gate kaufmann's
  shared-account routes were missing.
- **`MaxAttempts: 1`.** A retry cannot distinguish "never sent" from "sent,
  receipt poll timed out", and the second case re-grants something the customer
  may since have revoked.
- **The display gate resolves live against accounts-api, not from
  `users.shared_account_signer_address`.** That column has one writer —
  `ProvisionService`, only when this service created the account — so it is
  empty for every owner whose account kaufmann created, which is exactly the
  population sharing targets. Resolving live also makes the display gate and
  the execution gate the same question against the same source, so they cannot
  disagree the way kaufmann's cached column can.
- **A 403 and a 502 must never collapse into each other.** An accounts-api
  outage that read as "not authorized" would tell every customer their owner
  had revoked a signer that was never revoked. Both directions are tested.
- **Permissions are the frontend's default set, including `COMMANDS`** —
  everything except `APPROXIMATE_LOCATION`, cross-checked against b2b's
  `sacdPermissionValue`. `COMMANDS` grants lock/unlock to a stranger and is in
  by an explicit 2026-08-18 decision, with a test whose only job is to make
  removing it deliberate.
- **Status is the single-job `isSuccessful` boolean**, never a `"Success"`
  string. Both conventions exist in kaufmann for different operations.

### Two things found while building, both silent failures

- **River will not start with an empty worker bundle** ("at least one Worker
  must be added"). The plan called for registering an empty one; built that way
  it becomes `logger.Fatal` at startup in exactly the environments where
  sharing is configured — a two-app outage from a feature neither app calls.
  `NewQueue` returns `(nil, nil)` for a nil bundle instead. Only found by
  running against a real database.
- **`values-prod.yaml` was missing five settings** `values.yaml` has, including
  `VEHICLE_NFT_ADDRESS`, which `SharingConfigured()` requires — sharing would
  have been silently off in prod. Fixed in #53, and it was not only a sharing
  problem — see below.

### #53 — the minter settings were never in prod

`DIMO_AUTH_URL`, `ACCOUNTS_API_ENDPOINT`, `IDENTITY_API_ENDPOINT`,
`TOKEN_EXCHANGE_URL` and `VEHICLE_NFT_ADDRESS` were added to `values.yaml` when
the minter shipped — the provisioning section above records them as "chart
values.yaml". But `buildpushprod` writes `values-prod.yaml`, and `buildpushdev`
says of itself *"while there is no dev environment"*. The settings went into the
one chart file that never reaches a pod.

Nothing caught it because nothing ran the code path: the same section records
that provisioning has never been exercised against real accounts-api. The first
deliberate console use would have failed on `"DIMO_AUTH_URL is not configured"`
— pointing at config that looks correct in `values.yaml`.

Both files now carry identical env key sets. Worth confirming on the next
rollout:

```sh
kubectl -n prod exec deploy/fleet-tenancy-api -- env | grep -E \
  'DIMO_AUTH_URL|ACCOUNTS_API_ENDPOINT|IDENTITY_API_ENDPOINT|VEHICLE_NFT_ADDRESS|SACD_ADDRESS|RPC_URL|BUNDLER_URL'
```

### Deliberately not built

Revoke (the same machinery with zeroed permissions); the passkey signing path
for owners whose accounts this ecosystem did not create — phase 2, extracted
from `b2b-fleet-mgr-app/web` and published as an npm package; and surfacing
sharing in the b2b console. A per-permission picker was also left out: v1 sends
one fixed mask, and the request schema already carries an optional
`permissions` field so a picker is frontend-only work later.

### A trap for whoever merges the next stack

Squash-merging a stacked PR and deleting its branch **closes** the child PRs
rather than retargeting them, and GitHub refuses to reopen a PR whose base
branch is gone. #51 was lost that way and reopened as #56. Retarget children to
`main` before merging the parent, or do not delete base branches until the
whole stack has landed.

## Session handoff, 2026-08-19 ~18:30 UTC — step 1 SHIPPED, kept for the incident record

**Step 1 is done, released and verified in prod** — fleet-lite-app#134 and
#135, released as `v0.16.0` on 2026-08-19; see the section at the end
of this file. Everything below is the incident analysis that produced it, which
is still the best account of *why* the empty fleet happened and is worth reading
before touching the sync or plan 07's remaining steps. Treat its instructions as
history: they describe work that has been done.

### What happened

A TRAST admin (`jreate@me.com`, wallet `0x6272d24f…`) saw **zero vehicles** in
fleet-lite while TRAST had nine entitled vehicles and nine active commercial
memberships. Nothing errored anywhere.

Two causes, stacked:

1. **The nightly sync skips every credential-less tenant.**
   `fleet-lite-app/api/cmd/fleet-lite-app/sync_vehicles.go:60` builds its own
   `VehicleService` and never calls `UseTenancy` — the only call is in
   `api/internal/app/app.go:118`, the web server. So `syncEntitledVehicles`
   hits its *"no tenancy client is configured"* guard
   (`api/internal/service/vehicle.go:247`), the loop logs
   `sync vehicles, skipping tenant`, and **the run still exits 0**.
2. **A cached set under live gates.** `fleets_lite.vehicles` is a nightly cache;
   the membership and group-scope gates resolve from this service on 60s TTLs
   (`GROUPS_FROM_TENANCY=true` in prod). TRAST's only local row was `190171` —
   an entitlement *revoked* on 2026-08-18 — and the live membership gate
   correctly excluded it. Cached-set ∩ live-gate = ∅.

Had both been stale you would have seen one wrong vehicle. Had both been live,
nine. **Zero is the artifact of mixing**, and it is silent.

### What was already done — do not redo

- **TRAST was repaired by hand**, 2026-08-19 18:03 UTC, via
  `POST /tenants/f004fc62-752b-4d87-9de9-c20c56e67248/sync-vehicles` with an
  end-user JWT. Verified: nine rows, `190171` pruned, and the local set now
  matches entitlements and memberships exactly.
- **This is a one-time repair.** The cron errors before it writes, so it does
  not re-break TRAST — it simply never updates it. TRAST stays correct until the
  next entitlement change and then drifts again, silently.
- Note the plan's evidence section quotes TRAST's *broken* state. That is
  history, not a live reading. Query it now and you will find nine healthy rows.

### Step 1, concretely

Three changes; the second matters most.

1. Wire `vehicleSvc.UseTenancy(tenancyAPI)` in `sync_vehicles.go`, as `app.go`
   does. While there, audit the same file for other by-hand-built services
   missing wires — `UseMemberships` and the group index are constructed the same
   way and were not checked.
2. **A skipped tenant must exit non-zero.** Count skips, log each with its
   reason, fail the run. The wiring omission cost three days of a customer
   seeing an empty fleet *because the CronJob stayed green*. Fixing only (1)
   leaves the next omission to cost the same again.
3. Add a `vehicles-diff` command alongside `tenancy-diff` and
   `groups-diff` — in fleet-lite-app, where those live; `invitations-diff` was
   deleted in 7ed9bd8 when P4 retired the local invitation path, leaving no
   local side to diff. Per explicit-mode tenant, compare this service's active
   entitled set against fleet-lite's local rows, reporting
   `agree / missing_local / extra_local`. From 2026-08-18 it would have printed
   `missing_local=9, extra_local=1` for TRAST.

### Verifying

The CronJob is `fleet-lite-app-sync-vehicles` (`0 3 * * *`). Its job pods are
garbage-collected quickly, so **do not plan on reading yesterday's logs** — that
is why the skip was never seen. Trigger one by hand instead:

```sh
kubectl -n prod create job --from=cronjob/fleet-lite-app-sync-vehicles \
  sync-vehicles-manual-$(date +%s)
kubectl -n prod logs -l job-name=sync-vehicles-manual-... --tail=200 | \
  grep -iE 'skipping|synced|complete'
```

Before the fix TRAST is skipped and the run still succeeds; after it, TRAST
syncs and any genuine skip fails the job.

### Reaching prod data

An SSH tunnel to the prod RDS is how all of the above was measured:

```sh
ssh dimo-database-prod          # ~/.ssh/config, LocalForward 5430 -> RDS:5432
```

Per-service DB users are scoped to their own schema — kaufmann's credentials
cannot read `fleet_tenancy_api` or `fleets_lite`. Take each service's own
credentials from its k8s secret:

```sh
kubectl -n prod get secret fleet-tenancy-api-secret \
  -o jsonpath='{.data.DB_USER}' | base64 -d      # and .data.DB_PASSWORD
# fleet-lite-app-secret likewise; then psql -h localhost -p 5430 \
#   -d fleet_tenancy_api | fleets_lite  (PGSSLMODE=require)
```

Tables live in a schema named for the database, not `public` — qualify as
`fleet_tenancy_api.tenants`, `fleets_lite.vehicles`, `kaufmann_oracle.vins`.

### What is NOT started

Steps 2–5 of plan 07 — the freshness fix, the roster table here, the reader
cutover, and shrinking `vins`. Plan 06 (signer-key consolidation) is also
unstarted; its step 1 is read-only and cheap.

**Nothing about vehicle sharing has been exercised yet** — still no UserOp ever
sent. Unchanged by this session.

---

## Session handoff, 2026-08-19 ~20:00 UTC — step 1, kept for the release record

Step 1 of [`plans/07-vehicle-roster.md`](plans/07-vehicle-roster.md) is done,
released and verified in prod. **Start with step 2** — the freshness fix — or
with plan 06 step 1, which is read-only and cheap.

### What shipped

**fleet-lite-app [#134](https://github.com/DIMO-Network/fleet-lite-app/pull/134)
and [#135](https://github.com/DIMO-Network/fleet-lite-app/pull/135), released as
`v0.16.0`** (image `bbfd63a`, ArgoCD Synced/Healthy, 2/2 pods). All of it is in
that repo; nothing in step 1 touched fleet-tenancy-api, despite the plan saying
"here".

1. `UseTenancy` wired in `api/cmd/fleet-lite-app/sync_vehicles.go`, guarded on
   `Configured()` as `app.go:118` does.
2. A skipped tenant now fails the run: skips collected not just counted, each
   logged with a `reason`, the summary logged at error level naming
   `skipped_tenant_ids`, and `ExitFailure` returned. The loop still continues
   past a bad tenant — one tenant's failure should not cost the rest their sync.
3. New `vehicles-diff`, alongside `tenancy-diff` and `groups-diff`, with a
   CronJob at 03:30.

**The wiring audit came back negative on purpose.** `UseMemberships` and the
group index are read-time filters that the sync path never consults, so wiring
them would be dead weight that reads as coverage. The reasoning is in the code
comment; do not reopen it.

**The chart carried half the fix, and this was not in the plan.** `sync-vehicles`
inherited the template's 1-hour `ttlSecondsAfterFinished` — which is *why* the
skip went unseen; the pod was collected before anyone looked — and
`backoffLimit: 1`, which would retry a deterministic skip and double the log.
Both now match `groups-diff`: `0` and three days. A non-zero exit nobody can read
is not much better than a green one.

### What was verified — all of it, in prod, on `v0.16.0`

The bug was reproduced first, on the then-deployed image, so there is a real
before/after rather than an assertion:

| | baseline (`49aafdb`) | released (`v0.16.0` / `bbfd63a`) |
|---|---|---|
| `sync-vehicles` | `synced=612 skipped_tenants=1` | `synced=621 skipped_tenants=0` |
| TRAST | skipped, *"no tenancy client is configured"* | `synced=9` |
| job | `Complete`, exit `0` | `Complete`, exit `0` |

The difference is exactly TRAST's nine. Both runs are in-cluster, from the real
CronJob definitions. `vehicles-diff` reports
`entitled=9 local=9 agree=9 missing_local=0 extra_local=0`, exit 0.

**The failure path was confirmed by accident, and it is the most useful result.**
A run made before identity-api was port-forwarded exited **1**, logged at error
level, and named the tenant — proving both that the tenancy wire works (TRAST got
past the `Configured()` guard all the way to `fetch operator privileged
vehicles`, so `TenantDetail`, `Entitlements` and `DimoToken` all succeeded) and
that a genuine skip now fails the run.

**The chart values were confirmed the same way, by a genuine failure.** See the
deploy-mechanics section below for why prod briefly ran the new chart against
the old binary; the upshot is that `vehicles-diff` fired against an image with no
such subcommand, exited 2, the job went `Failed`, **exactly one pod was created**
— `backoffLimit: 0` holding, no retry — and it persisted on the three-day TTL
rather than being collected within the hour. Nothing about that needed
contriving, and it is the property the incident lacked.

Note the `sync-vehicles` runs wrote to prod — idempotently, the same operation
the 03:00 cron performs, and `vehicles-diff` was clean afterwards.

### Merging does not deploy this app

Worth internalising before the next change that touches code and chart together.

`values.yaml` is bumped by `buildpushdev` on merge to main, but **prod's image
tag lives in `values-prod.yaml`** and only moves when a **`v*` tag** is pushed
(`.github/workflows/buildpushprod.yml`). `cronJobs` has no prod override, so
merging fleet-lite-app#134 shipped the *chart* to prod immediately while the
*binary* stayed on the previous release. That left a `vehicles-diff` CronJob
scheduled at 03:30 against an image that had never heard of the subcommand —
a nightly red job meaning nothing, which is corrosive to the exact signal this
work existed to create. Closed by releasing `v0.16.0`.

Two follow-ons:

- ArgoCD reporting `Synced/Healthy` is **not** evidence the code deployed. It
  was Synced at the version-bump revision the whole time the binary was stale.
  Check the image tag on the workload, not the app's sync status.
- The version-bump workflow round-trips `values.yaml` and **strips every comment
  in the file**. Two explanatory comments were gone one commit after landing.
  Durable rationale goes in `templates/cronjobs.yaml`, which it does not rewrite
  (fleet-lite-app#135).

### Running fleet-lite commands against prod from a laptop

Worth recording, because it took a few attempts and is how step 2 will be tested
too. The DB tunnel alone is not enough: `TENANCY_API_URL` and
`IDENTITY_API_ENDPOINT` are both `.svc.cluster.local` and need port-forwards.

```sh
ssh dimo-database-prod                                              # LocalForward 5430
kubectl -n prod port-forward svc/fleet-tenancy-api 18084:8084
kubectl -n prod port-forward svc/identity-api-prod 18080:8080
```

Build a `settings.yaml` from `configmap/fleet-lite-app-config` +
`secret/fleet-lite-app-secret`, then override three values: `DB.HOST/PORT` to
`localhost:5430`, `TENANCY_API_URL` to `http://localhost:18084`,
`IDENTITY_API_ENDPOINT` to `http://localhost:18080/query`. Two traps: the `DB_*`
env keys collapse into a **nested `DB:` block** (`db.Settings` yaml tags are
`USER`/`PASSWORD`/`HOST`/`PORT`/`NAME`/`SSL_MODE`), and ints and bools must be
emitted unquoted or `LoadConfig` refuses the file.

### A correction to make once, not repeatedly

`invitations-diff` **does not exist.** It was deleted in fleet-lite-app `7ed9bd8`
when P4 retired the local invitation path — there is no local side left to diff.
Plans 06 and 07 both cited it as a live sibling to copy; both are now corrected
to name `tenancy-diff` and `groups-diff`, which are real and both live in
fleet-lite-app. Earlier references in this file and in plan 04 are left alone:
they record runs from when the command did exist, and are accurate history.

### What is NOT started

Steps 2–5 of plan 07 — the freshness fix, the roster table here, the reader
cutover, and shrinking `vins`. Plan 06 (signer-key consolidation) is also
unstarted; its step 1 is read-only and cheap.

Step 2 is the one that closes the incident properly: step 1 makes the refresh
trustworthy and its failures loud, but the *cached-set-under-live-gates* mixing
that turned a stale cache into a **zero**-vehicle fleet is still there. Note its
own warning — a token in the resolved set with no local metadata row must still
appear, and that case needs a test or the bug just moves somewhere harder to see.

**Nothing about vehicle sharing has been exercised yet** — still no UserOp ever
sent. Unchanged by this session.

---

## Session handoff, 2026-08-20 ~02:00 UTC — step 3 pre-deploy, kept for the coverage analysis

Plan 07 steps 1–3 are done. **Two things are waiting on a human**, both
deliberate:

1. **Cut `v0.17.0` in fleet-lite-app** to release step 2. It is merged and
   verified but prod still runs `v0.16.0`, so every operator-managed customer's
   vehicle list is still resolved the old way.
2. **Run `scripts/roster-diagnostic.sh` once and read it**, then deploy step 3
   and run `reconcile-vehicles -dry-run` before letting the CronJob write.

### Where each step stands

| | state |
|---|---|
| 1. Trustworthy refresh, loud failures | released `v0.16.0`, verified in prod |
| 2. Stop the freshness mixing | fleet-lite-app#136 merged, verified, **not released** |
| 3. Stand up the roster | built here, **not deployed** |
| 4. Cut the readers over | not started |
| 5. Shrink `vins` | not started |

### Step 3, as built

`vehicles` keyed by `vehicle_token_id`, `vehicle_owner_changes` beside it, a
`reconcile-vehicles` command, and a 04:00 CronJob with `backoffLimit: 0` and a
three-day TTL.

**The population is the union of privileged sets over every licence in
`tenant_credentials`**, not `vehicle_entitlements`. Entitlements cover
explicit-mode tenants only and would have left out the 178 self-serve vehicles —
a roster with a permanent hole, which is what disqualified kaufmann. Sweeping
licences also self-heals as tenants come and go and needs no cross-database
access.

Read `roster.go`'s comments before changing any of it; the three rules that are
easy to "simplify" and must not be are owner-is-re-read-and-logged,
VIN/plate-fill-forward-never-clear, and a-partial-sweep-marks-nothing-unseen.

### The diagnostic has been run — 2026-08-19 ~22:00 UTC

You do not need to run it again unless you want the numbers refreshed. It
confirmed the plan on live data:

```
contradictions: 3     192379, 192400, 192401
                      kaufmann=0xda13fe28…  fleet-lite/chain=0x97b8ba44…
kaufmann-only : 27    exactly as documented
fleet-lite-only: 179  documented as 178 — one new self-serve vehicle since
```

Treat the contradiction count as the assertion and the population counts as
context: 3 becoming 4 is a finding, 179 becoming 180 is a Tuesday.

**And the roster corrects them.** A full prod-scale reconcile — prod's ten real
licences against prod identity-api, writing to a LOCAL database, nothing written
to prod — produced all three T60s reading `0x97B8bA44…`, the chain's answer. A
second run reported `inserted=0 updated=619 owner_changes=0`, so the steady
state is quiet and a real transfer will not hide in noise.

### Coverage, measured — and the one honest gap

| | |
|---|---|
| roster | **619** |
| union of `vins` + `fleets_lite.vehicles` | 655 |
| in the union, not the roster | **45** (26 kaufmann-only, 16 fleet-lite-only, 3 in both) |

The 45 are vehicles **no licence we hold is privileged on** — the plan's own
words for the kaufmann-only 27 are "onboarded, not (or no longer) in any synced
fleet". Different in kind from kaufmann's hole, which was 178 vehicles a
customer was actively using. **None of the 45 is entitled to anybody**, so no
customer is affected.

Bounded, not closed, and deliberately: identity-api answers `vehicle(tokenId:)`
without privilege, so each of the 45 is *reachable* — what is missing is a way
to *learn* their ids, since only kaufmann's table names them and this service
cannot read that schema. If it matters later, the fix is kaufmann publishing its
onboarded token ids, not this service reaching across a schema boundary.

**What is guaranteed:** an active entitlement's vehicle is always in the roster.
The reconcile fills any entitled token the sweep cannot enumerate via a single
lookup — because once readers cut over in step 4, an entitled vehicle missing
from the roster IS the empty-fleet incident again, one layer down.

### Still to do before the cron writes in prod

```sh
# once step 3 is deployed, a dry run first — it computes everything, writes nothing
kubectl -n prod create job --from=cronjob/fleet-tenancy-api-reconcile-vehicles \
  reconcile-dryrun-$(date +%s)      # then edit the job to add -dry-run, or run by hand
```

### What was verified, and what was not

Verified: the migration applies and reverses cleanly; nine roster tests run
against a real postgres; the identity-api query was run against **prod** through
the gateway and returned **553 vehicles over six pages**, every one with owner,
`mintedAt` and definition parsed, no repeated token ids, empty client id
refused. 553 matches what fleet-lite's sync reports for that licence, so the
pagination is complete rather than truncated.

Not verified: `reconcile-vehicles` has never WRITTEN to the prod database. It
has been run at full prod scale against prod's real licences and prod
identity-api, but writing to a local postgres — which exercises everything
except the prod write itself. That is why the dry run above is still a step.

### A trap that bit twice today

`cronjobs.yaml` in **this** chart used Helm's `default` for numeric overrides,
so `backoffLimit: 0` was silently replaced by 1. fleet-lite's chart had already
been fixed for exactly this; this one had not. Ported the `hasKey` form and the
reasoning.

Related, and the reason chart rationale keeps moving into templates: the
version-bump workflow round-trips `values.yaml` and **strips every comment**.

### Still true, unchanged by this session

**Nothing about vehicle sharing has been exercised** — still no UserOp ever
sent. Plan 06 (signer-key consolidation) is unstarted; its step 1 is read-only
and cheap.

---

## Session handoff, 2026-08-20 ~02:15 UTC (superseded — see below)

**Plan 07 steps 1, 2 and 3 are done, released and running in production.**
Nothing is half-finished and nothing is waiting on a human. Start with **step 4
— cut the readers over**, or with plan 06 step 1, which is read-only and cheap.

### State of the world

| | released | in prod |
|---|---|---|
| 1. Trustworthy refresh, loud failures | fleet-lite-app `v0.16.0` | sync-vehicles syncs every tenant, fails loudly on a skip |
| 2. Stop the freshness mixing | fleet-lite-app `v0.17.0` | vehicle set resolved from tenancy, metadata joined |
| 3. Stand up the roster | fleet-tenancy-api `v0.15.0` | `vehicles` holds 619 rows, reconciling nightly at 04:00 |
| 4. Cut the readers over | — | not started |
| 5. Shrink `vins` | — | not started |

The incident is closed at every layer it had: the sync runs, its failures are
loud, the set and its gates share one freshness, and the chain's answer about
ownership is held once and re-read.

### What is true in prod right now

- `fleet_tenancy_api.vehicles`: 619 rows, all with owner, `minted_at` and
  definition. `unseen_since` null throughout. `vehicle_owner_changes` holds 619
  first observations and 0 transfers.
- **192379, 192400, 192401 read `0x97B8bA44…`** — the chain's answer.
  `kaufmann_oracle.vins` still says `0xDA13fE28…` and was deliberately not
  touched. Step 5 drops that column once nothing reads it.
- `reconcile-vehicles` is on `0 4 * * *`, `backoffLimit: 0`, three-day TTL.
  Two consecutive prod runs gave `inserted=619` then `inserted=0 updated=619
  owner_changes=0`, so the steady state is quiet.
- **Nothing reads the roster yet.** That is step 4.

### Step 4 — read this before starting

The plan says to do it behind a flag per reader, as `GROUPS_FROM_TENANCY` was.
**Take that seriously**: step 2 shipped without a flag by explicit decision, and
its revert path is a release. Step 4 is strictly larger — it makes this service
load-bearing for every fleet page render — so the flag is the difference between
a config flip and an incident.

Three things already learned that step 4 inherits:

1. **The set and every gate over it must age together.** Step 2's entitled read
   is cached at 60s because the membership gate and group index are; a live set
   against stale gates is the same mixing with the staleness on the other foot.
   `entitledTTL` is asserted equal to `membershipTTL` in a test. Any new leg of
   that intersection must join the same TTL.
2. **A token in the resolved set with no metadata must still appear.** Step 2
   guarantees this with `MetadataPending`; step 4 must keep it when metadata
   moves to the roster. An inner join gives a provably correct set and a short
   response — the same bug, harder to see.
3. **An entitled vehicle is always in the roster**, guaranteed by the
   individual-lookup fill in `reconcile-vehicles`. It reported
   `entitled_filled=0` in prod because all nine entitlements are covered by the
   licence sweep — it is insurance for exactly this step.

### The known, bounded gap

The roster holds 619 of the 655 tokens the two source tables hold between them.
The 45 it misses are vehicles **no licence we hold is privileged on** — the
plan's own words for the kaufmann-only 27 are "onboarded, not in any synced
fleet". None is entitled to anybody, so no customer is affected.

Bounded rather than closed, deliberately: identity-api answers
`vehicle(tokenId:)` without privilege, so each of the 45 is *reachable* — what
is missing is a way to *learn* their ids, since only kaufmann's table names them
and this service cannot read that schema. If it ever matters, the fix is
kaufmann publishing its onboarded token ids, not this service reaching across a
schema boundary.

### Traps this repo has now paid for twice — do not relearn them

- **Merging does not deploy.** `values.yaml` is bumped by `buildpushdev` on
  merge, but prod's tag lives in `values-prod.yaml` and only moves on a **`v*`
  tag**. A change touching code *and* chart ships the chart immediately and the
  code not at all. That happened on 2026-08-19 and left a CronJob firing against
  an image that had never heard of its subcommand.
- **ArgoCD `Synced/Healthy` is not evidence the code deployed.** It was Synced
  at the version-bump revision the whole time the binary was stale. Check the
  image tag on the workload.
- **The version-bump workflow strips every comment from `values.yaml`.**
  Durable rationale goes in `templates/cronjobs.yaml`.
- **Helm's `default` treats 0 as empty**, so `backoffLimit: 0` is silently
  replaced by 1. Both charts now use `hasKey`. fleet-lite's was fixed first;
  this one still had the bug a day later.
- **The prod DB tunnel's cloudflared cert lasts four minutes** and re-auth is
  interactive. Plan a session around that, or ask the operator to open it.

### Running things against prod from a laptop

The DB tunnel alone is not enough — `TENANCY_API_URL` and
`IDENTITY_API_ENDPOINT` are both `.svc.cluster.local`:

```sh
ssh dimo-database-prod                                        # LocalForward 5430
kubectl -n prod port-forward svc/fleet-tenancy-api 18084:8084
kubectl -n prod port-forward svc/identity-api-prod 18080:8080
```

Build `settings.yaml` from the service's configmap + secret, then override the
three hostnames. Two traps: `DB_*` env keys collapse into a nested `DB:` block
(`USER`/`PASSWORD`/`HOST`/`PORT`/`NAME`/`SSL_MODE`), and ints and bools must be
unquoted or `LoadConfig` refuses the file. Per-service DB users are
schema-scoped — take each service's own credentials from its own secret.

`scripts/roster-diagnostic.sh` wraps the cross-service comparison and writes
nothing; it was last run 2026-08-19 22:00 UTC and reported 3 / 27 / 179.

### Still true, unchanged

**Nothing about vehicle sharing has been exercised** — still no UserOp ever
sent. Plan 06 (signer-key consolidation) is unstarted; its step 1 is read-only
and cheap.


## PICK UP HERE — session handoff, 2026-08-20 ~04:30 UTC

**The step 4 endpoint is released and running in prod as `v0.16.0`. Nothing
calls it.** Next action is fleet-lite's cutover behind a flag — the endpoint it
needs now exists.

*(This service is now on `v0.17.0` — same endpoint, plus the mint retry below.
fleet-lite is on `v0.18.0`.)*

Deployed and checked: prod `/version` reports `fd2a692`, the merge commit, on
two fresh pods, and `values.yaml` and `values-prod.yaml` both carry that tag —
the merge-does-not-deploy trap does not apply here, because this change has no
chart half. Note that probing the route over HTTP proves nothing either way: the
`/v1` trusted-caller guard answers 401 before routing, so an unknown path and a
real one are indistinguishable from outside. The evidence that the endpoint is
there is the commit `/version` names, plus its route test in that commit.

### What this session did

`POST /v1/tenants/{tenantId}/vehicle-metadata` — the read side of step 3's
roster, which step 4 refers to as "step 3's endpoint" and which did not exist.
Body `{"tokenIds": [...]}`, response `{"vehicles": [...]}` carrying owner,
definition, make/model/year, `minted_at`, VIN, plate, `reconciled_at` and
`unseen_since`.

- `internal/models/vehicle.go`, `internal/service/roster_read.go`,
  `internal/controllers/roster.go`, route in `internal/app/app.go`.
- Tests: seven service tests against a real database, two route tests, one
  controller unit test. `make lint` and `go test ./...` clean.
- **No migration, no chart change, no new setting.** So the release is a tag and
  nothing else — and specifically NOT a case of the merge-does-not-deploy trap
  below, because there is no chart half to ship ahead of the binary.

### The decision inside it, which the plan did not make

**The endpoint does metadata only. It does not resolve the set.** The plan's
sentence — "fleet-lite's vehicle list resolves set *and* metadata from step 3's
endpoint" — reads as one call doing both. It should not, and this is worth
holding onto rather than rediscovering:

fleet-lite already resolves the set from this service, as of step 2 in
`v0.17.0`: entitled ∩ active memberships ∩ group scope, three endpoints, with
`entitledTTL == membershipTTL` asserted in a test. A resolving endpoint here
would be a **second implementation of that intersection**, and the two would
diverge only for members whose group scope had just changed — silently, and in
the one direction that matters. So step 4 swaps fleet-lite's *metadata source*
and leaves its set resolution untouched. Smaller change, cleaner flag revert.

Three properties that are tests rather than intentions:

1. **A token with no roster row is absent from the response, not an error.**
   Absence means the roster has not seen it yet — entitled minutes ago, before
   the 04:00 reconcile. The caller must keep its left join and its
   `MetadataPending`; treating absence as exclusion is the empty-fleet incident
   one layer down.
2. **POST, not GET.** 619 token ids in a query string is refused by fiber while
   reading the request line, before any handler or gate runs, with an error
   naming the read buffer rather than the URL. Asserted, because "make it a GET,
   it's a read" is the obvious review note.
3. **The tenant in the path authorizes the caller and is not a per-vehicle
   filter.** The roster is keyed by token id alone by design — owner and
   definition are properties of the vehicle — so there is no tenant column to
   filter on. What bounds it is that the caller must *name* the tokens: no
   listing, no wildcard, no cursor. Read that sentence before adding any
   "list all" convenience to this controller.

### Step 4, what remains

1. ~~Release this (`v*` tag).~~ Done — `v0.16.0`, 2026-08-20, zero behaviour
   change because nothing calls it.
2. **fleet-lite behind a flag** (`VEHICLE_METADATA_FROM_TENANCY`, or whatever
   name — the point is the flag). Its `listResolvedVehicles` keeps
   `resolveTokenSet` exactly as is and swaps the `dbmodels.Vehicles` query for
   this call; `mergeResolvedVehicles` stays, since the left join is the same
   left join. Favourites, geofences, TCO and `last_lat/lon/seen` stay local —
   they are app-local columns and are not in the roster.
3. **Measure p99 on `/v1/authz` before and after**, as the plan asks. This
   service is already what both apps fail closed on and now also holds River and
   a gas-spending path; making it load-bearing for every fleet page render is
   the real risk in step 4, not the correctness of the join.
4. Kaufmann's b2b-facing vehicle reads follow, same shape.
5. Then `fleets_lite.vehicles` narrows to app-local columns, and step 5 drops
   `owner` / `minted_at` from `kaufmann_oracle.vins`.

### The DIMO login challenge is flaky — found 2026-08-20, fixed in both repos

Worth knowing before it is diagnosed a third time as a bad credential.

`fleet-lite-app-groups-diff` failed at 06:45 with `submit_challenge` 400
**"Could not verify signature"** for tenant `e0cd30da`. That reads exactly like
a wrong or rotated key. It is not:

- identity-api says that licence has one signer, `0xEb4Fa156…`, enabled
  `2026-07-29T17:17:15Z` and unchanged since — the tenant's own creation time.
- A re-run **minted that tenant fine and failed on a different one**.
- Three runs after that were clean: 6 tenants, 88 groups, `differ=0
  missing_remote=0`.

So the keys are right, the group mirror is healthy, and the challenge flow is
unreliable at roughly **one attempt in fourteen**.

**Why a retry is the fix rather than a cover-up:** the challenge is SINGLE-USE.
`shared/pkg/dimoauth` already retries the two HTTP calls individually
(`shttp.WithRetry(3)`), which cannot help and may be the cause — re-submitting a
consumed or unknown `state` is precisely what "could not verify signature" looks
like from outside. Only a fresh challenge can succeed, and `GetToken` starts one
on every call. Both repos now retry three times, 250ms apart, and still return
nil after the last, so a genuinely wrong credential fails in about half a second
instead of hanging.

Released as `v0.17.0` here and `v0.18.0` in fleet-lite-app, both deployed and
verified: a groups-diff run against prod on the fixed image returned
`tenants=6 groups=88 agree=83 differ=0 missing_remote=0 unreachable=0`, exit 0.

Here that is `mintWithRetry` in `internal/service/mint_retry.go`, wired into
`DeveloperJWT` (the `/v1/tenants/{id}/dimo-token` minter fleet-lite calls for
managed tenants) and `ValidateCredential` — the second because rejecting a valid
key a human has just pasted tells them their key is broken when it is not.

**A second, unrelated fault the same investigation surfaced:** `groups-diff`
returned on the first tenant it could not reach. The walk is ordered by tenant
name, so a flake took every tenant after it down too, and from the exit code a
run that verified five tenants and one that verified one were identical. Fixed
in fleet-lite-app#137 — unreachable tenants are collected, named, counted, and
still fail the run.

**On retiring jobs.** `mirror-groups` and `groups-diff`, in both caller repos,
are scaffolding for the groups move: they keep and verify local mirrors that P5
deletes. They are not dead weight yet — they are the safety net for a migration
that is not finished — but they retire with the local group tables, and nothing
else should be built on them.

### Everything in the previous handoff is still true

The roster is 619 rows reconciling nightly at 04:00, the three T60s read the
chain's answer here and kaufmann's wrong one there, the 45-vehicle gap is
bounded and affects nobody, and **no vehicle share has ever been sent**. Plan 06
step 1 remains read-only and cheap if you want something smaller than a cutover.

The traps list above is unchanged and still the thing to read before deploying:
merging does not deploy, `Synced/Healthy` is not evidence, the version-bump
workflow eats comments in `values.yaml`, Helm's `default` eats `0`, and the prod
DB tunnel's cert lasts four minutes.
