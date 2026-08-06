# Agent Guidelines — fleet-tenancy-api

**Status: scaffold.** Health and version only. The schema is designed and
migrated; the handlers are not built.

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
  JWT, not the key. Don't add an endpoint that returns plaintext.
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

Gitignored for now — they document unresolved weaknesses in the systems being
replaced. They live at `fleet-lite-app/docs/operator-tenancy/` until those are
fixed. Read them before changing the schema.
