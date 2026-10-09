# Releasing a prefix claim — `bd store-prefix release` and `history`

A prefix claim (`bd store-prefix add`) used to be one-way: the record was never
rewritten, so a store that wanted to give a prefix up had no verb. Release is
that verb. It does **not** reopen silent rewriting — every ownership change,
a claim or a release, appends a row to an append-only event table in the same
transaction as the change, so there is no ownership change without a trail.

The design and its decisions: [`../design/brain-prefix-release.md`](../design/brain-prefix-release.md).
Command text: [`../CLI_REFERENCE.md`](../CLI_REFERENCE.md#bd-store-prefix).

## Release

```
<store> store-prefix release <prefix> --confirm [--reason "why"]
```

Run it under the **owning store's own wrapper** (`BD_NAME` must equal the
owner). There is no `--store` escape on this side: `add` may claim on another
store's behalf, release may not, because a mistaken release is subtractive.
`--confirm` is required and is the reason the act cannot happen as a side
effect; `--reason` (≤ 64 characters) is recorded verbatim, `released` when
omitted.

On success the ownership row is deleted — the prefix is back to exactly its
pre-claim state, claimable by anyone through the ordinary `add` — the owner's
`allowed_prefixes` loses it, and a `release` event is written. Release is
refused, loudly, naming the prefix and the reason, when:

| Refusal | Why |
|---|---|
| no `--confirm` | intent must be explicit |
| the prefix is not recorded | a release of nothing must be an error, not a success-shaped no-op |
| the caller is not the owner's namespace | authority is namespace identity |
| the prefix is **build-decided** (`declared-by-store-config`, `store-name-matches-prefix`, `first-observing-source`, `no-source-declares-or-matches`) | retracting the unify build's mapping is a re-unification act, not namespace maintenance |
| the prefix still carries **any** bead | absolute — see below |

None of these is bypassable: there is no `--force`, no `--with-beads`.

### Why the beads refusal is absolute

A prefix that still carries beads cannot be released. If it could, a second
store could claim the prefix and ownership of the existing beads would become
ambiguous; and "refuse rather than guess" is the posture of the whole record.
A prefix frees only when it carries no beads, and migrating the beads off it
first is the explicit, auditable path. The beads are counted live from the
merged `issues` table at release time, not from the stale `bead_count` on the
ownership row. If a genuine need for an override ever appears, adding one is a
small reversible change; shipping one first and withdrawing it is not.

### Transfer

A transfer is two acts: the owner releases, the new store claims.

```
old-store store-prefix release acme --confirm --reason "moving to new-store"
new-store store-prefix add acme
```

The unclaimed window between them is real and the next claim wins it; do the
two acts back to back and check `store-prefix list` in between. An atomic
`move` is a named fast-follow, to be built if contested transfers actually
appear — the event vocabulary already reserves `transfer`.

## History

```
<store> store-prefix history <prefix> [--json]
```

Prints the prefix's events newest first: when, what (`claim` / `release`),
who acted, the owner before → after, the decision reason, and the bead count
observed at that moment.

```
2026-10-08T21:56:27Z  claim    actor=brain  owner '' -> brain  reason=operator-added  beads=0  event=…
2026-10-08T21:56:26Z  release  actor=task   owner task -> ''   reason=handoff to brain  beads=0  event=…
2026-10-08T21:56:17Z  claim    actor=task   owner '' -> task   reason=operator-added  beads=0  event=…
```

**The trail starts at adoption.** Claims are recorded from the moment the
event table exists; a runtime claim made before that has no event row and none
is invented for it. `history` says so where you are looking (an empty trail
for an owned prefix prints the current owner, its decision reason, and that
the earlier claim has no event) rather than filling the gap with a fabricated
row. The table is created inside the first transaction that changes
ownership on a unified database that predates it; reading history never
creates it.

## The event record

`brain_store_prefix_events` — append-only, never updated, deleted or compacted:

| Column | Meaning |
|---|---|
| `id` | event id (time-sortable) |
| `event_type` | `claim`, `release` (`transfer` reserved) |
| `prefix` | the prefix |
| `actor` | the namespace that performed the act |
| `old_store` / `new_store` | owner before / after (`''` = none) |
| `reason` | the decision reason (`operator-added`, `store-created`, or the release `--reason`) |
| `bead_count` | live beads under the prefix when the event was written |
| `event_at` | UTC time |

Only unified databases have it; a legacy per-store database has no ownership
record and the verbs refuse there.
