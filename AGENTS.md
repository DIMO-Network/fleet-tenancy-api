# Agent Guidelines — fleet-tenancy-api

**Status: live in prod and load-bearing.** Both callers authorize every
request against `GET /v1/authz` (cutover 2026-08-11) and both write
memberships here (2026-08-12). Since 2026-08-13 this service also **owns
fleet groups**: both apps write groups here, read them from here behind
`GROUPS_FROM_TENANCY`, and their attestation import/reconcile machinery is
deleted — this service is the single publisher of
`dimo.document.vehicle.groups`. The operator console (b2b, via kaufmann's
proxies), the token minter and on-behalf provisioning are also live.

`/user/v1` remains unbuilt. What is left of the groups move is P5 — retiring
the callers' local group tables — see `docs/HANDOFF.md`.

Since 2026-08-19 this service is also an **on-chain writer**: vehicle sharing
grants SACD permissions by sending a UserOperation from the vehicle owner's
kernel account, signed with the tenant's signer key. The first production share
landed on chain 2026-08-22 — see `docs/HANDOFF.md` for the transaction and for
the two-byte secret that blocked the first attempt. Revocation (a zeroed SACD
record, not a deletion) is built but not yet exercised on chain.
It brings River, a bundler connection and a code path that spends gas into the
same process that serves `/v1/authz`, which is why its queue is small and its
settings are all-or-nothing.

**Consequence worth internalising: an outage here is an outage there.** Both
apps fail closed on `/v1/authz` (503, deliberately not 403), and group writes
fail the request rather than reporting a success the owner never saw.

## What this service is

The source of truth for **tenants, users, memberships, delegations and vehicle
entitlements** across DIMO's fleet products. Its hot path is one question:

> *What may this wallet do in this tenant?* → `GET /v1/authz`

Clients: `b2b-fleet-mgr-app` (operator console), `fleet-lite-app` (end-customer
product), `kaufmann-oracle`.

It is **not** a DIMO platform service. `accounts` / `profiles-api` / `users-api`
own DIMO end-user accounts and wallets. This owns *application tenancy* and is a
consumer of accounts-api.

## Vocabulary

One word throughout: **tenant**. Table `tenants`, parent link
`parent_tenant_id`, wire header `Tenant-Id`, `kind` carrying
`operator` | `customer`. Don't reintroduce "organization" — it was an
earlier draft's disambiguation device and was renamed away.

## Things to get right

- **`permissions` is authoritative, `role` is a label.** Every authorization
  check reads `permissions`. `role` exists for display and as a preset when
  adding a member. Never gate on `role`.
- **Group scope lives in `scope_group_ids` only** (NULL = unrestricted). There is
  deliberately no `view_all_fleets` capability — it would encode the same fact
  twice with no defined resolution when the two disagree.
- **Credentials never leave the service.** Callers get a minted DIMO developer
  JWT, not the key. Don't add an endpoint that returns plaintext. The signer
  key is decrypted for the duration of one share and goes nowhere else.
- **Sharing only works where the effective credential has a signer.** Operators
  have one from the backfill and managed customers inherit it; self-serve
  tenants have none, so sharing is off for them. Fails closed, and is not an
  oversight to patch quietly — see `docs/HANDOFF.md`.
- **`Settings.Validate` refuses to boot on an empty `TENANT_SECRET_ENC_KEY`
  outside local.** `sha256("")` is a valid AES-256 key, so an unset key encrypts
  successfully with a constant anyone can compute — silent and wrong. This
  reached production in fleet-lite-app; the check exists so it can't happen here.
  Don't weaken it.
- **A delegation is management only.** It never grants a fleet-lite session —
  operator staff are b2b-only and there is no impersonation.
- **Entitlement rows exist only for `entitlement_mode = 'explicit'`.** Operator
  and self-serve tenants resolve their fleet from the license's privileged set.

## Conventions

Matches `fleet-lite-app` and `kaufmann-oracle` so code is portable:
Go + Fiber v2 + zerolog, goose migrations in `internal/db/migrations`, sqlboiler
models, `stretchr/testify`, `go.uber.org/mock`, testcontainers-go for
DB-dependent tests. Timestamps are `created_at` / `updated_at`
`TIMESTAMPTZ NOT NULL DEFAULT NOW()`. Users are identified by wallet, not user id.

## Start here

[`docs/HANDOFF.md`](docs/HANDOFF.md) — current state, what to do next, and the
traps that have already caused one production incident. Read it before touching
anything.

## Design docs

[`docs/operator-tenancy/`](docs/operator-tenancy/) — the full design set, nine
locked decisions, published 2026-08-12. Read them before changing the schema.

They were held back while they documented two unfixed weaknesses (the
fleet-group id collision and fleet-lite's tenant-credential encryption). Both
are fixed and deployed, so the reason expired. An identical copy has been
public in `fleet-lite-app` since that repo's `#96` regardless.
