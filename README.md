# fleet-tenancy-api

Shared tenancy service for DIMO's fleet products. It owns **tenants**, **users**,
**memberships**, **delegations** and **vehicle entitlements**, and answers one
hot question for every caller:

> *What may this wallet do in this tenant?*

`b2b-fleet-mgr-app` (operator console) and `fleet-lite-app` (end-customer
product) are the clients; `kaufmann-oracle` gets its authorization answers here
too. Today each of those keeps its own tenant table, its own membership model
and its own copy of the same AES-GCM credential encryption — this service
replaces the duplication with one source of truth and adds the operator →
customer hierarchy the duplication can't express.

It also **mints DIMO developer JWTs**, so developer-license private keys never
leave the service.

> **Status: scaffold.** The schema and the service contract are designed; the
> handlers are not built yet. See `docs/` (held back — see below).

## Not a DIMO platform service

`accounts` / `profiles-api` / `users-api` deal with DIMO *end-user accounts and
wallets*. This deals with *application tenancy for our fleet products* — which
tenants exist, who belongs to them, and which vehicles they may see. It is a
consumer of accounts-api, not a replacement for it.

## Stack

Go · Fiber v2 · zerolog · goose migrations · sqlboiler · Postgres, matching the
layout used by `fleet-lite-app` and `kaufmann-oracle` so code is portable
between them.

```
cmd/fleet-tenancy-api/   entrypoint; doubles as a CLI (google/subcommands)
internal/app/            fiber wiring, middleware, routes
internal/config/         settings
internal/db/migrations/  goose migrations
charts/                  Helm chart
```

## Local dev

```sh
cp settings.sample.yaml settings.yaml   # edit as needed
make migrate                            # goose up
make run
```

Needs Postgres running locally.

## Design docs

The design set (current state, target architecture, service spec, phased
migration, risks) is **not yet in this repo**. It documents two unresolved
weaknesses in the systems being replaced, so it is gitignored until those are
fixed. Until then it lives at `fleet-lite-app/docs/operator-tenancy/`.

Remove the `/docs/operator-tenancy/` block from `.gitignore` to publish it.
