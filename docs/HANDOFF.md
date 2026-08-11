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
2. **Cutover, one call site at a time — now unblocked.** `fleet-lite`'s
   `NewTenantMiddleware` and kaufmann's `NewAccessMiddleware` are the two edge
   checks `/v1/authz` replaces. Both read their own tables today, both have a
   client ready, and the reconciliation above says the data agrees. Do it behind
   a flag defaulting off, flip one app at a time, and re-run `tenancy-diff`
   first — it is the cheapest possible pre-flight. Note the merge means cutover
   *widens* access for 8 Kaufmann-tenant memberships; that is the intended
   consequence of unifying the two systems, but it should be a decision someone
   makes knowingly rather than discovers
3. The DIMO token minter (`GET /v1/tenants/{id}/dimo-token`), so credentials
   never leave this service
4. `/user/v1` management surface, then the b2b operator console — which is also
   when b2b's own identity question stops being deferrable

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
