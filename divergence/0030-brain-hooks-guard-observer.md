---
id: 0030
title: hooks — declared guards refuse a write, observers fail open, unset hooks warn with a durable record
isc: []
status: built
created: 2026-10-09
updated: 2026-10-09
commits: []
touches:
  - internal/hooksdef/
  - internal/storage/hook_guard_decorator.go
  - internal/storage/hook_guard_decorator_test.go
  - cmd/bd/hook.go
  - cmd/bd/main.go
  - cmd/bd/errors.go
  - cmd/bd/context.go
  - docs/brain/HOOKS.md
  - docs/CLI_REFERENCE.md
  - website/docs/cli-reference/hook.md
  - website/docs/cli-reference/index.md
upstream_rebase_notes: |
  New code is brain-only (internal/hooksdef, hook_guard_decorator.go,
  cmd/bd/hook.go). Hotspots: cmd/bd/main.go PersistentPreRunE (a hook-loading
  block after `useReadOnly`, and a one-line wrap beside NewHookFiringStore —
  the guard decorator must stay the INNERMOST wrapper); cmd/bd/errors.go
  CheckReadonly and cmd/bd/context.go isReadonlyMode (each consults
  insideHook()). The legacy `.beads/hooks/on_*` runner and the git-hook
  installer (`bd hooks`) are untouched; the new verb is `bd hook`, singular.
---

# Why

The captain's approved vision: "A hook may refuse a write before it happens,
and the refusal is surfaced as the reason the bead was never created", and a
hook's failure policy is declared per hook, with an unset hook warning, because
"if an agent runs hook -> warn -> agent might just move on". Before this, hooks
were not a mechanism: the exfiltration decorator sat inside the write path,
`.beads/hooks/on_*` scripts fired fire-and-forget after the fact, and nothing
could refuse a write.

# What changed

- **Declaration**: `<beadsDir>/hooks.d/<name>.toml` (`run`, `policy`, `when`,
  `timeout`, `refusal_exit`), discovered by listing the directory at command
  start — no daemon, no registry, no hidden directory. Any unreadable or
  ambiguous file (bad TOML, unknown key/policy/when, name mismatch, duplicate,
  stray file) refuses every command that could write, naming the file.
- **Policies** (`internal/hooksdef.Decide` is the one place they live): `guard`
  runs before the write; exit 0 passes, the refusal exit refuses, every other
  outcome (exit, timeout, would-not-start) fails closed. `observer` runs after
  the write; any failure fails open and records a warning. No policy: runs like
  an observer but warns on every run, pass or fail.
- **Where**: `storage.HookGuardStore`, innermost decorator
  (raw → guard → HookFiringStore → exfiltration), covering create/update/close/
  delete/label/dependency/comment, bulk create, and every Transaction method
  (guards refuse inside the transaction, so it rolls back; observers fire after
  commit).
- **Warnings are records, not stderr lines**: config-table rows
  `hookwarning.<id>` (JSON), written by the store layer, `open` until
  `bd hook warnings --ack`. No schema migration; versioned and synced with the
  database.
- **Hooks cannot write**: no store handle is ever passed; the hook environment
  drops BEADS_/BD_/BRAIN_/DOLT_/MYSQL_ variables and runs in an empty temp dir;
  the process carries `BD_INSIDE_HOOK`, and `CheckReadonly`/`isReadonlyMode`
  refuse writes for it; and a `hooks-running.<pid>.lock` marker in the store's
  `.beads` makes any `bd` write whose process ancestry includes the hook-running
  `bd` refuse, so scrubbing the marker variable does not help.
- **CLI**: `bd hook list | warnings [--ack|--ack-all] | test`.

# Left out on purpose

- Routing. The warning record is the emission side; nothing notifies anyone.
- Bulk paths that bypass the guarded methods (import, dolt merge/pull,
  compaction, DeleteIssuesBySourceRepo) and proxied-server mode (writes are
  refused when hooks are declared there, rather than run unguarded).
- A hook that daemonises past its own exit, or an operator who edits the
  database directly, is outside the mechanism; an OS-level sandbox would be the
  next layer if that ever matters.
