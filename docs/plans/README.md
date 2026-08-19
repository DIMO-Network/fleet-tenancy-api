# Plans

Work that is decided but not yet built, or being built across more than one
repo. A plan lives here from the moment the shape is agreed until the last PR
merges, and then it is deleted — the record of what was done belongs in
[`../HANDOFF.md`](../HANDOFF.md) and in commit messages, not here.

Distinguish these from [`../operator-tenancy/`](../operator-tenancy/), which is
the *design*: locked decisions and the reasoning behind them, kept indefinitely.
A plan is the execution path to a design, and it goes stale the moment it is
finished.

| Plan | Status |
|---|---|
| [01-groups-into-tenancy.md](01-groups-into-tenancy.md) | Agreed, not started |
| [02-vehicle-memberships.md](02-vehicle-memberships.md) | Steps 1–5 shipped 2026-08-14; step 6 next |
| [03-fleet-lite-operator-tenants.md](03-fleet-lite-operator-tenants.md) | Planned 2026-08-15, nothing shipped |
| [04-invitations-into-tenancy.md](04-invitations-into-tenancy.md) | P1 deployed, P2 backfilled 2026-08-16; flag flip outstanding |
| [06-signer-key-consolidation.md](06-signer-key-consolidation.md) | Written 2026-08-19, not started |
| [07-vehicle-set-coherence.md](07-vehicle-set-coherence.md) | Written 2026-08-19, not started; step 1 is an open prod bug |

## Writing one

State what is wrong now with evidence, what the destination is, and the order
of the steps with what each one costs if it goes wrong. Record the things that
were considered and rejected — the next person will otherwise re-propose them,
and the reasoning is the expensive part.
