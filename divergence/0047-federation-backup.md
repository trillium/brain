---
id: 0047
title: back every registered store's database up with DOLT_BACKUP and prove each backup on a Dolt server of its own
isc: []
status: proposed
created: 2026-11-16
updated: 2026-11-16
commits: []
touches:
  - cmd/bd/brain_federation_backup.go
  - cmd/bd/brain_federation_backup_test.go
  - cmd/bd/main.go
  - docs/brain/FEDERATION_BACKUP.md
  - docs/brain/README.md
  - docs/CLI_REFERENCE.md
  - docs/cli-reference/brain.md
  - docs/docs.json
  - divergence/0047-federation-backup.md
upstream_rebase_notes: |
  `brain_federation_backup.go` and its test are brain-only files; resolve
  `ours`. `main.go` is upstream with brain edits: the two
  `upgradeCommandCanRunWithoutStore(cmd)` sites gain `|| isFederationBackupVerb(cmd)`;
  re-add the clause if the rebase drops it. The verb builds only on
  `internal/brainupgrade` (brain-only, 0043) and `schema.DrainCall`.
---

# Why

On 2026-08-04 an autonomous job backed the federation up by copying the live
:3307 server's data directory file by file, started two more sql-servers over
it, and broke every store at once (`~/code/firstmate/data/incidents/2026-08-04-dolt-federation-outage.md`,
`robots-15qy`). The captain's standing order since then: a federation backup
that never snapshots the live server by file copy. bd's own `backup`
family (0032) backs one store's database up, on the schedule of that store's
commands, into `<store>/.beads/backup`: inside `~/data`, one store at a time,
and in server mode only when `backup.enabled` is set. Nothing iterated the
registry, proved a backup, or refused a bad destination.

# What

`bd brain federation-backup` (alias `fedbackup`):

- iterates the registry `brain stores list` reads, collects every database
  the stores' metadata names (each once: every live store shares
  `brain_unified`), confirms each with a read-only `SHOW DATABASES`;
- backs each up with `CALL DOLT_BACKUP('sync-url', 'file://<dest>/<db>')`,
  so the server writes the backup and this process never opens the data
  directory; a rerun syncs into the same directory;
- proves each backup: lays it out on a `dolt sql-server` of its own (free
  loopback port, scratch under `<dest>/.scratch`, stopped and removed
  afterwards), compares `dolt_hashof_db`, HEAD and every table hash with the
  pre-sync reading (or, when the database changed during the sync, requires
  the pre-sync HEAD in the backup's history and every table present), then
  runs `bd list --json --limit 1` with this binary for every store that lives
  in the database, the way the store's wrapper would, with a scratch
  `.beads/metadata.json` and all `BEADS_*`/`BD_*`/`BRAIN_*` env dropped;
- refuses `--dest` inside a Dolt database (`.dolt/noms`), a data directory
  (`.doltcfg`, or a database child), or a registered store's directory;
- reports a table and JSON (`<dest>/reports/`), timed per step, and exits
  non-zero unless every present database is backed up and verified. A named
  database the server does not hold (`beads_global` today) is a warning.

The verb skips store initialisation like `brain upgrade` does (it talks to
the server directly), so it runs from any directory.

# Proof

Unit tests cover registry resolution (ok, elsewhere, unreadable, `--store`),
database planning (shared and global databases), state comparison (identical,
drifted, changed-during-backup, missing table) and every destination guard.
`TestFederationBackupEndToEnd` (Dolt-gated) runs the built binary against a
sql-server of its own with two stores sharing one database and a third naming
an absent one, checks the report, reruns, and checks the data-directory
refusal.

On 2026-11-16 the verb ran on this machine against a scratch server seeded
with copies of the real `runbooks`, `decisions` and `person` stores
(October-2026 backups restored under an isolated `DOLT_ROOT_PATH`): three
databases backed up and verified, `bd list` OK for every store, 2.2 s in the
verb; timings per database in `docs/brain/FEDERATION_BACKUP.md`, transcripts
under `~/fm_home/macbook-ops/data/task-fable-3brn-20261116/proof/`. The live
:3307 server was read once with `SHOW DATABASES` and never written. Nothing
was scheduled on mini0; that is a mini0-ops follow-up named in the doc.
