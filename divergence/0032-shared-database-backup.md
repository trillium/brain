---
id: 0032
title: back a shared database up once — one destination, one throttle, one lock
isc: []
status: proposed
created: 2026-10-09
updated: 2026-10-09
commits: []
touches:
  - cmd/bd/backup_auto.go
  - cmd/bd/backup_shared.go
  - cmd/bd/backup.go
  - cmd/bd/backup_dolt.go
  - internal/storage/versioncontrolops/shared_backup.go
  - internal/storage/versioncontrolops/shared_backup_test.go
  - docs/CONFIG.md
  - divergence/0032-shared-database-backup.md
upstream_rebase_notes: |
  Upstream's auto-backup (`maybeAutoBackup`, `runBackupExport`,
  `DoltStore.BackupDatabase`) is untouched. The only edits in upstream files
  are additive: one early `if maybeSharedAutoBackup(ctx) { return }` in
  `cmd/bd/backup_auto.go`, a shared-database branch at the top of `bd backup
  status` (`cmd/bd/backup.go`) and in `bd backup sync` (`cmd/bd/backup_dolt.go`).
  Everything else is brain-only: `cmd/bd/backup_shared.go` and
  `internal/storage/versioncontrolops/shared_backup*.go` — resolve `ours`. A
  database without `brain_unified_config` never reaches any of it.
---

# Why

Since the switchover every store on the serving machine reads one merged Dolt
database, `brain_unified`. bd's automatic backup still ran per store: each
store's bd registered a `backup_export` destination at its own `.beads/backup`
and kept its throttle there, so one database was backed up up to ~50 times onto
the same disk (15 GB of backup folders on a 97%-full disk, and an auto-backup
timeout). The interim was `backup.enabled: false` plus a launchd job running
`bd backup sync` on a schedule — a hand-made substitute for a feature bd should
have. This makes bd's own backup do it.

# What changed

When the connected database is shared, auto-backup (`maybeAutoBackup`) hands off
to `RunSharedBackup` (`internal/storage/versioncontrolops/shared_backup.go`)
instead of the per-store path. A database used by one store never gets there and
behaves exactly as upstream.

## How a shared database is recognised

By the database itself: it carries the `brain_unified_config` table — the same
signal `issueops.UnifiedScope` uses for every other unified-database behaviour
(prefix ownership, config routing). It is checked by a direct
`information_schema` query on the store's connection.

Why that and not a setting: the question is a property of the database, and
every store on it must give the same answer. A setting lives in a store's own
`config.yaml`; 50 stores means 50 places to set it, and one store that forgets
makes a private copy of the shared data — the exact failure being removed. The
table is created by the unify build and cannot disagree between stores. Not
`BD_NAME`: a wrapper without it (or an operator's `bd`) is on the same shared
database and must back it up the same way.

The probe's error is not folded into "not shared" (`UnifiedScope` degrades to
legacy on a probe error; that is right for routing and wrong here): an
unanswerable probe refuses.

## Destination

The `default` entry of `dolt_backups` — the name `bd backup init` registers and
`bd backup sync` pushes to. Registrations live on the database, so every store
sees the same one. Auto-backup does not register anything on a shared database,
so nothing is pointed back and forth. A stale `backup_export` left by the
per-store path is ignored (`default` wins) and never used.

## Throttle and change detection

One JSON row (`last_dolt_commit`, `timestamp`, `destination`) in
`brain_shared_backup_state`, a table of its own registered in `dolt_ignore`
before it is created. A dedicated table because `local_metadata` — the obvious
home — is only dolt-ignored in databases that ran bd's migration 0028, and the
merged database does not carry bd's `dolt_ignore` rows (found on the copy: its
`local_metadata` is tracked). Writing the state to a tracked table would commit
it, move HEAD, and make the next command back up again, forever. Verified:
after a sync, HEAD does not move and the next command reports `unchanged`.

The first sync on a database adds the one `dolt_ignore` row; the next ordinary
auto-commit commits it (one HEAD move, once).

HEAD is read before the sync, so a commit that lands during the sync is never
recorded as backed up.

## Exclusion between stores

A Dolt/MySQL named lock, `GET_LOCK('bd-shared-backup:<database>', 0)`, on a
pinned connection (the lock is session-scoped) — the mechanism
`schema.MigrateUpWithLock` already uses. Nothing was invented: no lock file, no
table. Auto-backup does not wait: a store that finds the lock held reports
`in-progress` and moves on, since the holder is doing the work. After taking the
lock the store reads the state and HEAD again — the holder may have just
finished — and only then syncs. The lock is freed by the server if the process
dies. A manual `bd backup sync` takes the same lock and waits up to two minutes,
and records the same state, so manual and automatic syncs never overlap and the
throttle sees both.

## Refusals (all skip the backup and print `auto-backup REFUSED — no backup was taken: …` to stderr, not gated by `--quiet`)

1. The shared-database probe cannot be answered.
2. `dolt_backups` cannot be read.
3. No `default` destination (nothing registered, or only other names, which are
   listed) — tells the operator to run `bd backup init <path>`.
4. The state row is unreadable (not JSON, wrong table shape, query error).
5. The state table cannot be registered in `dolt_ignore` / created — refused
   *before* the sync, so a database that cannot hold the state never syncs on
   every command.
6. The lock cannot be taken (error, not "held").
7. No connection to a server (embedded Dolt): not refused, not shared — an
   embedded database cannot be used by another process.

A sync that fails is a plain warning (not a refusal) and leaves the state alone,
so the next command retries.

## Not changed

Per-store `.beads/backup` folders already on disk, off-machine destinations (a
`default` registered with any scheme syncs the same way), the launchd job, and
`bd backup restore`/`remove`. `bd backup status` shows the shared record on a
shared database.

# Follow-ups

- Delete the existing per-store backup folders once the shared backup is
  verified (not done here).
- `bd backup init` on a shared database also writes the per-store
  `.beads/dolt-backup.json`; harmless, but `status` no longer reads it there.
- `bd backup remove` on a shared database removes the shared `default`
  registration for every store (that is what the registration is); it leaves
  the state table.
- `bd backup restore` registers the folder it restored from as `default` (upstream
  behaviour), so on a shared database a restore re-points the shared destination
  at that folder. Unchanged here; an operator restoring should expect it.

# Proof

Behavioural, on copies: `/Users/mini0/fm_home/mini0-ops/data/brain-shared-db-backup/report.md`.
