# One signer key, in one service

Status: **written 2026-08-19, nothing started.** Prompted by the vehicle-sharing
work, which made this service the second holder of a production signing key it
does not own, cannot write and cannot rotate.

This plan departs from the shape it was first sketched in. The brief was "move
kaufmann's shared-account operations into fleet-tenancy-api, and the key follows
them." Reading the two services says the key and the operations should be
separated instead: the key moves, the operations stay. The evidence for that is
in [What is wrong now](#what-is-wrong-now) and the rejected alternative is
recorded in full under [Considered and rejected](#considered-and-rejected), so
it can be re-argued rather than re-discovered.

## What is wrong now

### The same private key is stored twice, under two different master keys

One tenant signer private key. Two rows, two services, two AES-256-GCM
implementations, two AWS Secrets Manager entries wrapping them.

| | kaufmann-oracle | fleet-tenancy-api |
|---|---|---|
| Column | `tenants.signer_key_enc` (`internal/db/migrations/20260506154609_add_signer_to_tenants.sql:7`) | `tenant_credentials.signer_key_enc` (`internal/db/migrations/20260805120000_init.sql:49`) |
| Crypto | `TenantService.encryptSecret`/`decryptSecret`, `internal/service/tenant.go:338`, `:359` | `service.EncryptSecret`/`DecryptSecret`, `internal/service/crypto.go:20`, `:36` |
| Master key | ASM `{ns}/kaufmann-oracle/tenant-enc-key` (`charts/kaufmann-oracle/templates/secret.yaml:58`) | ASM `{ns}/fleet-tenancy-api/tenant_secret_enc_key` (`charts/fleet-tenancy-api/templates/secret.yaml:31`) |
| Plaintext lifetime | cached **24 hours** in memory (`internal/service/tenant.go:52`, key decrypted into the cached struct at `:125`) | duration of one call (`internal/service/sharing.go:167` and the comment above it) |
| Reader | `TenantService.GetSignerKey`, `internal/service/tenant.go:140` | `ShareAuthorizer.signerKey`, `internal/service/sharing.go:181` |
| Writer | `TenantController` (`internal/controllers/tenants.go:221`) and `backfill-tenant-signers` | **none** — only `cmd/fleet-tenancy-api/backfill.go:228`, run once |

The two ciphertexts are not copies of each other. `recrypt`
(`cmd/fleet-tenancy-api/backfill.go:309`) decrypts under kaufmann's master key
and re-encrypts under this service's, so the rows differ byte for byte while the
plaintext is meant to be identical. **Nothing verifies that it still is.**
`tenancy-diff` and `tenancy-check` compare authorization grants only — neither
mentions signers — so a divergence between the two copies would be silent until
a UserOp was signed by a key no kernel account ever registered.

The two implementations are functionally the same today (`sha256(passphrase)` →
AES-256-GCM, `base64(nonce|ciphertext)`), which is exactly what makes the
duplication easy to keep: nothing forces them to stay the same, and the next
change to either — a KDF, a KMS envelope, a nonce format — silently breaks the
backfill's `recrypt` in a way that decrypts to garbage rather than erroring,
because GCM only authenticates against the key you actually used.

### Neither copy can be rotated, and that is not a gap to fill

This is the part that changes what the fix should be.

The signer address is registered on the owner's ZeroDev kernel account **at
account creation**, as a weighted-ECDSA secondary validator — that registration
is the entire basis on which either service may sign for an owner
(`internal/service/shared_signer.go:173`, kaufmann
`internal/service/shared_signer.go:46`). Both services' accounts-api clients
expose `CreateAccount(email, providedSignerAddress, jwt)` and nothing else that
touches the signer: there is **no update path in either client**
(`internal/gateway/accounts_api.go:31` and kaufmann's `:94`, `:161`).

So rotating a tenant's signer key does not roll a credential. It orphans every
kernel account that registered the old address — every vehicle that arrived
through kaufmann's email-transfer flow becomes unsignable, which is precisely
the population both the shared-account operations and vehicle sharing serve.

Two consequences, and they point the same way:

- The API key has a rotation path (`SetCredentials`, self-serve credential
  writes, and `keyFingerprint` at `internal/service/credentials.go:63` so a
  rotation invalidates the cached minter). The signer key has none, in either
  service, and **should not simply be given one** — a rotation endpoint that
  quietly breaks every existing account is worse than no endpoint.
- Because rotation is not the response to a compromise of this key, **reducing
  the number of places it is stored is the response.** That is the whole reason
  this plan is worth doing now rather than filing behind a rotation story.

### The key's blast radius is two services, its use is one function

Every use of the signer *private key* in kaufmann funnels through a single
function. `GetSignerKey` has exactly one non-test call site in the entire repo:

```
internal/onboarding/disconnect_shared.go:169   deps.tenantSvc.GetSignerKey(ctx, tenantID)
```

That line is inside `resolveSharedSigner`, which all three shared-account
workers call — `transfer_shared.go:115`, `disconnect_shared.go:114`,
`delete_shared.go:110`. Nothing else in kaufmann signs with it.

The signer *address* is a different story and is not in scope for removal: both
services create shared accounts with it (kaufmann
`internal/controllers/account.go:419`, this service
`internal/service/provision.go:110`), and it is a public address. Duplicating a
public address across two services that both legitimately need it is not the
problem. Duplicating the private key is.

### The operations that use the key are not portable, and the key is

The brief's proposed vehicle was to move `transfer/shared`,
`disconnect/shared`, `delete/shared` and the post-transfer re-share into this
service, with the key following. What those workers actually do, once the
on-chain call is removed, is kaufmann's device-oracle domain end to end:

- `burnSDShared` (`internal/onboarding/shared_burn.go:43`) calls the external
  vendor's `Disconnect([]vin)` (Kore/Eleva, `internal/onboarding/vendor.go:24`),
  then writes `onboarding_status`, `connection_status`, `disconnection_status`,
  `synthetic_token_id` and `wallet_index` on the `vins` row.
- `burnVehicleShared` (`:108`) clears `vin_fleet_groups` memberships before
  nulling `vehicle_token_id`, because of a foreign key it must not trip
  (`service.ClearFleetGroupsForToken`; the fix for it is kaufmann #221).
- `delete_shared.go:131` auto-chains the disconnect off `record.SyntheticTokenID`
  and calls `resetAfterBurn` on the inventory state machine.
- `transfer_shared.go:158` appends to the IMEI inventory audit log.

`vins` is 21 columns (`internal/db/models/vins.go:377`). This service's entire
vehicle vocabulary is `vehicle_entitlements.vehicle_token_id`
(`internal/db/migrations/20260805120000_init.sql:121`) — no VIN, no IMEI, no
synthetic device token, no onboarding status, no vendor connection.

The per-VIN status shape follows from the same place and cannot move
independently of it: `GetDisconnectStatusForVins` and `GetDeleteStatusForVins`
derive `"Success"`/`"Failure"`/`"Pending"` from `vins.onboarding_status` via
`status.GetDisconnectStatus` (`internal/onboarding/status/status.go:185`), per VIN,
with no job id involved. Only `GetTransferStatusForVins`
(`internal/controllers/vehicle.go:1946`) uses the single-job `isSuccessful`
shape this service uses. So the "two status conventions" friction is not an
impedance mismatch to reconcile — it is a signal that two of the three
operations are reads of a device-inventory table, not reads of a job.

Moving all of that here would import the device oracle into an authorization
service, and would invert this programme's own stated boundary:

> the chain records what a vehicle is and who owns it; web2 records who may look
> at it
>
> — [`../operator-tenancy/06-onchain-surface.md:125`](../operator-tenancy/06-onchain-surface.md)

## The destination

**One copy of every tenant signer private key, in fleet-tenancy-api. Kaufmann
keeps its operations, its `vins` bookkeeping and its status shapes, and asks
this service to sign.**

```
b2b-fleet-mgr-app  ──POST /v1/vehicle/{transfer,disconnect,delete}/shared──▶  kaufmann-oracle
                                                                                   │
                                                       vendor disconnect,          │  vins row,
                                                       inventory, status  ◀────────┤  fleet groups
                                                                                   │
                                       ──POST /v1/tenants/{id}/shared-ops──────────▶  fleet-tenancy-api
                                       ◀─────── jobId, then poll status ────────────       │
                                                                                           │ signer key
                                                                                           ▼
                                                                                   owner's kernel account
```

After this plan:

| | kaufmann | tenancy |
|---|---|---|
| `signer_key_enc` | **column dropped** | the only copy |
| `signer_address` | kept — it creates accounts with it | kept — it creates accounts with it |
| Shared-account routes | unchanged, still kaufmann's | — |
| `vins`, vendor, inventory, status | unchanged, still kaufmann's | never touched |
| UserOp signing | none | all of it |

Nothing about b2b-fleet-mgr-app's three proxy routes changes
(`api/internal/app/app.go:151`, `:157`, `:163`;
`api/internal/controllers/vehicles.go:211`, `:221`, `:231`). No frontend change.
No status-shape change. That is the point of choosing this seam: the whole
migration is invisible above kaufmann.

The substrate to do it already exists here, built for sharing and idle between
shares: the River queue and its isolated pgx pool (`internal/sharing/queue.go`),
the go-zerodev fleet client (`internal/sharing/client.go`), the
`SendCall(owner, signerPK, msg)` worker shape (`internal/sharing/worker.go:147`),
`SharedSignerService` resolving signing authority live against accounts-api, and
`ShareAuthorizer` running the chain twice. The kaufmann→tenancy HTTP channel
also exists, with its two-header scheme already reasoned through
(`kaufmann-oracle/internal/gateway/tenancy_api.go:30`).

One property of the existing authorizer makes the fit closer than it looks:
`assertEntitled` (`internal/service/sharing.go:139`) is a no-op for
implicit-mode tenants, and the backfill wrote every kaufmann tenant as
`entitlement_mode='implicit'` (`cmd/fleet-tenancy-api/backfill.go:221`). So for
kaufmann's operations the chain reduces to owner-resolution plus the live signer
check — which is exactly what `resolveSharedSigner` does today
(`internal/onboarding/disconnect_shared.go:151`), against the same accounts-api,
with the same meaning.

## Steps

### 1. Prove the two copies are the same key

A `signer-diff` subcommand in this service, shaped like fleet-lite's
`tenancy-diff` and `groups-diff`. For every tenant present in both databases:
decrypt both `signer_key_enc` values under their own master keys, derive the
public address from each, and compare **addresses only — never log, print or
compare the key material**. Report `agree / differ / missing_local / missing_remote`,
and cross-check each derived address against the stored `signer_address` in both
rows.

Nothing else starts until this reports `differ=0` and no unexplained missing.

**Cost if wrong:** everything downstream assumes one logical key with two
wrappers. If a tenant's copies have drifted — a signer regenerated on one side,
a partial backfill, a master key changed — then consolidating picks a winner
silently. The losing key is the one some kernel accounts registered as their
validator, and those vehicles become permanently unsignable with no error that
names the cause: accounts-api simply reports `providedSignerAddress` ≠ our
signer, and every operation on them returns a clean, wrong 403.

### 2. Make this service able to write and provision a signer, without pretending it can rotate one

Today the only writer of `tenant_credentials.signer_key_enc` is a one-shot
backfill command. Before this service can be the sole holder it must be able to
*create* a signer for a tenant that has none — kaufmann's
`backfill-tenant-signers` (`cmd/kaufmann-oracle/backfill_tenant_signers.go`)
generates a go-ethereum keypair, encrypts the private key and writes both
columns, and ports directly.

Two constraints on the shape:

- Provisioning is **create-if-absent only**. Overwriting an existing signer is
  the orphaning failure described above, so the write must be conditional in SQL
  (`WHERE signer_key_enc IS NULL`), not merely conditional in Go.
- Ship a `docs/` note stating that this key is effectively permanent and why,
  and what the actual compromise response is (re-registering validators per
  kernel account, or account migration — neither of which exists, and neither of
  which this plan builds). The absence of a rotate endpoint must read as a
  decision, not an omission, or someone will helpfully add one.

Confirm with the accounts-api team whether a `providedSignerAddress` update
exists server-side. Our clients do not expose one; if the service does, the
permanence claim softens and the note should say so precisely.

**Cost if wrong:** an unconditional write turns a routine re-run of a
provisioning command into a mass-orphaning event across a tenant's whole fleet,
discovered only when customers report that transfers stopped working. This is
the single most destructive thing in the plan and it is destroyed by one missing
`WHERE`.

### 3. A typed shared-operations endpoint here — an enum, never raw calldata

`POST /v1/tenants/{tenantId}/vehicles/{tokenId}/shared-ops` → `202 {jobId}`,
with `GET .../shared-ops/status?jobId=` mirroring `ShareStatus` exactly
(`internal/sharing/enqueue.go:37`, tenant-scoped, `ErrJobNotFound` for another
tenant's job).

The body carries an **operation enum**, not calldata:

| `op` | Packs | Sent to |
|---|---|---|
| `transfer_vehicle` | `VehicleId.PackSafeTransferFrom(owner, target, tokenId)` | `VEHICLE_NFT_ADDRESS` |
| `burn_synthetic` | `SyntheticDeviceId.PackBurn(syntheticTokenId)` | `SYNTHETIC_NFT_ADDRESS` |
| `burn_vehicle` | `VehicleId.PackBurn(tokenId)` | `VEHICLE_NFT_ADDRESS` |
| `grant_sacd` | `BuildSetPermissionsCall(...)` — the existing one | `SACD_ADDRESS` |

Each job reuses `ShareAuthorizer.AuthorizeShare` unchanged: entitlement (a no-op
for implicit tenants), live owner from identity-api, live signer authority from
`SharedSignerService`, then the key. `MaxAttempts: 1` for the same reason shares
use it, and more sharply — a retried burn is not idempotent in any useful sense.

`grant_sacd` subsumes the post-transfer re-share
(`transfer_shared.go:shareWithTenant`), which is the same call this service
already makes, aimed at the tenant's own client id rather than a customer's
grantee. It should chain from `transfer_vehicle` here rather than being a second
call from kaufmann, so the "transfer then re-share on the new owner's kernel"
sequence stays one unit with one signer resolution.

**Cost if wrong:** an endpoint that accepts caller-supplied calldata is a signing
oracle over every kernel account any operator's signer can act for. A bug in the
authorization chain, a leaked `X-Tenancy-Key`, or a compromised operator license
then means "burn any vehicle in the fleet, transfer it anywhere" rather than
"perform one of four known operations on a vehicle whose owner authorised us."
The enum is not ergonomics — it is the security boundary, and it is very hard to
re-narrow once a caller depends on the general form.

### 4. Point kaufmann's three workers at it

Inside each worker, replace `resolveSharedSigner` + `fleetClient.SendCall` with
an enqueue-and-poll against the new endpoint. Everything else in those workers
stays exactly where it is: the vendor disconnect, every `vins` column write,
`ClearFleetGroupsForToken`, `resetAfterBurn`, the inventory audit append, the
owner refresh after transfer.

The failure semantics must be preserved precisely, because
`shared_burn.go` writes a distinct status before returning on each on-chain
failure (`OnboardingStatusBurnSDFailure`, `BurnVehicleFailure`) and the delete
worker's auto-chain depends on the disconnect leg failing loudly
(`delete_shared.go:131`). A polled remote job that ends `discarded` must land on
the same status as a local `SendCall` error does today, and a poll that times
out while the UserOp is still in flight must not be recorded as a clean failure
— that is the one state that produces a chain/`vins` disagreement.

Kaufmann's worker timeouts (10 min transfer, 30 min disconnect/delete) sit above
this service's 5-minute receipt window and 10-minute job timeout, so a poll
loop fits without changing either. Verify that before writing the loop rather
than after.

**Cost if wrong:** the chain and the `vins` row disagree. The concrete shape:
tenancy burns the synthetic device, kaufmann's poll gives up, the row is written
`BurnSDFailure` with `synthetic_token_id` still populated — and the next delete
auto-chains a disconnect that tries to burn a token that no longer exists. The
recovery is manual and per-VIN.

### 5. Delete kaufmann's copy

Drop the read path first (`GetSignerKey`, the `SignerKey` field, the decrypt at
`internal/service/tenant.go:125`), confirm nothing references it, ship, and only
then drop `tenants.signer_key_enc` in a separate migration. Keep
`tenants.signer_address` — kaufmann still creates accounts with it.

The read set is exactly one function, which step 4 deletes, so this is smaller
than it sounds. The staging is not about doubt over the read set; it is so that
the column still exists if step 4 has to be reverted.

**Cost if wrong:** dropping the column while any path still reads it takes down
kaufmann's shared-account operations, and the column cannot be restored from
this service's copy without the kaufmann master key — which, if this plan has
gone well, is no longer wired into anything. Do not drop the ASM entry in the
same change; leave it until a release has run clean.

### 6. Close the loop on drift

Once there is one copy, `signer-diff` from step 1 has nothing to compare and
should be deleted rather than left to report `0 0 0` forever. What replaces it
is a single assertion in `tenancy-check`: for every tenant with a
`signer_address`, this service holds a key deriving to it.

**Cost if wrong:** low. This is the step to drop if the work has to stop early —
but note that stopping after step 4 and before step 5 leaves *two* copies and
*one* user, which is the worst of both and should not be allowed to become the
resting state.

## Considered and rejected

**Move the shared-account operations into this service.** The brief's original
shape, and the reason this plan exists in the form it does. It ends the
duplication completely and puts every server-signed on-chain operation in one
place, which is genuinely attractive. It was rejected on what would have to come
with it: `vins` (21 columns), the Kore/Eleva vendor connection, the inventory
state machine, `vin_fleet_groups` and its foreign key, and `onboarding_status` —
from which two of the three status endpoints are derived per VIN, so the status
shapes cannot be reconciled without moving the table that produces them. That is
the device oracle, and importing it into an authorization service contradicts
`../operator-tenancy/06-onchain-surface.md`. It would also touch
b2b-fleet-mgr-app's three proxy routes and the frontend calls behind them, where
the chosen seam touches neither. Worth revisiting only if kaufmann's device
domain is being retired for other reasons — in which case this plan's step 3
endpoint is what the moved operations would call anyway, so nothing here is
wasted.

**Leave the key in kaufmann and have this service ask kaufmann to sign.** The
mirror image, and it has one real argument: kaufmann's copy is the original, and
its `AccountController` is where most shared accounts are created. Rejected
because the direction of travel is the other way — this service is the shared
source of truth for tenants and credentials, holds the API keys and the minter,
and the whole operator-tenancy programme is about kaufmann's tenant tables
becoming this service's. Adding a permanent dependency from the new source of
truth back into the system it replaces is a step backwards. It also leaves this
service unable to share a vehicle when kaufmann is down, coupling a
customer-facing feature to an oracle it otherwise never calls.

**A generic "sign this calldata" endpoint.** Much less code than the enum, and
new operations would need no change here. Rejected: see step 3's cost. The
narrow interface is the entire security property, and a general one cannot be
narrowed later without breaking its callers.

**Give this service a signer rotation endpoint first.** Superficially the right
order — fix rotation, then consolidate. Rejected because rotation as normally
understood is not available: the signer is registered as a validator on each
kernel account at creation and neither service's accounts-api client can change
it, so "rotate" means "orphan every existing account". Building the endpoint
would create a loaded weapon whose use is almost always a mistake. The honest
version is step 2's note.

**Sharing one master key between the two services so at least the ciphertext
matches.** Rejected outright. It halves the number of secrets while doubling the
number of services a single compromised master key unlocks, and it removes the
one thing the current split does right. The number to reduce is the copies of
the plaintext, not the copies of the wrapper.

**Do nothing until a share has actually been sent.** A real position: no UserOp
has ever been sent from this service (`../HANDOFF.md`, "Nothing has sent a UserOp
yet"), so the second copy of the key is currently unused in anger. Rejected
because the copy exists in production either way — it is already in the database
and already wrapped by a second master key — so the exposure is present whether
or not the code path runs. Step 1 in particular should not wait: it is read-only,
it is cheap, and it verifies an assumption that vehicle sharing already depends
on.

## Not in scope: minting

This plan does not move minting into fleet-tenancy-api and should not be read as
a step toward it. `../operator-tenancy/06-onchain-surface.md` draws the line —
the chain records what a vehicle is and who owns it; web2 records who may look at
it — and minting is on the far side of it. Mechanically it also needs things this
service does not have and would have to acquire wholesale: synthetic-device
wallets and the wallet-index bookkeeping, vendor onboarding, VIN attestation, and
a `VehicleOwnerSig` produced by a passkey in `b2b-fleet-mgr-app`
(`kaufmann-oracle/internal/onboarding/onboard.go:300`). The server-signed
operations in this plan all act on assets that already exist and on owners whose
kernel accounts already registered our signer; minting creates both, and the
authorization it needs is a human's passkey, not a tenant's signer.

The passkey path is separately noted as phase-2 work for *widening* shared-op
coverage — operator-held vehicles are owned by passkey accounts with no
`providedSignerAddress`, so they get `canShare` false and would get the same
answer from step 3's endpoint. Extracting `b2b-fleet-mgr-app/web`'s
`signing-service.ts` into an npm package is what would change that, and it is
not this plan either.
