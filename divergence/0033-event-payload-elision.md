---
id: 0033
title: cap string values in audit `events` rows so appends stop growing the table quadratically
isc: []
status: proposed
created: 2026-10-09
updated: 2026-10-09
commits: []
touches:
  - internal/storage/issueops/event_payload.go
  - internal/storage/issueops/event_payload_test.go
  - internal/storage/issueops/update.go
  - internal/storage/issueops/claim.go
  - internal/storage/domain/db/issue.go
  - docs/CONFIG.md
  - CHANGELOG.md
  - divergence/0033-event-payload-elision.md
upstream_rebase_notes: |
  Three upstream call sites change from `json.Marshal` of the old issue and the
  applied updates to `issueops.MarshalEventPayloads`: `issueops/update.go`,
  `issueops/claim.go`, `domain/db/issue.go`. Each is a few lines; re-apply them
  by hand if upstream touches the event-writing code. `event_payload.go` and its
  test are brain-only — resolve `ours`.
---

# Why

Every update writes an `events` row holding a JSON copy of the issue before the
change and a JSON copy of the applied updates. For an append-heavy field such as
`notes`, each append rewrites the whole field, so the row is as big as the field
and the table grows quadratically in appends.

Measured on a copy of the unified database: 193,134 events rows, 8.46 GB of raw
payload (`length(old_value)+length(new_value)`), of which 8.0 GB is `updated`
rows on the `lifespan` namespace. One bead (`lifespan-fv1kk`, 568 KB of notes)
carries 7,976 events = 4.7 GB. A single 37-byte `lifespan note` append wrote
about 1.2 MB of event payload.

The cost is raw payload, not disk: the whole copy is 767 MB on disk because
Dolt dedupes the repeated prefixes. What the bloat costs is row size, scan time,
the copy and dump paths (`internal/brainunify/dbsource.go` already batches
around "a single events row can carry a whole session transcript"), and a
growth rate that is quadratic in appends.

# What changed

`MarshalEventPayloads` caps every string value in the two payloads at
`BEADS_EVENT_FIELD_LIMIT` bytes (default 1024). An oversize value keeps its head
and tail around an `…[bd: elided N bytes]…` marker; a field that was appended to
collapses its unchanged leading bytes to a marker, so the row costs bytes
proportional to the appended text. The cap is strict (markers count against it)
and output is always valid UTF-8.

Same append after the change: 812 bytes of event payload instead of about
1.2 MB, with the note intact in `issues.notes`.

# What is lost

- The full before/after copy of any single field over the limit, in the audit
  row. The full value is still in `issues` and in Dolt's own commit history;
  nothing in brain reads the full copy from `events` (event outbox, change
  events, edit-back and unify build/replay/verify were checked).
- Appended tails survive, so "what was added" is still recorded.
- `BEADS_EVENT_FIELD_LIMIT=0` restores the old full-copy behaviour.

# Default-on for every store

The cap applies to every store, including small ones: a description over 1 KB
is now elided in its audit row. That is accepted; it is noted in the changelog.
