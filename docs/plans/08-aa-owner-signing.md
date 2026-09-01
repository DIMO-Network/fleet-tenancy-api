# Owner-mode signing — the tenant's own AA wallet

Status: **written 2026-08-31, nothing started.** Decisions below were taken in
discussion the same day; the open questions at the end are pre-flight checks,
not blockers on starting step 1.

## What is wrong now

Server-side signing exists, but only for one narrow shape of ownership. Every
shared operation this service or kaufmann performs is sent *from the vehicle
owner's kernel account, signed by the tenant's signer key* through the
secondary weighted-ECDSA validator (`go-zerodev/fleet.Client.SendCall`). That
works only when the owner's kernel account was created by kaufmann-oracle with
the tenant signer registered at creation — a property of the account, set
exactly once, with no backfill and no admin path
(`kaufmann-oracle/docs/onboarding-frontend-context.md`, "It is enabled at
account-creation time"). In production that is **25 of the Kaufmann tenant's
187 distinct owners**. The other 162 — including the operator wallet most
minted-but-unsold vehicles sit on — cannot be server-signed at all, so every
share, transfer, disconnect or delete on them needs a person in
`b2b-fleet-mgr-app` with a passkey.

The passkey path is also structurally expensive: `web/src/services/
signing-service.ts` in b2b is a 412-line hand-rolled reimplementation of
`@dimo-network/transactions`' `KernelSigner` (the repo drifted off the SDK in
its PR #26 and still carries the dead dependency), it stores a session key in
localStorage, and it makes every bulk operation a human-in-the-loop exercise.

## The destination

An operator tenant configures an **AA wallet** — a Kernel v3.1 / EntryPoint 0.7
smart account plus the root EOA private key that is its sudo validator — as a
tenant credential in this service. Vehicles the tenant wants machine-managed
are owned by that wallet. When a vehicle's live owner (identity-api) **is** the
tenant's AA wallet, this service signs the operation itself with the root key —
"owner mode" — instead of requiring either a passkey or the
owner-registered-our-signer arrangement.

Owner mode is the third row of a table that already has two:

| Mode | Sent from | Signed by | Validator | Envelope | Mechanism |
|---|---|---|---|---|---|
| Passkey (browser) | user's kernel | user via Turnkey | sudo ECDSA | kernel wrap | b2b `signing-service.ts` |
| Shared-signer | owner's kernel | tenant signer key | secondary weighted-ECDSA `0xeD89…eEEE` | EIP-191 personal_sign (`go-zerodev/fleet/signer.go:12-41`) | `fleet.Client.SendCall` |
| **Owner (new)** | **tenant's AA kernel** | **tenant AA root key** | **sudo ECDSA `0x845ADb2C711129d4f3966735eD98a9F09fC4cE57`** | kernel wrap (`go-zerodev/account/signer.go`) | `zerodev.Client.SendUserOperation` |

The mechanism is already proven in production: the sudo path is exactly how
kaufmann's `transactions.Client` mints with the developer AA wallet today. The
pinned `go-zerodev v0.5.1-0.20260513203230` exposes it directly —
`zerodev.NewClient` + `GetSmartAccountSigner(address, pk)` (`client.go:224`) +
`SendUserOperation(callData, wait)` (`client.go:204`). No library change is
needed for the send path.

First slice is **vehicle sharing from fleet-lite**, because the whole
share/revoke pipeline (authorization, job queue, calldata builders, status
polling, UI) already exists here and only the send/authorize step is
mode-aware. Minting to the fleet wallet and the other lifecycle operations
follow as later steps.

## Decisions, 2026-08-31

- **D1 — the credential lives here and only here.** Two columns on
  `tenant_credentials` (`aa_wallet_address`, `aa_wallet_key_enc`), encrypted
  exactly like `signer_key_enc` under `TENANT_SECRET_ENC_KEY`. kaufmann stores
  nothing and proxies config writes, continuing plan 06's direction (one key,
  one service) rather than repeating the double-custody it exists to fix.
- **D2 — managed customers inherit it through effective-credential
  resolution.** The AA wallet rides on the credential row, so the existing
  resolution rule (own credential if present, else parent's) gives an
  operator-managed customer owner-mode sharing with no extra wiring. **For v1
  this is open to any member who passes the existing route gates
  (`manage_vehicles`)** — a member of a managed customer can share any vehicle
  the fleet wallet owns and the customer is entitled to. A real permissioning
  model (per-tenant enablement, per-member capability) is deliberately deferred
  to the next version; this paragraph is the required call-out.
- **D3 — config flows b2b → kaufmann proxy → `/v1`,** like every other console
  write. The key transits kaufmann in flight and is stored only here. This does
  not reopen the deferred b2b-credential question.
- **D4 — strict config-time validation, plus a generate flow.** A pasted
  wallet is refused unless it verifies (spec below), because the failure mode
  of a wrong key is a gas-spending job with `MaxAttempts: 1`. For tenants with
  no wallet, the console offers **generate**: a browser port of
  `wallet-creator` (`~/Source/wallet-creator/index.ts`) — generate root key,
  derive the kernel counterfactual, deploy it with a sponsored no-op UserOp,
  then submit address + key through the same config endpoint. b2b already has
  every dependency (`@zerodev/sdk`, `@zerodev/ecdsa-validator`, `viem`).
- **D5 — mode is chosen per vehicle by the live owner, not per tenant.**
  `AuthorizeShare` already fetches the owner from identity-api; owner ==
  AA wallet → owner mode, else the existing shared-signer check. A per-tenant
  switch was rejected (below).
- **D6 — mint-to-fleet-wallet is in scope for the programme,** as a later
  step: an add-vin toggle in b2b choosing the tenant's AA wallet or the current
  user's wallet as the minted owner.

## Validation spec (strict, step 1)

On `PUT` of an AA wallet, in order, refusing on the first failure:

1. Address parses and is stored EIP-55 checksummed (`common.HexToAddress(...)
   .Hex()` — the same normalization the backfill uses; the shared-accounts
   lookup trap shows what a non-checksummed write costs).
2. Key parses (`crypto.HexToECDSA`, tolerating a `0x` prefix explicitly — the
   share path deliberately does not trim one, so trim here at the edge).
3. `eth_getCode(address)` is non-empty — the kernel is deployed. go-zerodev has
   no initCode support, so an undeployed wallet would fail at first use;
   refuse it at config time instead. The generate flow always passes this
   because deployment is part of generation.
4. The kernel's sudo validator recognises the key: read the ECDSA validator
   `0x845ADb2C711129d4f3966735eD98a9F09fC4cE57`'s owner record for the kernel
   address (`ecdsaValidatorStorage(address)` — verify the getter name against
   the deployed contract before coding it) and compare to the key's derived
   address. This works whatever account index the wallet was created with,
   which pure counterfactual re-derivation would not.
5. Chain sanity: the RPC answers `eth_chainId` == `CHAIN_ID`. Free, and it
   turns a mangled RPC secret into a config-time error instead of a job
   failure (the two-backslash incident).

## Steps

**Step 1 — this repo: credential storage + config surface.** Migration adding
`aa_wallet_address VARCHAR(43)` / `aa_wallet_key_enc TEXT` to
`tenant_credentials`, sqlboiler regen, `PUT /v1/tenants/{id}/credentials/
aa-wallet` (write-only key, full validation above) and a readback that returns
address + configured-flag, never the key. CredentialService gains a decrypt
path with the same one-operation-lifetime discipline as the signer key. Cost
if wrong: a credential row, reversible; nothing reads it yet.

**Step 2 — this repo: owner mode in the sharing engine.** `AuthorizeShare`
branches on live owner == effective credential's AA wallet before the
`MaySignFor` check; the share/revoke workers send owner-mode jobs through a
`zerodev.Client` (sudo) instead of `fleet.SendCall`; `shareable-owners`
reports the AA wallet as a positive owner (with an `ownerModeWallet` field so
callers can distinguish owner from signer) without touching accounts-api or
`shared_accounts`. Calldata builders are untouched — they are mode-agnostic.
Same `MaxAttempts: 1`, same job kinds, same status route. Cost if wrong: this
is the gas-spending step; it ships dark (no tenant has an AA wallet until
step 3) and the first exercise is step 5's checklist, not user traffic.

Decisions taken while building step 2 (2026-08-31):

- **`AA_BUNDLER_URL` is its own setting**, optional on top of the sharing set
  (like `SACD_UPLOAD_URL`), because paymaster sponsorship is per-project and
  the project confirmed to sponsor fresh tenant AA wallets is the one the
  wallet-creator flow uses — which need not be `BUNDLER_URL`'s. It is the ONE
  switch for the feature: the authorizer, the display gate (`FilterSignable`,
  and through it `MaySignFor`) and the workers all read
  `OwnerModeConfigured()`, so half-configured means off everywhere, never a
  wrong-validator attempt.
- **The value lives in ASM, gated by a values flag** (`aaBundlerSecretEnabled`,
  default false in both values files). The user asked for values
  configurability, but this repo is PUBLIC and the URL embeds the project id —
  in git it would hand the sponsorship budget to anyone reading GitHub. The
  flag in values is the per-environment switch; the URL never appears in git,
  and the gated remoteRef cannot fail the whole ExternalSecret before the ASM
  entry exists.
- **One long-lived owner client** (resolves open question 3): go-zerodev's
  `Client` binds an account at construction, but `GetUserOperationAndHashToSign`
  + per-job `GetSmartAccountSigner` + `SendSignedUserOperation` let one client
  serve every tenant's wallet. The bound account is an ephemeral throwaway key
  that never signs anything.
- **The SACD document path carries over unchanged**: `SignSACDDocument`
  already signs with the `0x01 + ECDSA-validator` identifier envelope, and for
  an AA wallet that validator's per-kernel owner record is exactly what config
  time verified.
- **Typed shared operations refuse owner-mode vehicles** — synchronously at
  the endpoint (409) and again in the worker — until step 7. Every shared-op
  path signs through the weighted-ECDSA validator the AA wallet does not have,
  and the transfer op chains a re-share whose gate assumes the signer
  arrangement.

**Step 3 — kaufmann proxy + b2b console.** Thin write-through proxies in
kaufmann (the #197/#200 pattern, no local storage), and a settings section in
b2b beside the dev-license config: paste (masked, write-only) or generate
(D4). Deploy order as always: this service, then kaufmann, then b2b.

**Step 4 — fleet-lite copy.** `AnnotateCanShare` needs no logic change — the
improved `shareable-owners` answer flows through. The frontend work is
`web/src/utils/share-blocker.ts`: the `'owner'` blocker's "this account hasn't
authorized fleet sharing" is wrong advice when the tenant has a fleet wallet —
the actionable sentence is "not held by the fleet wallet". Pass the `via`
field through if the copy wants to distinguish owner-mode shareables.

**Step 5 — first prod exercise.** Configure a test tenant's wallet via the
console generate flow, transfer one vehicle to it (existing b2b passkey
transfer), send one share, watch the worker logs, then revoke it. The
owner-mode path will be exactly as unexercised as the share path was on
2026-08-22 and the same rules apply: `no receipt` means unknown, a 403 is
policy, a 5xx is infrastructure, and nothing retries.

**Step 6 — mint to the fleet wallet (later).** The add-vin toggle (D6). Two
designs need settling first: (a) when the minted owner is the tenant AA wallet,
the owner's EIP-712 mint signature must be produced server-side — as an
ERC-1271 kernel signature by the AA root key (`SmartAccountPrivateKeySigner.
SignTypedData` exists in go-zerodev; confirm the registry accepts ERC-1271 for
contract owners on this path, and how kaufmann's existing "no typedData =
server-owner" mint case decides); (b) kaufmann must obtain that signature
without holding the key — a scoped, **enum-typed** signing operation on this
service (plan 06's rule: an enum, never raw bytes). A generic sign-anything
endpoint is rejected below.

**Step 7 — the other lifecycle ops (later).** Transfer, disconnect, delete in
owner mode. These should land as modes of this service's typed shared-ops
surface and converge with plan 06 step 4 (pointing kaufmann's workers here),
not as a parallel kaufmann implementation. The b2b UI then skips the passkey
whenever the vehicle's owner is the tenant's AA wallet.

## Considered and rejected

- **A per-tenant "AA mode" switch** deciding how to sign. Rejected: the chain
  already says who owns each vehicle, and a switch is a second copy of that
  fact with no defined resolution when they disagree (the same one-fact-twice
  reasoning as D9's `view_all_fleets`). Per-vehicle owner comparison is one
  eth-free identity-api read the authorize step already performs.
- **Storing the AA key in kaufmann too** so its workers can sign locally.
  Rejected: recreates the double-custody plan 06 is unwinding. kaufmann's
  workers get owner mode by calling this service (step 7), not by holding keys.
- **Backend key generation in Go** for the generate flow. Rejected for v1:
  go-zerodev cannot deploy (no initCode) and has no counterfactual derivation,
  so generation would mean extending the library; the browser already has the
  full ZeroDev SDK and the wallet-creator flow is a proven port. Revisit if
  key-transits-browser becomes unacceptable — the config endpoint's contract
  (address + key in, nothing out) would not change.
- **A generic sign-typed-data / sign-hash endpoint** for step 6. Rejected: a
  service that signs arbitrary payloads with a fleet-owning key is an oracle
  for stealing the fleet. Signing operations are enumerated, like shared ops.

## Open questions / pre-flight checks

1. ~~**Paymaster policy.**~~ **Resolved 2026-08-31**: the wallet-creator
   project (`cde30207-…`, `index.ts:9`) is confirmed as the one allowed to
   sponsor these UserOps. It is configured as `AA_BUNDLER_URL` (see step 2's
   decisions); write the ASM entry from a file, then flip
   `aaBundlerSecretEnabled`.
2. ~~**Validator storage getter.**~~ **Resolved 2026-08-31**: verified against
   the deployed Polygon contract — `ecdsaValidatorStorage(address)`, selector
   `0x20709efc`, returns the owner as a 32-byte-padded address; a known kernel
   answered its root EOA.
3. ~~**Client lifecycle.**~~ **Resolved 2026-08-31**: one long-lived client,
   per-job signer — see step 2's decisions.
4. **Custody call-out for the docs** (not a question, a note): with a
   generated wallet, this service is the sole custodian of a key that owns
   vehicles. There is no rotation story, same as the signer key
   (`docs/signer-permanence.md` is the sibling); losing `TENANT_SECRET_ENC_KEY`
   or the row loses control of the fleet wallet's vehicles until recovered
   through Turnkey-less means (the root EOA can always be re-verified but not
   re-derived). Say this plainly in the operator-facing docs.
