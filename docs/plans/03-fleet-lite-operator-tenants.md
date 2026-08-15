# Fleet-lite opens operator-managed tenants

Status: **planned 2026-08-15, nothing shipped.** Written after the first real
provisioning run (tenant TRAST under Kaufmann, 2026-08-14) proved the console
half works end to end and the fleet-lite half does not exist: the provisioned
member logs into fleets.dimo.co and is shown the self-serve onboarding screen.

## What this is

The fleet-lite side of the operator-tenancy programme — migration-plan Phase 2
plus the fleet-lite slice of Phase 3's exit criterion:

> an operator can create a customer, add a user, assign vehicles, and that user
> can log into fleet-lite and see exactly those vehicles — without anyone
> touching a database.

Everything upstream of fleet-lite already works and is verified in prod:
TRAST exists (`f004fc62-752b-4d87-9de9-c20c56e67248`, customer, parent
Kaufmann, `entitlement_mode='explicit'`), the provisioned member holds
role=admin with capabilities, and vehicle 190171 is entitled with operator
provenance. fleet-lite cannot see any of it, for three structural reasons:

1. **`GET /tenants` joins the local `tenant_users` table** — the console
   provision flow (correctly) never writes it, and the tenancy service has no
   list-tenants-for-wallet endpoint for fleet-lite to ask instead.
2. **`NewTenantMiddleware` loads the tenant locally to authenticate as it** —
   a managed tenant has no local row and no credentials, so even a correct
   `Tenant-Id` dies before the authz question can be put.
3. **Every DIMO call mints from `models.Tenant`'s own credentials** — a managed
   tenant's data access runs under its *operator's* license, which fleet-lite
   does not hold and must never hold.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| fleet-lite's caller identity | **Registered service caller** — its Login-with-DIMO license (`0x51dacC165f1306Abfbf0a6312ec96E13AAA826DB`, key already in `prod/fleet-lite-app` secrets as `DIMO_AUTH_PRIVATE_KEY`) gets a `tenant_credentials` row with `is_service_caller = true` | The alternative — a user-JWT `/user/v1` surface — cannot cover the sync/location/group **crons**, which act with no user in the loop. An app identity is needed regardless, and `is_service_caller` was built for exactly this ("may query /v1 for any tenant; grant sparingly"). fleet-lite is the customer product; acting across customer tenants is its job. |
| When the app identity is used | **Only when the subject tenant holds no local credentials.** Self-serve tenants keep authenticating as themselves | Two reasons: rollout safety (existing calls change nothing, so a bad registration row cannot take down self-serve tenants), and least privilege (the bounded path stays bounded where it can). |
| Wallet → tenants | **`GET /v1/tenants?wallet=&surface=`** on the service surface, per the spec that already defines it | `/user/v1` stays unbuilt. The service surface already has three auth layers and a scope model; the list is scope-filtered for ordinary callers (each row must pass the `CallerMayAccess` expression) and unfiltered for service callers, so it leaks nothing new. |
| Managed tenants in fleet-lite's DB | **A local mirror `tenants` row (id + name, no credentials), upserted when the middleware first resolves the tenant remotely** | Every fleet-lite table FKs `tenants(id)` — vehicles, groups, favorites, geofences, TCO. A mirror row is the difference between this increment and re-keying the whole schema. Same pattern as the P4 group mirrors: tenancy owns the record, the local row exists for SQL joins. |
| Developer JWTs for managed tenants | **The tenancy minter** (`GET /v1/tenants/{id}/dimo-token`), routed through `DimoAuthProvider.GetDeveloperJWT`'s existing choke point when `ClientID == ""` | Every DIMO call site (telemetry, attest, fetch, extract, vehicle/asset exchange) funnels through the provider, so one branch covers all of them. `dimoauth`'s `GetVehicleJWT`/`GetAssetJWT` take the dev JWT as a parameter — the exchange needs the token-exchange URL, not the key — so no key material is ever needed locally. |
| Vehicles for explicit-mode tenants | **Entitled set ∩ operator's privileged set, upserted per (tenant, token), with deletion of rows that leave the entitled set** | The design's materialisation rule. Deletion is new — self-serve sync is additive-only — because revoking an entitlement must actually remove the vehicle. Deletion is safe here precisely because the entitled set is authoritative and exclusive per operator. |
| `GET /tenants` | **Union of tenancy list and local list, deduped by id** | Tenancy alone would drop any self-serve tenant created after the backfill, because `POST /tenants` still writes only locally. The union is the transitional read, exactly as the groups move did it; it collapses to tenancy-only when self-serve creation writes through (deferred with `/user/v1`). |
| Member writes | **Write-through to `PUT`/`DELETE /v1/tenants/{id}/members/{wallet}`, in a separate PR** | Found during planning: fleet-lite's member/invitation writes are still local-only, so since the 2026-08-11 cutover every fleet-lite grant reports success and confers nothing — the same bug kaufmann had fixed on 2026-08-12. The HANDOFF's "both write memberships here" was wrong about fleet-lite. Kaufmann's ordering rules apply: grants local-first, revocations remote-first, a failed remote write fails the request. |
| Login recording | **`POST /v1/tenants/{id}/members/{wallet}/login`** in tenancy; fleet-lite calls it and keeps the local touch best-effort | `last_login_at` lives on the shared membership row (the spec says so; the sync tiering reads it). A managed tenant has no local `tenant_users` row to touch at all. |

## What does NOT change

- Self-serve tenants: same auth, same sync, same listing (their local rows are
  in the union). The coexistence guarantee from the migration plan.
- b2b and kaufmann: untouched. Their surface was finished in the console
  programme.
- No impersonation: `via=delegation` is still refused outright by the
  middleware. The wallet-tenants endpoint returns **direct memberships only**
  for `surface=fleet_lite`, so a delegated tenant never even appears.
- The scale tiering (master-pass per operator, warm/cold) is **deferred**: at
  today's managed-tenant count the per-tenant privileged fetch is one paged
  identity-api query. R6's mitigation becomes real work when an operator with
  thousands of vehicles onboards a fleet-lite customer; the seam (a sync path
  keyed on effective clientId) is where it will land.

## Steps

1. **tenancy: wallet-tenants + login touch** (this repo).
   `GET /v1/tenants?wallet=&surface=` — memberships joined with tenants for a
   wallet; `surface=fleet_lite` keeps `status='active' AND fleet_lite_enabled`,
   direct only; each row `{tenantId, name, kind, entitlementMode, role,
   permissions, scopeGroupIds}` with the raw-message scope encoding (nil ≠ []).
   Ordinary callers get rows filtered by the `CallerMayAccess` expression;
   service callers get them all.
   `POST /v1/tenants/{id}/members/{wallet}/login` `{email}` touches
   `memberships.last_login_at` and fills `users.email` when empty.
2. **Register fleet-lite's service identity** (prod data change, gated on a
   human): a `tenants` row (`kind='operator'`, `fleet_lite_enabled=false`,
   named as the app identity) + `tenant_credentials` row for `0x51dacC…` with
   `is_service_caller=true` and **no key material** — nothing mints as it
   server-side; the key stays only in fleet-lite's own secret.
3. **fleet-lite: app-identity client + gateway methods.** A synthetic self
   tenant from `DIMO_AUTH_CLIENT_ID`/`DIMO_AUTH_PRIVATE_KEY` mints the caller
   JWT when the subject tenant has no credentials. New methods:
   `ListTenantsForWallet`, `GetTenant`, `DimoToken`, `Entitlements`,
   `LoginTouch`.
4. **fleet-lite: listing, middleware, mirror, minter, sync** — the decisions
   above, one PR.
5. **fleet-lite: member/invitation write-through** — the divergence fix,
   separate PR so it reviews on its own merits.

**Deploy order is the usual one: this service first**, then the registration
row, then fleet-lite. The fleet-lite changes are inert until both exist —
managed tenants simply keep 403ing as they do today — so a half-deployed state
is the current state, not a new failure mode.

## Appendix — registering fleet-lite's service identity

Run once against prod (through the tunnel), after this service's PR deploys
and before fleet-lite's. Idempotent: the guard makes a re-run a no-op.

```sql
BEGIN;

-- fleet-lite-app's own identity: the Login-with-DIMO license. The private key
-- lives ONLY in prod/fleet-lite-app's DIMO_AUTH_PRIVATE_KEY; this row carries
-- no key material because nothing ever mints AS this identity server-side —
-- it exists so layer 2 can resolve the JWT to a caller and layer 3 can see
-- is_service_caller.
--
-- kind='operator' + fleet_lite_enabled=false + zero memberships: it appears
-- in no wallet listing, no fleet-lite tenant list, and no console children
-- list. The backfill never deletes tenants absent from its sources, so a
-- re-run leaves it alone.
WITH app_tenant AS (
  INSERT INTO fleet_tenancy_api.tenants (name, kind, status, entitlement_mode, fleet_lite_enabled)
  SELECT 'fleet-lite-app (service identity)', 'operator', 'active', 'implicit', FALSE
  WHERE NOT EXISTS (
    SELECT 1 FROM fleet_tenancy_api.tenant_credentials
     WHERE lower(dimo_client_id) = lower('0x51dacC165f1306Abfbf0a6312ec96E13AAA826DB'))
  RETURNING id
)
INSERT INTO fleet_tenancy_api.tenant_credentials (tenant_id, dimo_client_id, is_service_caller)
SELECT id, '0x51dacC165f1306Abfbf0a6312ec96E13AAA826DB', TRUE FROM app_tenant;

COMMIT;

-- Verify: exactly one row, is_service_caller = true.
SELECT t.id, t.name, c.dimo_client_id, c.is_service_caller
  FROM fleet_tenancy_api.tenant_credentials c JOIN fleet_tenancy_api.tenants t ON t.id = c.tenant_id
 WHERE c.is_service_caller;
```

Worth stating because the flag is powerful: this credential may act on ANY
tenant across the whole `/v1` surface, including minting any tenant's
dimo-token. That is precisely fleet-lite's job — it is the customer product
serving every managed tenant — and the key never leaves fleet-lite's own
secret. The same client id is configured in b2b as the frontend Login-with-DIMO
app id, but b2b holds no key and cannot authenticate with it.

## Verification gate

- `tenancy-check` still passes for all credentialed tenants (no regression on
  the bounded path).
- A new `fleet-lite tenancy-check -wallet <w>` style probe: list tenants for
  the TRAST member's wallet as the app identity, resolve TRAST, mint its
  dimo-token, list its entitlements — all four from inside the prod pod.
- The real thing: jreate@me.com logs into fleets.dimo.co, lands in TRAST, sees
  exactly vehicle 190171, and telemetry loads. Then `tenancy-diff` and
  `groups-diff` re-run clean.
