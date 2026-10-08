---
id: 0027
title: durable event outbox — events are rows in the store, delivered with retries until acknowledged
isc: []
status: landed
created: 2026-10-09
updated: 2026-10-09
commits: [89e1d3aeb, 5af862c76, 4b0e91078, 43a0e0106]
touches:
  - cmd/bd/outbox.go
  - cmd/bd/outbox_cmd.go
  - cmd/bd/outbox_delivery.go
  - cmd/bd/outbox_test.go
  - cmd/bd/outbox_dolt_test.go
  - cmd/bd/main.go
  - internal/config/config.go
  - docs/brain/event-outbox.md
  - docs/CLI_REFERENCE.md
  - website/docs/cli-reference/outbox.md
  - website/docs/cli-reference/index.md
  - divergence/0027-durable-event-outbox.md
upstream_rebase_notes: |
  Brain-only feature, opt-in (`change-events.outbox.enabled`, default off);
  with it off the only behavioural change is one config read per write
  command. New files are `cmd/bd/outbox*.go` — resolve `ours`. Hotspots in
  upstream files are small and additive: `cmd/bd/main.go` (a preflight call
  after the store opens in PersistentPreRunE; record → auto-commit →
  change-event → delivery → outbox-commit in PersistentPostRunE) and
  `internal/config/config.go` (five defaults under change-events.outbox.*).
  The feature deliberately does NOT touch `internal/storage/**` or the write
  path, so a parallel hook mechanism there does not conflict; if a future
  change moves event emission into the store transaction (the follow-up that
  closes the window below), `maybeRecordOutboxEvents` is the function to
  retire. `docs/CLI_REFERENCE.md` and `website/docs/cli-reference/` are
  generated (`scripts/generate-cli-docs.sh`): regenerate, do not merge by hand.
---

# Why

VISION.md: "Every change emits an event, and events are durable: they are
delivered to subscribers with retries until acknowledged. A subscriber being
down delays an event; it never loses one. The event log is part of the record,
not a side channel to it." What existed was fire-and-forget — the `events`
table per store, an opt-in `change-events.jsonl` line per mutated bead, and a
webhook wrapper that pushes federated events off the machine. An event that
vanished because a listener was down is the silent failure the vision names as
the defect class.

# What changed

- **`event_outbox` table in the store's own database** (created on first
  emission; staged and committed on its own as `event outbox: <command>`), one
  row per mutated issue per write command, written by the command's post-run
  in one transaction per command. Rows freeze the subscriber set at emission.
- **Delivery with bounded backoff, retired only by acknowledgement**: HTTP 2xx
  within `timeout` (url subscriber), exit 0 within `timeout` (command
  subscriber), or `bd outbox ack`. Retry delay 1s, 2s, 4s, … capped at 1h;
  attempts unbounded. A late acknowledgement does not count (at-least-once; the
  payload carries `seq` for dedupe); a second acknowledgement is idempotent; a
  subscriber that never acknowledges leaves the event visible forever.
- **Bounded, honest growth**: `max-pending` (default 10000) bounds unretired
  events. At the bound, any command that might write is refused *before it
  mutates* with a named error; nothing is dropped, nothing throttled. The
  operator sees the backlog in `bd outbox list` (`pending N of bound M`).
- **Verbs**: `bd outbox list | deliver | ack | record | purge`.
- **Refusals instead of guesses**: unusable/duplicate subscriber definition,
  backlog at the bound, embedded store, unparseable `config.yaml` on a store
  that already has an outbox, corrupt rows, undeliverable attempts — each loud
  and named (full table in `docs/brain/event-outbox.md`).
- **The store is authoritative.** `change-events.jsonl` remains an opt-in
  convenience tail; it can lag, the outbox cannot drift.
- **Not closed, stated plainly:** the event insert is a second transaction
  after the store's own mutation commit. A process killed in that window leaves
  a mutation without an event; `bd outbox record` repairs it by hand. Putting
  the insert inside the store's mutation transaction touches the shared write
  path (`internal/storage/**`) that a parallel task is changing, so it is a
  follow-up, not part of this entry. Also: no delivery daemon (delivery runs
  after each write and on `bd outbox deliver`), and server-mode stores only.

# Brain-spec link

[VISION.md](../VISION.md), §"Every change emits an event" (durable,
retried-until-acknowledged delivery) and §"A silent failure is the defect
class" (every unresolvable condition is a named refusal). Behaviour,
acknowledgement contract and bound: [docs/brain/event-outbox.md](../docs/brain/event-outbox.md).
