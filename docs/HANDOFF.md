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

### 1. A clean `backfill -dry-run` against prod — NOT DONE

Both blockers below are cleared, so the dry-run is now the next real step. It
was not run on 2026-08-06 because the SSH tunnel dropped again mid-session.

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

**Still outstanding from R1:** fleet-lite's group-attestation republish has not
been run. Until it is, `DROP_FOREIGN_TENANT_GROUPS` must stay `false` — see the
trap below, which is unchanged by this deploy.

## Traps — things that will bite you

**`DROP_FOREIGN_TENANT_GROUPS` must stay `false`.** Enabling it before
fleet-lite republishes its own group attestations deletes 370 of 378 group
memberships. 0 of 287 grouped vehicles have ever been edited locally, so
fleet-lite has never published a group CE — the entire production group
structure is kaufmann's assertions, and `removalAllowed` is open on all 287.

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

1. A clean `backfill -dry-run` against prod
2. Run fleet-lite's group-attestation republish, then flip
   `DROP_FOREIGN_TENANT_GROUPS` — in that order, never the reverse
3. `/v1/resolve/client-id/{clientId}` + the developer-license middleware —
   **`/v1` is currently unauthenticated**, fine locally, must land before deploy
4. The DIMO token minter (`GET /v1/tenants/{id}/dimo-token`), so credentials
   never leave this service
5. `/user/v1` management surface, then the b2b operator console

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
