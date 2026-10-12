---
id: 0034
title: small fixes from the adversarial review of the 2026-10-07..09 changes
isc: []
status: proposed
created: 2026-10-10
updated: 2026-10-10
commits: []
touches:
  - cmd/bd/outbox_delivery.go
  - cmd/bd/outbox_proc_unix.go
  - cmd/bd/outbox_proc_windows.go
  - cmd/bd/brain_unify.go
  - internal/hooksdef/exec.go
  - internal/hooksdef/running.go
  - internal/storage/hook_guard_decorator.go
  - internal/storage/versioncontrolops/shared_backup.go
  - internal/brainunify/replay.go
  - docs/brain/HOOKS.md
  - docs/brain/README.md
  - divergence/README.md
  - divergence/0024-isa-retirement.md
  - divergence/0034-review-fixes-hooks-outbox-backup.md
upstream_rebase_notes: |
  All brain-only code except `internal/storage/hook_guard_decorator.go`
  additions (new methods; no upstream file is edited). Resolve `ours`.
---

# Why

An adversarial review of the three days of changes before this one found
small, clear defects. This fixes the ones that need no design decision. The
unify cutover blockers (silent child-row loss on identical duplicates, replay
resurrecting or overwriting merged rows, prefix release atomicity) are left for
a decision.

# What changed

- Outbox command subscribers: `WaitDelay` (2s), their own process group killed
  on timeout, and an environment scrubbed of `BEADS_`/`BRAIN_`/`BD_`/`DOLT_`/
  `MYSQL_` (the hooks scrubber, minus the hook marker). A clean exit that leaves
  a background child still acknowledges.
- Hook guard: `ClaimIssue`, `ClaimReadyIssue`, `DeleteIssues` and
  `UpdateIssueID` now run guards (and observers). A test fails when a
  `DoltStorage` method is neither wrapped nor on a named allowlist.
- Hook ancestry: `ps` by absolute path; markers found with `ReadDir`, not
  `Glob`; an unreadable store directory counts as "a hook is running".
- Shared backup: a destination different from the last synced one is never
  "unchanged".
- `unify replay` keep-existing inserts use `on duplicate key update`, not
  `insert ignore`, so bad data errors instead of being coerced.
- `unify build --project-id` help says a missing id warns (it does not refuse).
- Docs: dangling sha in 0024, wrong link to the outbox note, divergence index
  brought up to 0033.

# What is lost

Nothing intended. `ClaimReadyIssue`'s guard payload has no subject (the store
picks the bead); observers see the claimed bead.

# Brain-spec link

[VISION.md](../VISION.md)
