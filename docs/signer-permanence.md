# The tenant signer key is effectively permanent

Written 2026-08-21, as part of plan 06 step 2. The absence of a rotate
endpoint, a rotate subcommand or an overwrite path is a **decision**, and this
note exists so it is never read as an omission and helpfully "fixed".

## Why it cannot be rotated

The signer's public address is registered on-chain as a validator on kernel
accounts — every shared vehicle account created with
`providedSignerAddress = <tenant signer>` authorizes exactly that address via
a weighted-ECDSA validator installed at registration. The key in our database
is one half of a pair; the other half is **state on other people's accounts**.

Writing a new key here does not touch that state. After an overwrite:

- every operation on those kernel accounts fails — accounts-api and the chain
  still expect the OLD address;
- the failure names nothing: token-exchange answers a clean 403
  ("lacks permissions"), indistinguishable from a permissions problem;
- nothing in our systems knows which accounts registered which historical
  address, so the blast radius is not even enumerable after the fact.

That is why `ProvisionSigner` is create-if-absent with the condition **in the
SQL** (`WHERE signer_key_enc IS NULL OR signer_key_enc = ''`), why a tenant
with a signer is refused rather than updated, and why `signer-diff` (step 1)
compares the two legacy copies by derived address before any consolidation.

## What the actual compromise response is

Verified against the `accounts` repo, 2026-08-21:

- **There is no server-side signer update.** `providedSignerAddress` is
  accepted exactly once, in `POST /api/shared/account/email` (shared-account
  registration), where the weighted-ECDSA validator is installed. No PATCH or
  PUT of it exists anywhere in the API.
- **The accounts are not irrecoverable in principle.** The Turnkey EOA remains
  a kernel co-signer that the end user can drive via OTP / email recovery, so
  an on-chain validator change (or a migration to a new kernel) is possible
  through that path. **Nothing automates it today** — it would be per-account,
  driven from the user side, and has never been exercised.

So the honest statement is: a compromised signer key means every kernel
account that registered it must have its validator replaced through the
Turnkey co-signer path, one account at a time, with tooling that does not yet
exist. Prevention is the plan: the key is AES-256-GCM encrypted at rest,
decrypted only inside `CredentialService` for the duration of one operation,
and never leaves the service.

## If accounts-api ever grows a signer update

The permanence claim softens to "rotation requires coordinating a server-side
validator swap per account". Update this note and plan 06 in the same change —
`signer-diff`'s stored-address cross-check becomes the verification tool for
any such rotation.
