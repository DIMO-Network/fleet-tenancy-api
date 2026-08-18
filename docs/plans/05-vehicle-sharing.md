# Vehicle sharing — SACD grant to a wallet, server-signed

Status: **planned, not started**. Written 2026-08-18.

Fleet-lite gets a **Share** button on the vehicle list view: enter a 0x wallet
address, pick a duration, and the grantee receives an on-chain **SACD
permission grant** on the vehicle NFT. No ownership change, no wallet prompt —
the transaction is signed server-side, in this service.

This is the plan for the whole slice: the transaction machinery lands here, the
product surface lands in `fleet-lite-app` (its side is detailed in
[`fleet-lite-app/docs/VEHICLE_SHARING_PLAN.md`](../../../fleet-lite-app/docs/VEHICLE_SHARING_PLAN.md),
which defers to this doc on everything backend).

## Decisions taken 2026-08-18

| Decision | Choice | Why |
|---|---|---|
| What "share" means | SACD `setPermissions0` grant (grantee wallet, permission bitmask, expiration) | Matches DIMO's sharing vocabulary and the mobile app. Transfer is a different, existing b2b flow. |
| Who signs (v1) | The **operator's signer key**, on the owner's kernel account, server-side | Most fleet-lite customers got their vehicles via kaufmann's email-transfer flow, so their kernel accounts registered the operator signer at creation. No frontend web3 at all in v1. |
| Who signs (later) | Browser passkey (Turnkey/ZeroDev), for owners whose accounts weren't oracle-created | Phase 2. The signing code will be extracted from `b2b-fleet-mgr-app/web` and **published as an npm package from that repo**; fleet-lite consumes it. |
| Where the tx backend lives | **This service** | It already holds `tenant_credentials.signer_key_enc`, the accounts-api gateway, own-or-parent credential resolution, entitlements, and the `manage_vehicles` capability. A share is an authorization decision followed by one contract call — the decision half is all here already. |
| Job model | **River** (new dependency here) | Kaufmann's pattern: POST returns a `jobId`, a worker sends the UserOp, the client polls. Survives restarts and 10–60s bundler latency; a synchronous handler that times out leaves the tx state ambiguous. |
| v1 scope | Create a share + list existing shares | **Revoke is deferred.** Listing is read-only from identity-api and doesn't touch this service. |
| v1 UX | Fixed default permission set + duration picker | No per-permission checkboxes. Defaults mirror b2b's `defaultPermissions` (see traps). |

Considered and rejected:

- **Synchronous share endpoint (no queue).** `SendCall` waits for the UserOp
  receipt; bundler latency regularly exceeds sane HTTP timeouts, and a
  client-side timeout after the op was sent means the caller can't tell whether
  the grant landed. Rejected for the same reason kaufmann never did it.
- **A separate fleet-tx service.** Cleaner blast radius, but a whole deployable
  for one worker type, and it would still call back here for every
  authorization input (signer key, entitlement, capability). Revisit if tx
  volume or tx types grow; nothing in this plan precludes extraction later.
- **Keeping the endpoint in kaufmann-oracle and proxying fleet-lite to it.**
  Couples the customer product to the operator's oracle, and the oracle's
  tenancy tables are being retired — building new surface on them walks
  backwards.
- **Per-permission picker in v1.** SACD's bitmask is developer-facing; end
  customers get a sensible default. The bitmask is in the API from day one, so
  a picker is purely frontend work later.

## What exists to build on

**Already in this service:**

- `tenant_credentials.signer_address` / `signer_key_enc`, encrypted under
  `TENANT_SECRET_ENC_KEY`, resolved own-or-parent by
  `CredentialService.Effective` — the operator's signer reaches its managed
  customers exactly like the license does.
- `internal/gateway/accounts_api.go` — reads accounts-api (by-wallet lookup
  needs adding; today it is by-email only).
- Entitlement resolution (`internal/service/entitlements.go`) and the
  `manage_vehicles` capability, added precisely because kaufmann's shared
  routes lacked a per-member gate.

**To port from kaufmann-oracle (`internal/onboarding/transfer_shared.go` and
`internal/service/shared_signer.go`):**

- `buildSetPermissionsCall(...)` — packs `setPermissions0` calldata via
  `go-transactions/contracts/sacd.TryPackSetPermissions0`. Hand-packed because
  go-transactions' high-level `Client.SetPermissions` is broken (commented out,
  references a nonexistent function) — do not go looking for the helper.
- `fullSacdPermissions()` and the expiration convention (now + 40 years for
  "indefinite").
- The `fleetCaller` interface + `fleet.SendCall(ctx, kernel, signerPK, msg,
  waitForReceipt)` from `go-zerodev/fleet` — sends a UserOp from the owner's
  kernel account, signed by the operator key.
- `AssertTenantMaySignFor` — owner's `providedSignerAddress` (live, from
  accounts-api) must equal the effective credential's signer.

**New dependencies:** `riverqueue/river`, `DIMO-Network/go-transactions`,
`DIMO-Network/go-zerodev`.

**New settings** (mirror kaufmann's `internal/config/settings.go` names where
they exist): chain RPC URL, ZeroDev bundler + paymaster URLs, `SACD_ADDRESS`
(Polygon prod `0x3c152B5d96769661008Ff404224d6530FCAC766d` — a separate
contract from the registry), `VEHICLE_NFT_ADDRESS`, chain id. Follow kaufmann's
tx-client construction in `internal/onboarding/onboard.go`.

## The authorization chain

A share executes only when **all** of these hold. 1–2 are checked in
fleet-lite before it calls; 1–5 are checked here at submit; 3–5 are
**re-asserted in the worker** before the irreversible call, kaufmann-style.

1. Caller is a member of the tenant holding **`manage_vehicles`** (`/v1/authz`).
2. Surface check: fleet-lite session, acting tenant resolved.
3. **Entitlement**: `tokenId` is in the tenant's entitled fleet. Never build a
   contract call for a token id that didn't come from an entitlement-filtered
   query — this is the standing rule from the operator-tenancy design.
4. **Owner lookup** (identity-api): current owner of the vehicle NFT. Do not
   trust an owner sent by the caller — ownership can change between page load
   and click.
5. **Signing authority** (accounts-api, live): the owner account's
   `providedSignerAddress` equals the effective credential's
   `signer_address`. This is the ported `AssertTenantMaySignFor`. Policy
   denial → 403, infrastructure failure → 5xx; keep them distinct.

Plus grantee sanity: valid hex address, not the zero address, not the owner
itself.

## API surface (this service)

Behind the existing `/v1` trusted-caller guard + acting-tenant resolution:

```
POST /v1/tenants/{id}/vehicles/{tokenId}/share
     { "grantee": "0x…", "durationDays": 365, "wallet": "0x…" }   // wallet = acting member, for authz + audit
     → 202 { "jobId": "…" }

GET  /v1/tenants/{id}/vehicles/{tokenId}/share/status?jobId=…
     → { "state": "…", "isSuccessful": bool, "errors": [] }
```

- Status mirrors kaufmann's single-job `TransferJobStatus` shape (success is
  `isSuccessful`, **not** a per-VIN `"Success"` string — the two shapes coexist
  in kaufmann and confusing them is a known trap).
- `durationDays` omitted or 0 → "indefinite" = now + 40y, matching mint.
- Permissions in v1 are fixed server-side (see traps); the request schema
  gets an optional `permissions` bitmask field from day one so the phase-2
  picker is API-compatible, but v1 rejects values it doesn't expect.

River worker `vehicle_share`:

- `MaxAttempts: 1`, like kaufmann's transfer — an automatic retry of a
  possibly-landed on-chain call is worse than a reported failure.
- Re-asserts entitlement + owner + signing authority, builds the call, sends
  via `SendCall(owner, signerPK, msg, true)`, records receipt/failure on the
  job row for the status endpoint.
- Decrypted signer key exists only inside the worker invocation — same
  discipline as `CredentialService` (never logged, never returned, never
  stored decrypted).

### The display gate (`canShare`) — resolved live, decided 2026-08-18

Fleet-lite needs a per-vehicle "would server-signing work" flag to decide
whether to render the button. Add to this service:

```
GET /v1/tenants/{id}/shareable-owners      → { "owners": ["0x…", …] }
```

— the distinct entitled-vehicle owners whose accounts the effective signer may
sign for, **resolved live against accounts-api** (per-owner
`providedSignerAddress` lookup, short-TTL cache, ~minutes). Fleet-lite joins
this against its vehicles' `owner_address` to set `canShare` per vehicle.

**Do not read `users.shared_account_signer_address` for this.** Investigated
2026-08-18: that column has exactly one writer here (`provision.go`, only when
this service itself created the account, and its own comment says provenance,
not an authorization input). The kaufmann email-transfer population — the
very owners this feature targets — never reaches it: kaufmann's
`mirrorCustomerUserToTenancy` mirrors only `(wallet, email)`, and
`readKaufmannMembers` in the backfill joins `user_profiles` for email only.
The column is empty for everyone who matters, and populating it would need a
backfill **plus** permanent dual-write in kaufmann's `account.go` — a code
path operator-tenancy is retiring.

Resolving live was chosen over fixing the column because:

- A fleet-lite tenant's vehicles typically have one or very few distinct
  owners (the customer's own kernel account), so this is ~1–3 upstream calls
  per tenant, cached.
- Display gate and execution gate become the same check from the same source.
  Kaufmann's cached-column design can disagree with accounts-api both ways
  (button shows then 403s; capability exists but is never offered) — that
  divergence becomes structurally impossible here.
- The exact-match-on-checksummed-strings footgun of the SQL approach
  disappears; the accounts-api comparison is case-insensitive.

Prerequisite: add a by-wallet account lookup to
`internal/gateway/accounts_api.go` (kaufmann's `AssertTenantMaySignFor`
already calls accounts-api by wallet, so the upstream supports it).

## Steps

Each step lands separately; nothing user-visible until step 4.

1. **Tx plumbing here.** Deps (river, go-transactions, go-zerodev), River's
   own migrations as goose files, sharing settings, the fleet-client factory,
   and the queue lifecycle wired in `cmd/`. Run workers in the API process for
   now with a small `MaxWorkers`; splitting into a separate deployment is a
   values-file change we take only if bundler latency ever measurably crowds
   the API. *Cost if wrong: none user-facing; inert until step 2.*

   Two corrections found while building it:

   - **There is no such thing as an "empty worker registered".**
     `river.Client.Start` rejects an empty Workers bundle outright, and `main`
     treats an unstartable queue as fatal — so a client built with no workers
     would refuse to boot the service in exactly the environments where
     sharing is configured, i.e. a two-app outage caused by a feature neither
     app calls yet. `NewQueue` therefore returns `(nil, nil)` for a nil worker
     bundle and step 2 is what passes a real one.
   - **`Settings.Validate` stays untouched in this step.** Making the sharing
     settings boot-required before the chart has been syncing the secrets for
     a release is the same outage in a different costume. The strictness moves
     here only once step 1b has shipped.
1b. **Chart secret refs** for `RPC_URL` and `BUNDLER_URL`. Separate from step 1
   because it cannot merge until the AWS Secrets Manager entries exist: a
   missing `remoteRef` fails the whole ExternalSecret, so the pod loses its DB
   credentials too and never starts. That is the `#42` lesson already recorded
   in `charts/fleet-tenancy-api/templates/secret.yaml`. Merging this to `main`
   syncs the ExternalSecret; the code that needs the values ships later on a
   `v*` tag, which is the ordering the chart/image split exists to give us.

2. **Share endpoint + worker + status.** Port `buildSetPermissionsCall`,
   `AssertTenantMaySignFor`, the `fleetCaller` interface (mock it in tests,
   as kaufmann does), the authorization chain, the `shareable-owners`
   endpoint. Testcontainers for the queue, table-driven tests for the
   authorization chain. *Cost if wrong: an unauthorized grant is an on-chain
   permission leak — this step carries the plan's risk; the worker re-assert
   is the mitigation.*
3. **fleet-lite api.** `POST /vehicles/{tokenID}/share`, status proxy,
   `manage_vehicles` gate via `/v1/authz`, `canShare` on `GET /vehicles` from
   `shareable-owners`. *Cost if wrong: a wrong `canShare` shows a button that
   403s, or hides one that would work — annoying, not dangerous.*
4. **fleet-lite web.** Share button on the list view, share modal (wallet
   input, duration picker, existing-shares list read from identity-api via the
   existing proxy), job polling. Detailed in the fleet-lite doc. *Cost if
   wrong: frontend-only.*
5. **Later, out of v1:** revoke (same machinery, zeroed permissions);
   phase-2 passkey signing (npm package published from `b2b-fleet-mgr-app`)
   for owners without the operator signer; surfacing the same share flow in
   the b2b console.

## Traps, recorded so they're hit once

- **Address comparisons in SQL are exact-match on EIP-55 strings** in this
  ecosystem (this service's users PK, kaufmann's `FindSharedAccountOwners`).
  The live display gate avoids depending on that, but checksum-normalise
  anyway wherever an address is written or joined — fleet-lite's
  `owner_address` join against `shareable-owners` still compares strings.
- **The SACD contract is not the registry.** Separate address, separate
  setting. Kaufmann already made this mistake survivable by making an unset
  `SACD_ADDRESS` a logged no-op; here an unset address should fail
  `Settings.Validate` instead — this service's precedent (the
  `TENANT_SECRET_ENC_KEY` boot refusal) is the right one, and the share
  feature is the endpoint's whole point, not a best-effort side effect.
- **Permission bitmask encoding**: 2 bits per permission (`11`/`00`), 2
  reserved low bits. v1 default mirrors b2b's `defaultPermissions`: everything
  on **except `APPROXIMATE_LOCATION`**. Kaufmann's post-transfer re-share uses
  `0xFFFF << 2` (all on) — that's the tenant granting itself; a customer
  sharing to an arbitrary wallet gets the narrower default. **`COMMANDS` is
  on by default — decided 2026-08-18**: sharing is expected to include
  operating the vehicle (lock/unlock), matching b2b's defaults.
- **One SACD row per grantee**: `setPermissions0` for an existing grantee
  overwrites. Re-sharing to the same wallet is an update, not a duplicate —
  the UI can treat it as idempotent.
- **Success shape**: single-job `isSuccessful`, never the per-VIN string.
- **This service must keep failing closed without taking the feature down
  with it**: a bundler outage fails share jobs, and must not affect
  `/v1/authz` — keep the worker pool small and the tx path's DB usage off the
  authz path's pool budget.

## What done looks like

A fleet-lite member with `manage_vehicles`, in a tenant whose vehicles were
transferred in via kaufmann's email flow, opens the vehicle list, clicks
Share on a vehicle, pastes a wallet, picks "1 year", and within ~a minute the
grantee appears in the vehicle's share list (read back from identity-api, i.e.
from chain state, not from our database). A member without `manage_vehicles`
never sees the button; a vehicle whose owner never registered the operator
signer never shows it either.
