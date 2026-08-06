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

### 1. Flip `DROP_FOREIGN_TENANT_GROUPS` — now unblocked, still NOT DONE

The republish it was gated on is done and verified (below). Per
`07-r1-group-id-migration.md` §6 this must not ship in the same release as the
republish, and the republish has now landed in its own release, so the gate is
satisfied.

The cronjob meshing bug that was listed here second is fixed — see "Done on
2026-08-06" below.

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
| Merged + deployed | `kaufmann-oracle#185` (R1) — `v1.34.0` |
| Merged | design docs in all three repos |
| This repo | schema, `/v1/authz`, backfill. Builds and tests clean |

`fleet-lite-app#98` and various dependabot PRs are unrelated.

**R1 is now complete through step 5.** The republish has run and been verified,
so `DROP_FOREIGN_TENANT_GROUPS` is unblocked — it is still `false`, and flipping
it is the next action. The trap below explains what it protects against; read it
before flipping.

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
2. `/v1/resolve/client-id/{clientId}` + the developer-license middleware —
   **`/v1` is currently unauthenticated**, fine locally, must land before deploy
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
