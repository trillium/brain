# brain-unify-mini0

Operational scripts for running `bd brain unify build` + `verify` on **mini0**
against a frozen copy of the production federation, then copying the result
back to the macbook. They are the scripts that produced the run recorded in
[`docs/design/brain-unify-mini0-run.md`](../../docs/design/brain-unify-mini0-run.md).

They are deliberately not general-purpose: paths are the absolute paths of the
machines the run happened on. They are kept because this work has repeatedly
been blocked on the *environment* rather than the migration, and a recorded
procedure is worth more than a description of one.

## Layout on mini0

```
~/brain-unify/
  bin/bd                     the freshly built bd (the five-fixes branch) — NOT the installed bd
  prod-src/.beads/           frozen copy of the macbook's Dolt data dir, served on 127.0.0.1:3391
  src/<store>/.beads/        registry mirror: metadata.json + config.yaml per store
  scratch/unified/           the build output (the unified database)
  logs/                      run.log, build.log, verify.log, mem.csv
```

`~/.config/brain/stores.yaml` must also exist on mini0 with the same 46 stores
as production, paths rewritten to `~/brain-unify/src/...`, and each store's
`metadata.json`/`config.yaml` mirrored alongside with only the server port
changed.

## Order

1. Freeze the source. rsync the macbook's `~/data/.beads/` (the Dolt server's
   data directory — 5.8 GB, 851 files) to `~/brain-unify/prod-src/.beads`,
   excluding `dolt-server.lock|port|log`, `*.lock` and `backup/`. Run it three
   times: passes 2 and 3 copying nothing is the evidence that the snapshot is
   stable. Then prove it: the database list must match, every copied commit hash
   must be present in production's `dolt_log`, and per-table row counts must
   match (the only expected difference is the lifespan ledger, which is written
   continuously).

   Note the input is the Dolt **server's** data dir, not `~/data` (19 GB, of
   which 8.2 GB is `.beads/backup` trees the build never reads).

2. `launch.sh` — starts `run-unify.sh` detached (a dropped ssh cannot kill it)
   and stays attached only as a watcher, so one completion notice covers the
   whole build.

3. `run-unify.sh` — starts the source server over the frozen copy, then runs
   build and verify with timestamped per-source/per-table progress and a
   swap/RSS trace in `logs/mem.csv`. Build and verify read the *same* frozen
   bytes: on the macbook production is written continuously, which would
   otherwise make a verification failure ambiguous.

4. `copy-back-and-verify.sh` — copies the output back and verifies the copy by
   per-file SHA-256 manifest, per-table row counts and the build's own
   accounting tables. Run it from the macbook.

5. `repro-build.sh` — rebuilds from the same frozen source into a separate data
   directory, to test whether a content difference is deterministic. This exists
   because the run above found two beads whose `metadata` JSON does not survive
   the build; see Finding B in the report.

## Two traps worth knowing

- **Do not issue test queries against the unified database's own server.**
  Anything you `create database` there lands *inside* the delivered directory.
  This happened once, and only the copy verification caught it.
- **Opening a copy rewrites server scaffolding.** `.dolt/stats/…`,
  `.doltcfg/privileges.db` and `config.yaml` change as soon as a Dolt server
  runs over the directory, so a byte-comparison must be scoped to the
  data-bearing `brain_unified/.dolt/noms/` files to mean anything.
