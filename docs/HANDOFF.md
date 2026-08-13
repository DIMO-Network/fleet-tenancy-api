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

**P5 remains:** move the scope-filtering SQL and the vehicle/report group
joins onto tenancy-backed token-id sets, then drop the local tables, the
mirrors, the mirror-groups crons and the GROUPS_FROM_TENANCY flags, and add
the deferred FKs from `scope_group_ids` / `source_group_id` (array columns
need a trigger or check, not a plain FK — decide then). Also delete
`SyncVehicleGroups` with its frontend caller, and fleet-lite's
`vehicles.groups_updated_at` / `last_group_sync_at` columns.

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
