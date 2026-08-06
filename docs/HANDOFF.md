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

### 1. Clear Conexo2's DIMO client id — NOT DONE

`Conexo` and `Conexo2` in `kaufmann_oracle.tenants` share client id
`0x6A1C063751415231C9A41C64aEEd8FD061bc9807`. Conexo has 2 vehicles and 2
members; **Conexo2 has none**. The user approved clearing Conexo2's; the SSH
tunnel dropped before it ran.

```sql
UPDATE kaufmann_oracle.tenants SET dimo_client_id = NULL, updated_at = NOW()
 WHERE name = 'Conexo2'
   AND dimo_client_id = '0x6A1C063751415231C9A41C64aEEd8FD061bc9807';
```

Verify exactly one row changes. The encrypted secret is left in place — it is
inert without a client id, and removing it is a bigger change than was asked
for.

This is not only a migration blocker. `tenant_credentials.dimo_client_id` is
uniquely indexed here, and the duplicate is a **live ambiguity** in kaufmann's
public `/api/v1`: its resolver takes `qm.Limit(1)` with a comment that
duplicates "shouldn't happen, but the data model allows it". They do.

### 2. Merge and deploy the R1 PRs

- `fleet-lite-app#99` — uuid unification + tenant-scoped group ids
- `kaufmann-oracle#185` — tenant-scoped group ids

**#99 carries a production data migration** (re-keys the Kaufmann tenant) and
deserves a close read. Until it deploys, the backfill sees fleet-lite's Kaufmann
as unmatched and would migrate it as a *self-serve* tenant — wrong. Deploy #99
before any real backfill run.

## Current state

| | |
|---|---|
| Merged + deployed | `fleet-lite-app#97` — encryption fix; prod re-encrypted, verified |
| Merged | design docs in all three repos |
| **Open** | `fleet-lite-app#99`, `kaufmann-oracle#185` |
| This repo | schema, `/v1/authz`, backfill. Builds and tests clean |

`fleet-lite-app#98` and various dependabot PRs are unrelated.

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

- 16 tenants: 11 in kaufmann, 5 in fleet-lite, **one overlap** ("Kaufmann",
  different uuids — `7be1ab9e…` kept, `9708b213…` re-keyed by #99)
- fleet-lite: 576 vehicles, 82 groups, 378 memberships, 10 members
- Kaufmann is 524 of those vehicles; the other four tenants hold 52 between
  them and are **real** — "My Test Fleet" logged in the same day with 40
  vehicles. They migrate as **self-serve** tenants (no parent, own credentials,
  implicit entitlements)
- All three encryption keys differ, so the backfill decrypts per source and
  re-encrypts. All 11 kaufmann credentials decrypt cleanly with the real key

## Next, in order

1. Conexo2 (above), then a clean `backfill -dry-run` against prod
2. Deploy #99 and #185; run fleet-lite's group-attestation republish
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

## Outstanding decisions

- **Rotate the five DIMO developer licenses?** Re-encryption protects them going
  forward but does not undo prior exposure under the known key.
- **Delete the `AllowLegacyEmptyEncKey` shim** in fleet-lite — prod is through
  the re-encryption, so it is now dead weight that keeps the weak key readable.
- **Publish the design docs** — gitignored here pending the group-id and
  encryption fixes. The encryption one is done; R1 is not deployed.
