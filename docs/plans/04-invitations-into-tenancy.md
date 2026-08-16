# Invitations move into fleet-tenancy-api

Status: **P1 built 2026-08-16** (branch `invitations-p1`) — migration
`20260816120000`, `InvitationService`, the `/v1` routes, the Postmark send and
the webhook endpoint, all tested; no caller yet. P2–P4 remain. Originally
written as the handoff immediately after the self-serve creation write-through
(plan 03's closing step) deployed and every other write path was cut over.
Invitations are the last membership-adjacent record living outside this
service.

## What this is

fleet-lite's email invitation flow — invite by email, single-use accept link,
the invitee's wallet binds on acceptance — moves here, records and email
dispatch both. The membership half already write-throughs (fleet-lite `#126`:
Accept calls `GrantMember`), so today's split is: **the grant is shared, the
invitation is not.**

## Why now, in order of force

1. **The console cannot invite.** An operator managing a customer tenant has
   no way to send or see invitations — they live in fleet-lite's local table,
   which b2b cannot reach. Provisioning-by-email covers "create them an
   account", but the invitation path is the only one that works when we don't
   want to create a wallet on someone's behalf (design D4 keeps both paths on
   purpose).
2. **Phase 5 wants the table gone.** `tenant_users` is already a shadow;
   `invitations` is the other local table the decommission list names. It
   cannot drop while it is the system of record.
3. **Scope validation belongs where groups live.** Invites carry
   `allowed_group_ids`; fleet-lite validates them against its group mirror.
   Groups have lived here since P1/P3 — the authority should validate against
   itself.

## Decisions (adopted as proposed at the P1 build, 2026-08-16)

Two naming details settled during the build: the settings keys match
fleet-lite's exactly (`INVITATION_FROM_EMAIL`,
`POSTMARK_INVITATION_TEMPLATE_ALIAS`, `INVITE_EXPIRY_HOURS`,
`POSTMARK_WEBHOOK_SECRET`; plus this service's `INVITE_ACCEPT_URL_BASE`), and
accept derives the membership's permissions from the invite's role via the Q5
mapping (owner → manage_members + manage_settings, member → none), merging
with any existing membership the way fleet-lite's GrantMember does — union of
capabilities, higher role label, scope set verbatim.

| Decision | Proposal | Why |
|---|---|---|
| Surface | **`/v1`, service-to-service**, like everything else. `/user/v1` stays deferred | The three-layer auth and scope model already exist; fleet-lite is a service caller; b2b reaches it through kaufmann's proxies exactly as it reaches provisioning. Building `/user/v1` for this alone re-opens b2b's identity question for no gain |
| Accept authorization | `POST /v1/invitations/accept` with `{token, wallet, email?}` — **the token authorizes, the trusted caller asserts the wallet** | The wallet is an authorization input, but trusting the service caller to assert it is exactly the trust the membership write-through already extends: fleet-lite could PUT the membership directly. No tenant in the path — the token resolves it, as today |
| Email dispatch | **This service sends.** Token minted here, emailed here, hash stored here | The plaintext token should exist in exactly one service's memory. Splitting records-here/email-there means the token transits an API response on every create and resend — a wider exposure for no benefit. The Postmark plumbing already exists here (`AccessEmailService`, #41) |
| Delivery tracking | The Postmark webhook moves here too (`POST /webhooks/postmark`, basic auth, own secret) | Tracking upgrades rows; the rows are here. fleet-lite's webhook route retires with its table |
| Templates | Reuse fleet-lite's Postmark templates (alias + `-es` suffix per locale — `templateAlias`, `invitation.go:454`) via new settings here | They are server-side Postmark aliases, not in-repo assets; only the alias config moves |
| Accept URL | One configured base (`INVITE_ACCEPT_URL_BASE`) pointing at fleets.dimo.co | Every accept happens in fleet-lite regardless of who sent the invite. Operator-sent invites link to the same page |
| Operator-sent invites | `created_by_tenant_id` column (nullable), per the original spec | Distinguishes console-sent from customer-sent in both UIs; the audit question that will get asked |
| Scope encoding | `scope_group_ids TEXT[]`, NULL = unrestricted — same tri-state as memberships | The invite's scope becomes the membership's scope verbatim at accept; two encodings would need a lossy translation |
| Migration | **Id-preserving backfill with `token_hash` copied**, then a flagged cutover | Outstanding invite links must survive: the hash moving intact means an email sent last week still accepts next week. Same uuid-reuse trick as every other migration here |

## Current state inventory (verified 2026-08-16)

**fleet-lite** (all under `api/`):

- Schema: `internal/db/migrations/20260617120000_invitations.sql` — id,
  tenant_id, email, role, `token_hash` (SHA-256, unique), status
  (pending|accepted|revoked), invited_by_wallet, invitee_wallet, expires_at,
  accepted_at; `20260708190000` adds `postmark_message_id`, `email_status`
  (sent|delivered|opened|bounced), `email_status_at`, `email_status_detail`;
  a later migration adds `allowed_group_ids`.
- Service: `internal/service/invitation.go` — `Create` (mints token, sends,
  `ErrEmailNotSent` = partial success 201 + emailSent=false), `List`,
  `Revoke` (pending-only, idempotent), `Resend` (fresh token invalidates the
  old — single active token per invite), `Accept` (hash lookup, single-use,
  pending + unexpired; **since #126 calls `GrantMember`**, so a tenancy
  failure leaves the invite pending and a re-accept retries the whole grant).
- Email: `sendEmail` (`invitation.go:431`) — Postmark template alias by
  inviter locale, accept URL from settings, invitation id as message metadata
  for webhook correlation; `InviteExpiryHours` (default in code).
- Controllers: `internal/controllers/invitations.go` — CRUD gated on
  `manage_members` via `requireTenantCapability` (#126); Accept JWT-only.
- Webhook: `POST /webhooks/postmark` → basic auth against
  `POSTMARK_WEBHOOK_SECRET` → upgrades email_status (bounced beats all).
- Frontend: `web/src/services/tenant-service.ts` invitation methods; accept
  flow lands on the login page with the token.

**This service**: Postmark plumbing exists (`AccessEmailService`,
`gateway/postmark.go`, `POSTMARK_SERVER_TOKEN` secret — note #42's
ExternalSecret-ref lesson: the chart ref must exist before deploy).
`MemberService.Upsert` is the accept-time membership write. Groups tables for
scope validation. No invitation anything.

## Phases

Each independently shippable; the usual deploy order (this service first) and
the squash-merge stacking mechanics apply throughout.

**P1 — the surface, no caller.** Migration (spec's invitations table +
`created_by_tenant_id` + the email-tracking columns), `InvitationService`
(create/list/revoke/resend/accept, token minting + hashing, expiry), Postmark
send with the locale template mapping, the webhook endpoint with its own
secret, settings (`INVITE_ACCEPT_URL_BASE`, `INVITATION_TEMPLATE_ALIAS`,
`INVITE_EXPIRY_HOURS`, webhook secret). Routes: CRUD under
`/v1/tenants/{id}/invitations[/{invId}]` (assertScope), resend as an action,
`POST /v1/invitations/accept` (no tenant in path — validate the caller is a
trusted app; scope for the resolved tenant is checked against the CALLER the
same way the wallet-tenants listing does, or accepted service-caller-wide).
Accept writes the membership via `MemberService.Upsert` and marks the row in
one transaction — unlike fleet-lite's two-step, which #126's retry semantics
papered over.

**P2 — backfill + fleet-lite cutover.** An id-preserving `backfill-invitations`
command (this repo, in-cluster job, source DSN read-only — the P3-groups
manifest pattern). Then fleet-lite behind `INVITES_FROM_TENANCY`: gateway
methods, service delegating to the client, Accept calling the remote accept
(deleting the local GrantMember call — the tenancy accept now does the grant),
webhook route kept but inert once Postmark's webhook URL repoints. An
`invitations-diff` shaped like `tenancy-diff` gates the flag flip. Note the
Postmark webhook URL is Postmark-side config, not a deploy — coordinate the
repoint with the flag flip, and remember both receivers tolerate unknown
message ids silently.

**P2 also needs an ingress this chart deliberately does not have.** The
service is cluster-internal by design, but Postmark posts from the public
internet — so before the webhook URL can repoint, the chart must expose
exactly `POST /webhooks/postmark` (an ingress limited to that path, in front
of the basic-auth check that already gates it). Until then the endpoint
exists and is exercised only by tests; fleet-lite keeps receiving events.

**P3 — the console.** kaufmann proxy routes (the #200 pattern:
`/v1/customers/{id}/invitations…`), b2b BFF routes + an Invitations section on
the customer detail Users tab. `created_by_tenant_id` = the operator tenant on
these. This is the payoff feature; it needs nothing from fleet-lite.

**P4 — decommission.** After a soak with the flag on and the diff clean: drop
fleet-lite's table, service, webhook route, Postmark webhook secret, and the
flag. Belongs to the Phase 5 sweep alongside `tenant_users`.

## Traps, in the order they will bite

- **The token is the credential.** Plaintext exists only in the email and in
  this service's memory at mint time. It must never appear in logs, list
  responses, or the backfill (which copies hashes). Fleet-lite got this right;
  keep it.
- **Resend semantics**: a resend MINTS A FRESH TOKEN and invalidates the old
  (the unique index on token_hash is per-row — the row's hash is replaced).
  An accept racing a resend loses; that is intended, log it clearly
  (fleet-lite's Accept logs "superseded by a newer invite/resend").
- **The tri-state scope, fourth appearance.** Invite `scope_group_ids` NULL ≠
  `[]`, and it flows into the membership verbatim at accept. The service
  refuses an absent field on membership writes; the invite create should be
  equally explicit.
- **Email is courtesy, records are authoritative.** `ErrEmailNotSent` is a
  201-with-flag, never a 5xx — the operator can resend. Same decision as
  provisioning's access email (#41); don't relitigate it.
- **Accept must be atomic with the grant here.** fleet-lite's two-step
  (grant, then mark accepted) relies on idempotent retry; in this service the
  membership upsert and the row update can share a transaction — do that, and
  the retry story gets simpler, not carried over.
- **Webhook correlation is metadata-first**: `invitation_id` rides in Postmark
  message metadata, `postmark_message_id` is the fallback key. Copy both in
  the backfill or historical bounces stop resolving.
- **Capability gate**: create/list/revoke/resend proxied for the console must
  arrive with kaufmann's user-capability check already done (the BFF pattern);
  this service checks the caller tenant's scope, not the human — same split
  as member provisioning, documented at `TenantsController`'s header comment.
- **The chart's ExternalSecret**: a new webhook secret ref must exist in AWS
  before the chart lands, or the whole ExternalSecret fails and the pod loses
  its DB credentials too (#42's lesson, recorded in HANDOFF).

## Verification gate

- P1: route tests (registered + guarded), DB tests for the full lifecycle
  incl. single-use, expiry, resend-invalidates, accept-atomicity, scope
  tri-state.
- P2: `backfill-invitations -dry-run` clean; `invitations-diff` zero on
  differ/missing-remote over a soak window; an end-to-end invite sent before
  the flag flip and accepted after it (the outstanding-link guarantee).
- P3: console sends an invite to a managed customer; the invitee accepts in
  fleet-lite and lands with the invited role + scope; `created_by_tenant_id`
  shows the operator.
- Throughout: both existing diffs stay clean, and the error streams stay
  quiet — 4xx at warn, the #15 lesson.
