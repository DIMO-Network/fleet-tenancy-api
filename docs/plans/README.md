# Plans

Work that is decided but not yet built, or being built across more than one
repo. A plan lives here from the moment the shape is agreed until the last PR
merges, and then it is deleted — the record of what was done belongs in
[`../HANDOFF.md`](../HANDOFF.md) and in commit messages, not here.

Distinguish these from [`../operator-tenancy/`](../operator-tenancy/), which is
the *design*: locked decisions and the reasoning behind them, kept indefinitely.
A plan is the execution path to a design, and it goes stale the moment it is
finished.

Statuses are only true if they are maintained. This table was stale for a week
— it called the groups move unstarted while it ran in production, and called
plan 07 step 1 an open prod bug after it had been fixed and released. **Update
the row in the same PR that ships the step**, not later.

| Plan | Status |
|---|---|
| [01-groups-into-tenancy.md](01-groups-into-tenancy.md) | P1–P4 live, P5a landed; **P5b (drop the local tables) outstanding**, blocked on kaufmann's `access_fleet_groups` FK |
| [02-vehicle-memberships.md](02-vehicle-memberships.md) | **Done** — steps 1–6 shipped; enforced in prod |
| [03-fleet-lite-operator-tenants.md](03-fleet-lite-operator-tenants.md) | Planned 2026-08-15, nothing shipped |
| [04-invitations-into-tenancy.md](04-invitations-into-tenancy.md) | P1 deployed, P2 backfilled 2026-08-16; **flag flip outstanding** (`INVITES_FROM_TENANCY` unset in prod) |
| [06-signer-key-consolidation.md](06-signer-key-consolidation.md) | **Step 1 done 2026-08-21** — `signer-diff` reports `differ=0`, all 11 signer pairs agree, stored addresses match; steps 2–6 unblocked |
| [07-vehicle-roster.md](07-vehicle-roster.md) | Steps 1–3 done and live; **step 4 done** — both readers cut over and ON in prod (fleet-lite 2026-08-20 14:17, kaufmann `v1.53.0` + flip 20:20 UTC); neither path exercised by real traffic yet; step 5 not started |

## Writing one

State what is wrong now with evidence, what the destination is, and the order
of the steps with what each one costs if it goes wrong. Record the things that
were considered and rejected — the next person will otherwise re-propose them,
and the reasoning is the expensive part.
