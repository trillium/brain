---
id: 0042
title: bookkeeping issues expire after a retention period once closed
isc: []
status: proposed
created: 2026-11-03
updated: 2026-11-03
commits: []
touches:
  - cmd/bd/bookkeeping.go
  - cmd/bd/bookkeeping_test.go
  - cmd/bd/bookkeeping_store_test.go
  - cmd/bd/cleanup.go
  - docs/CONFIG.md
  - website/docs/cli-reference/admin.md
  - divergence/0042-bookkeeping-expiry.md
upstream_rebase_notes: |
  `cmd/bd/bookkeeping*.go` are brain-only. `cmd/bd/cleanup.go` gains a
  `--bookkeeping` flag, a branch in `RunE` that returns early into
  `runBookkeepingCleanup`, and the help text; the existing flow is otherwise
  unchanged, so an upstream edit to cleanup should merge cleanly. No schema
  change: the class is an ordinary label plus one config key.
---

# Why

Most of what accumulates in a store is procedural: session trails, guard
decisions, state-change records. Nobody reads it after the fact, yet it is kept
forever next to the deliberately written issues, which adds clutter and disk
pressure. Gas City treats workflow runs as throwaway and garbage-collects
closed ones on a retention setting; brain needs the same for its own
procedural records, without ever risking a hand-written issue.

A label keeps the class separate from deliberate issues, needs no new issue type
or schema, and can be applied by any writer. The existing `admin cleanup`
already owns "closed issues to deleted", so expiry lives there instead of a
second deletion path.

# What changed

- `bd admin cleanup --bookkeeping` considers only closed issues labeled
  `bookkeeping` whose close time is older than the retention period.
- Retention is `--older-than N`, else the `bookkeeping.retention_days` config
  key, else 30 days. A malformed configured value is an error, not a fallback.
- `--dry-run` lists what would be removed; without `--force` or `--dry-run` the
  command refuses, as plain cleanup does.
- Open issues, unlabeled issues and pinned issues are never selected. The
  selector re-checks status, label and close time itself rather than trusting
  the storage filter alone.
- Behavioral tests cover retention resolution, the selection boundaries and a
  store-backed run (skipped when no test server is available).

# Brain-spec link

[`VISION.md`](../VISION.md)
