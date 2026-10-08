# Durable event outbox

> Every change emits an event, and events are durable: they are delivered to
> subscribers with retries until acknowledged. A subscriber being down delays
> an event; it never loses one. The event log is part of the record, not a side
> channel to it. — [VISION.md](../../VISION.md)

Before this feature, a mutation's event was fire-and-forget: the opt-in
`change-events.jsonl` line, or a webhook push, was gone if the listener was down.
The outbox makes the emitted event a **row in the store's own database** and
delivers it with retries until every subscriber has acknowledged it.

Server-mode (`dolt sql-server`) stores only. An embedded store does not expose a
SQL handle, so with the outbox enabled every write is refused with a named error
rather than emitting an event that is not part of the record.

## Turn it on

`.beads/config.yaml`:

```yaml
change-events:
  outbox:
    enabled: true
    max-pending: 10000        # the bound (default 10000; <= 0 disables it — not recommended)
    timeout: 10s              # per-attempt acknowledgement window (default 10s)
    delivery-budget: 5s       # wall-clock cap on the automatic delivery pass after a write (default 5s)
    subscribers:
      - "name=pulse;url=http://127.0.0.1:8099/events"
      - "name=archive;command=/usr/local/bin/archive-event"
```

A subscriber is `name=<n>;url=<u>` (HTTP POST) or `name=<n>;command=<path>`
(executable, event on stdin). Names are letters, digits, `.` `_` `-`. Anything
else — unknown field, missing name, both or neither of `url`/`command`,
duplicate name — is a named refusal (see [Refusals](#refusals)).

With no subscribers configured events are still recorded, and retire at
emission: the record of what was emitted exists even when nobody listens yet.

## What an event is

One row in `event_outbox` per mutated issue per write command:

```json
{"ts":"2026-10-08T22:15:09Z","store":"task","command":"create","id":"task-abc","seq":1}
```

`seq` is the row's sequence number in that store's database. Delivery is
at-least-once, so subscribers dedupe on **(`store`, `seq`)**.

The table lives in the store's database, next to the data it describes. It is
staged and committed on its own (`event outbox: <command>` commits stage only
`event_outbox`), so it travels with the store through Dolt push, pull and backup
and can be diffed with `dolt_diff_event_outbox`. **The store is authoritative.**
`change-events.jsonl`, if you enable it, is a convenience tail target — it can
lag or be deleted; the outbox cannot drift from the store because it *is* in the
store.

## What is atomic

- All events of one command are inserted in **one transaction**: a command's
  events exist together or not at all.
- If the events cannot be recorded the command **fails loudly** (non-zero exit,
  named error) — never a silent eventless mutation.
- Every foreseeable refusal (unreadable subscriber definition, backlog at the
  bound, no SQL handle, unparseable config) happens in a **preflight before the
  command mutates anything**.
- **The one window this does not close:** the event is inserted by the command's
  post-run, a second transaction after the store's own mutation commit. A process
  killed (SIGKILL, power loss) in the milliseconds between them leaves a
  committed mutation without an event. That is not hidden: repair it with
  `bd outbox record <issue-id> --command <name>`, which a failed command's error
  message also names. Closing the window needs the event insert inside the
  store's mutation transaction, which touches the shared write path; it is left
  as a follow-up rather than done here.

## Acknowledgement

An event is **retired** only when every subscriber that was configured *when it
was emitted* has acknowledged. Nothing else retires it — not time, not attempt
count, not backlog pressure.

| Subscriber | Acknowledged when |
|---|---|
| `url=` | the POST gets an HTTP **2xx** within `timeout` |
| `command=` | the executable **exits 0** within `timeout` (event JSON + newline on stdin) |
| either | an operator runs `bd outbox ack <seq> --subscriber <name>` |

What happens otherwise:

- **Late** (answers after `timeout`): the attempt counts as failed. The event is
  retried and the subscriber receives it again — hence at-least-once and
  idempotent subscribers. A late 2xx is never recorded.
- **Twice** (the same subscriber acknowledges again, by delivery or by
  `bd outbox ack`): idempotent. The first acknowledgement timestamp stands.
- **Never**: the event stays `awaiting <subscriber>` in `bd outbox list` forever,
  with its attempt count, next attempt time and last error. It counts toward the
  bound. Nothing drops it; only an acknowledgement (including a deliberate
  operator one) retires it.
- **Several subscribers**: each one's acknowledgement is recorded separately; a
  subscriber that has acknowledged is not sent the event again while others are
  still outstanding.
- **A subscriber removed from config** while events await it: those events stay
  pending with a named error (`subscriber "x" frozen on this event is not defined
  …`). Re-add it, or `bd outbox ack <seq> --subscriber x`.

## Retry

After the *n*-th failed attempt the next attempt waits `min(1s · 2^(n-1), 1h)`:
1s, 2s, 4s, 8s, … capped at one hour. The delay is bounded; the attempt count is
not — a subscriber that never returns is retried hourly, forever, and the event
is never lost. Attempts happen:

1. automatically after each write command (due events only, within
   `delivery-budget`; a failure is a warning on stderr, never a failed command);
2. by `bd outbox deliver` (due events), or `bd outbox deliver --all` (ignore the
   backoff schedule — use it right after fixing a subscriber).

There is no daemon. If nothing writes and nobody runs `bd outbox deliver`, nothing
is retried — run it from cron or a loop if you want delivery without writes.

## The bound and the backlog

`max-pending` (default **10000**) bounds events **not yet retired**. When
`pending + 1 > max-pending`, any command that might write is **refused before it
mutates**:

```
Error: event outbox full: 3 pending events (1 more queued by this command) exceed
the bound change-events.outbox.max-pending=3 — run 'bd outbox deliver', fix or
'bd outbox ack' the subscriber, or raise the bound; refusing the command before
it writes rather than recording an event that may never be delivered
```

Nothing is dropped and nothing is throttled silently. The operator can always
get out: the `outbox` verbs, `config`, `vc`, `dolt`, `doctor`, `version`, `help`,
and the read-only commands are exempt from the check. A command that mutates
several issues can overshoot the bound by its own mutation count — an event is
never dropped to enforce the bound.

See the backlog: `bd outbox list` (`--pending-only`, `--json`). The first line is
the summary, `pending N of bound M`; at the bound a `BACKLOG AT BOUND` line
follows. Each row shows the attempt count, next attempt time, who is awaited and
the last error.

Retired events stay (they are record) until `bd outbox purge --before <RFC3339>`
deletes retired events delivered before that time. Pending events are never
purged.

## Verbs

| Verb | Does |
|---|---|
| `bd outbox list [--pending-only] [--json]` | the backlog, state of every event, corrupt rows by name |
| `bd outbox deliver [--all]` | one delivery pass over due (or, with `--all`, all) pending events |
| `bd outbox ack <seq> --subscriber <name> [--force]` | record an acknowledgement by hand; `--force` for a subscriber not frozen on the row, or to retire a corrupt row |
| `bd outbox record <issue-id>... --command <name>` | record the event of a mutation that has none |
| `bd outbox purge --before <RFC3339>` | delete retired events older than the time |

## Refusals

Each is loud and named; none is a warning that lets the write proceed.

| Condition | Result |
|---|---|
| unusable subscriber definition (unknown field, no name, both/neither of url+command, bad name, duplicate name) | write refused before it mutates; `deliver` refuses too |
| backlog at the bound | write refused before it mutates |
| embedded store (no SQL handle) | write refused |
| `config.yaml` cannot be parsed and the store already has `event_outbox` | write refused (bd would otherwise fall back to defaults with the outbox silently off) |
| events cannot be inserted after a mutation | non-zero exit; the error says the mutation may be committed and names `bd outbox record` |
| outbox rows cannot be committed to history | non-zero exit (the rows are still durable in the working set) |
| corrupt row (unparseable subscribers/acks/payload) | named in `list`/`deliver`; healthy rows still flow; retire deliberately with `ack --force` |
| delivery cannot be attempted (executable missing, URL unbuildable) | recorded in the row's `last_error`; the event stays pending with backoff |
| subscriber frozen on an event is no longer defined | recorded in `last_error`; stays pending |
| `ack` for an unknown seq / unfrozen subscriber; `purge` without `--before`; `record` without `--command` | refused with the missing piece named |

## What this does not do

- It does not deliver without a trigger (no daemon — see Retry).
- It does not order delivery across events: retries interleave. Use `seq`.
- It does not cover embedded stores.
- It does not close the mutation/event window described above.
- It does not replace the federated-event webhook wrapper; a webhook can be an
  outbox `url=` subscriber, which is how it becomes durable.

Design record: [`../../divergence/0029-durable-event-outbox.md`](../../divergence/0029-durable-event-outbox.md).
