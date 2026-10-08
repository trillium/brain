---
id: outbox
title: bd outbox
slug: /cli-reference/outbox
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc outbox`

## bd outbox

Operator verbs for the durable event outbox.

Every write command records one event per mutated issue in the event_outbox
table inside the store's own database — the same Dolt commit as the mutation —
and delivery retries each event with bounded backoff (1s, 2s, 4s, ... capping
at 1h) until every configured subscriber acknowledges it. A subscriber being
down delays an event; it never loses one.

  bd outbox list                      the backlog: pending rows, attempts, errors, bound
  bd outbox deliver                   one delivery pass over due events
  bd outbox ack &lt;seq&gt; --subscriber X  record an acknowledgement by hand
  bd outbox record &lt;id&gt;... --command C  record the event of a mutation that has none
  bd outbox purge --before &lt;RFC3339&gt;  delete retired events older than this

Configuration (config.yaml):
  change-events.outbox.enabled: true
  change-events.outbox.subscribers:
    - "name=pulse;url=http://127.0.0.1:8099/events"
    - "name=archive;command=/usr/local/bin/archive-event"

An acknowledgement is an HTTP 2xx (within change-events.outbox.timeout) for a
url subscriber, exit 0 for a command subscriber, or 'bd outbox ack'. Only an
acknowledgement retires an event; an unacknowledged event stays visible in
'bd outbox list' forever.

```
bd outbox [flags]
```

### bd outbox ack

Record an acknowledgement for one outbox event by hand.

This is the operator escape hatch for a subscriber that will never acknowledge
(decommissioned, or replaced by a renamed one) and for repairing a corrupt
row. The event retires when every subscriber frozen on it has acknowledged;
pass a name not on the row with --force to record it anyway.

```
bd outbox ack <seq> --subscriber <name> [flags]
```

**Flags:**

```
      --force               Acknowledge even when the subscriber is not frozen on the row, or the row is corrupt
      --subscriber string   Subscriber that acknowledged (required)
```

### bd outbox deliver

Deliver due outbox events to their configured subscribers.

Each pending event whose next-attempt time has arrived is attempted, once per
subscriber. Success (HTTP 2xx / exit 0) records the acknowledgement; an event
whose every frozen subscriber has acknowledged retires. Failure leaves the
event pending with its next retry scheduled by the bounded backoff (1s, 2s,
4s, ... capping at 1h) and the attempt's error recorded on the row. Nothing is
ever dropped: run again (loop, cron, or the automatic pass after each write)
until the subscriber comes back.

```
bd outbox deliver [flags]
```

**Flags:**

```
      --all   Attempt every pending event, ignoring the backoff schedule
```

### bd outbox list

List outbox events with the delivery state of each.

The summary line shows what the backoff-and-bound story is anchored on: events
not yet retired (pending) against change-events.outbox.max-pending. Reaching
the bound refuses new writes loudly; see docs/brain/event-outbox.md.

```
bd outbox list [flags]
```

**Flags:**

```
      --pending-only   Only unretired events
```

### bd outbox purge

Delete retired outbox events (delivered and acknowledged) whose
delivered_at is older than --before. Pending events are never touched: the
outbox only shrinks by acknowledgement, and only the operator deletes record.

```
bd outbox purge --before <RFC3339> [flags]
```

**Flags:**

```
      --before string   Delete retired events delivered before this RFC3339 timestamp (required)
```

### bd outbox record

Record outbox events by hand for issues whose mutation committed but whose
event was not recorded — the repair for the one window the outbox cannot close
(a process killed between the store's mutation commit and the event insert) and
for a command that failed with "its event was NOT recorded".

The events are subject to the same rules as automatic ones: subscribers are
frozen from the current configuration and the events are delivered like any
other. Requires change-events.outbox.enabled.

```
bd outbox record <issue-id>... --command <name> [flags]
```

**Flags:**

```
      --command string   Command whose mutation is being recorded (required)
```
