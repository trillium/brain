# Cutover runbook: moving one store to the unified database

**Status:** written from the first end-to-end rehearsal on copies, not from the
design. Every numbered behaviour below was driven, and the commands and outputs
are preserved under
`/Users/mini0/fm_home/mini0-ops/data/brain-cutover-rehearsal/` (see §"Evidence").
**Direction:** [brain-single-database.md](brain-single-database.md) §"Cutover and
rollback", steps 3–6. The design doc says *what* the act is; this runbook says
*what it took*, in the order it was driven, with the gaps the rehearsal found
before they can be found in production.

## The act, in one paragraph

A store's command line (`stories`, `task`, …) is a shell wrapper that pins
`BEADS_DIR`, `BD_NAME`, and the server address, and the store's `.beads/metadata.json`
names which **Dolt database** that wrapper opens (`dolt_database`) on which
**port** (`dolt_server_port`). Moving the store to the unified database is
therefore an edit to two places — the store's `metadata.json` (point
`dolt_database` at `brain_unified`, and in the rehearsal the port at the
unified-tracking server) and the wrapper's port pin, which must stay in step —
and nothing else. Everything the store does afterwards works on one row set:
narrow reads, creation, markdown exfiltration, backup. Rolling back is the same
edit in reverse, plus a digest-and-counts proof against the store's own
database, which the cutover never touches.

## What must be true before starting

1. **A current merged artifact.** `bd brain unify build` + `verify` (`RESULT:
   PASS`) against the state production is actually in, and a re-run acceptance
   of any collisions that became content-disagreeing since. The rehearsal used
   the frozen verify-confirmed `brain_unified` (381 MB on disk, 43 tables) that
   the 518/518 PASS was recorded against; a real cutover rebuilds fresh.
2. **The new build deployed where the wrappers exec it.** The wrappers run a
   plain `bd` off `PATH` (`~/.pi/agent/bin/bd` → `~/.local/bin/bd` shape). The
   build is `go build -tags gms_pure_go` (plain `go build` fails on this host's
   ICU CGO path); the binary must be put where `exec bd "$@"` resolves it,
   before any wrapper is re-pointed.
3. **A fresh backup of the unified database and a restore that has been proven
   once.** `verify` is the content proof; the backup is the up-time proof.
4. The unified database **reachable on the serving machine's own Dolt server**
   (see §"What a real cutover additionally requires" — listed there because the
   rehearsal could not prove it here).
5. Per store being moved: its own `metadata.json`'s `project_id` still matches
   that store's `_project_id` row (checked below). The rehearsal confirmed this
   is already true for every store the build imported — no per-store prep is
   needed, but check it rather than assume it.

## The exact sequence for moving one store

Commands below are the rehearsal's, run under a scratch HOME; on the serving
machine the wrapper paths are the real ones (`~/.local/bin/<store>`,
`<store-root>/.beads/`).

**Step 0 — before-state.** Serve the store's own database and the unified
database, then record, per store being moved:

```sh
# counts, through the store's own wrapper, before touching anything
<store> list | tail -2            # "Total: N issues (...)" and the status legend
<store> list --json > before-list.json

# whole-table digest, per table: row count + xor(crc32(concat_ws(col, char(31), ...)))
#   (the order-independent digest the unify verifier uses; recipe in the report)
dolt sql -q "select count(*) from <store>.issues"          # run on the own server

# the wrapper's config and metadata, bytewise
cp <store-root>/.beads/metadata.json  metadata.json.before-cutover
cat <store-root>/.beads/config.yaml
```

Keep the before-digest CSV; the rollback proof compares against it.

**Step 1 — the cutover edit.** Two fields change in one file, one pin in the
wrapper:

```json
{ "dolt_server_port": <unified-port>, "dolt_database": "brain_unified",
  "project_id": "<the store's OWN project id — unchanged>" }
```

and in the wrapper script `BEADS_DOLT_SERVER_PORT=<unified-port>` (the same
value, or the wrapper and metadata disagree and every command fails, see gap 2).
**Do not change `project_id`.** The identity guard on the unified database
resolves to the wrapper's own namespace row in `brain_unified_metadata`
(`task → c2e4f6a8-…`, whose value equals the store's `_project_id`), so the
wrapper keeps its own identity and the guard stays meaningful; a wrapper whose
`project_id` names a *different* store is refused exactly as designed and as
observed (stories wrapper carrying task's id: refused, `PROJECT IDENTITY
MISMATCH`, with the database-side value coming from the namespace row, not the
seeded plain table).

```sh
cp metadata.json metadata.json.cut-over      # keep both editable states
"$EDITOR" metadata.json                      # edit dolt_database (+ port, if the rehearsal shape)
"$EDITOR" ~/.local/bin/<store>               # same port pin as metadata
```

The edits are per store and independent: each wrapper is its own rollback unit,
which is what makes step 5 of the design's ladder ("one at a time") real.

**Step 2 — serve-side prerequisite** (gap 1): the serving Dolt server must be
(re)started *after* the unified database's directory is placed in its
data-dir — **a running server did not discover the new database from a
directory that appeared after server start; `show databases` only listed it
after a restart.** Plan the restart before the first wrapper is re-pointed, and
confirm `show databases` includes `brain_unified`.

**Step 3 — drive it as a working store.** The rehearsal's command run for
`stories` (7 beads, template store) and `task` (1,386 visible / 5,706 issues in
the database, the captain's most-used command, and the non-template path that
was broken until this week):

| Command | Result |
|---|---|
| `<store> list` | identical id set to the own-database view (`list --json` diff: empty, both stores) |
| `<store> ready` | identical, except the store's own new bead where one was created |
| `<store> show <id>` | renders, including the `metadata.brain_slug` the build preserved |
| `<store> search "<own-phrase>"` | identical result set, ever-result an own-prefix id |
| `<store> search "<phrase>" --wide` | the every-stores view; `task` wrapper saw 15,900 issues total |
| `<store> q "<title>"` / `create` | mints under the store's own prefix: `stories-tia`, `task-yaakg`; never anyone else's |
| wrong-prefix create (`--id brain-xxxx`) | refused: `prefix mismatch … (use --force to override)` |
| same, plus `--force` | **still refused**: `namespace ownership failed … prefix "brain", which namespace "stories" does not own … first claim it with 'bd store-prefix add brain'` — the fix that makes `--force` unable to escape the namespace on the unified database |
| no-`BD_NAME` wrapper `create` | refused: `refusing to create: unified database with no store namespace pinned … Run this command under a store wrapper that exports BD_NAME` |
| `<store> render-all` | exfiltrates under the namespace's own root; failures only pre-existing slug-collision refusals (identical **3,121** refused on both sides for `task`; the merged view rendered and wrote **22 more** markdown files than the own-db run, for the 21 `db:task`-namespace beads plus the rehearsal bead — all winners, none of the failures new) |
| `<store> backup init <dest>` + `<store> backup sync` | succeeded, 5.6 s / 270 MB — see finding 5 before relying on per-store backups |

**Step 4 — sanity checks, as the operator will run them.** A read of the store
plus `bd doctor` work per store after cutover's step-4 shape; note `brain
stores doctor` walks the full stores.yaml registry, so it is only a *complete*
verification when every registry entry resolves — in the rehearsal shape only
(as expected) the one moved store had a reachable `.beads`.

**Step 5 — rollback, one store at a time, and prove it.** Restore the two
edited files, re-point the wrapper port back, and *re-derive* the digest:

```sh
cp metadata.json.before-cutover <store-root>/.beads/metadata.json
"$EDITOR" ~/.local/bin/<store>                       # port pin back to the own server
<store> list | tail -2                               # the store answers, own data
# re-run the per-table digest exactly as in step 0 and diff
diff before-digest.csv after-rollback-digest.csv
```

Rehearsal result (details in the report): **30 of 31 tables byte-identical
(counts and digests) for both stores**; the only differing row is
`local_metadata.tip_claude_setup_last_shown`, bd's per-client display stamp —
finding 4 explains why it is in the digest at all and why it is not cutover
evidence. The store's `dolt_log` gained no commits from the whole rehearsal
cycle: every write the cutover phase made went to the unified database.

## The operator-visible changes to expect

* `list` gets **narrow** by design: it shows the store's own namespace only.
  For `brain` this is a visible shrink (115 foreign-prefix rows move to their
  owning stores' views, previously proven and now live). For `task` the
  rehearsal's before/after id sets were identical; the merged view adds the
  21 `db:task` closed rows the build carried, which historically lived beside
  the store under other databases.
* `--wide` is now the every-stores view — 15,900 issues for the task wrapper in
  the rehearsal.
* Refusals appear where today there is silence: wrong-prefix create, `--force`
  escaping, storeless minting. The texts name the way out (own the prefix).
* Markdown exfiltration renders under the namespace-nested root and *refuses*
  a render that would overwrite a file belonging to a different bead — the 3121
  refusals the task store already has on its own database are the same class.
* Backup cost stops being per store: syncing through any wrapper backs up the
  whole merged database (270 MB in rehearsal, five seconds on loopback).

## Gaps and surprises the rehearsal found

1. **The serving server does not discover a database placed in its data-dir
   while running.** A copy of `brain_unified` dropped next to the served stores
   was invisible to the running server's `show databases` until that server was
   restarted (reproduced twice: the `tasks` database copied in mid-rehearsal
   only appeared after a server restart). A real cutover needs the placement,
   the restart, and the `show databases` confirmity check *before* any wrapper
   is re-pointed; a restart also drops live connections, so do it while the
   fleet is otherwise idle.
2. **The port pin is load-bearing in two places.** `metadata.json`'s
   `dolt_server_port` and the wrapper's `BEADS_DOLT_SERVER_PORT` must agree;
   when they did not, the failure was `database "tasks" not found on Dolt
   server at 127.0.0.1:<port>` — a wrong-answer error that points at the
   database rather than the mismatch. On the serving machine the port should
   never change (one shared server), which is exactly why the two pins should
   be edited in the same commit, or a sed slipped.
3. **The identity guard's unified-side source is per-namespace, not the plain
   table.** The seeded plain `metadata` table in the unified database carries no
   `_project_id` at all (the template seed left `{clone_id, repo_id}`); a
   wrapper reading *that* table would skip the guard. The guard reads the
   wrapper's *own namespace* row of `brain_unified_metadata`, which is present
   for every participating store with its `_project_id`. The rehearsal proved
   both halves: a correct-namespace wrapper (with the same values as on its own
   database) opens; a wrapper carrying a different store's id is refused with
   that namespace's value. Consequence worth pinning in policy: nothing should
   ever seed `_project_id` into the plain `metadata` table after cutover, or
   non-template wrappers would all face a mismatch against a single store's
   identity.
4. **`local_metadata.tip_claude_setup_last_shown` is a per-open client stamp**
   bd writes into the store's own database on every display-bearing open, and
   the first digest of a fresh store copy will record it mid-flight. The
   rehearsal's before/after digest therefore differs in that one row after the
   rollback drive re-opened both stores. It is not drift and not cutover-caused:
   run a digest proof excluding `local_metadata` (or stamp once before the
   baseline), and say so in the proof, which is what this runbook's rollback
   step does.
5. **Backup cadence becomes whole-database.** `bd backup init`/`sync` through
   any wrapper backs up the entire unified database (270 MB), so a store's
   "own" backup after cutover costs the federation, not the store. The design
   doc's per-store `.beads/backup` cadence is superseded per store once the
   store is cut over. Recommendation: keep **one** backup target for the unified
   database, drop per-store backup targets (or keep them for the not-yet-cut
   stores only), and set one whole-database cadence. The rehearsal ran the sync
   path twice with no failure and 43 tables verified structurally via the earlier
   restore-once proof recorded in the verify stage; a fresh-cutover backup sync
   should be followed by one restore-and-verify on the serving machine.
6. **`bd backup` bare does nothing but print help** — the operator-facing
   sequence is `backup init <dest>` then `backup sync` (or the auto-backup
   wrapper behaviour, which the rehearsal saw attempt and fail only while its
   own server was being bounced). It belongs in the operator docs, not in code.
7. **The rehearsal env-only, no code changes.** Everything above was observed
   through wrappers and SQL on copies; no behaviour differed from the design's
   promises — rather, the design's promised differences (narrow reads, refusals,
   wide view) all behaved as written, for the first time against `stories` and
   `task` together and with rollback to prove the act is reversible.

## Where the rehearsal had a choice, and the recommendation

**Rehearsal shape: two servers, one per role, or one server with all databases?**

The rehearsal served the stores' own databases on one loopback server (port
24500) and the unified database on another (24501). That is not how the serving
machine will look — there, one server already hosts every store's database, so
the merged database joins *that* server, and the cutover edit becomes one field
(`dolt_database`) instead of two files' worth of pins.

*What each shape gets you:* two servers isolate failure — a bad merged database
cannot block the not-yet-cut stores, and rollback is literally "stop listening
on that port". *What it costs:* it forces the port-pin edit (both files) and, in
the rehearsal, produced the misleading "database not found" failure (gap 2) —
and it can never be the final state, so the rehearsal-only edits are ones a real
cutover will not repeat.

**Recommendation (follows a standing captain preference to state alternatives
with their trade-offs — `captain.md`, 2026-10-06):** rehearse the two-server
shape only when the merged database's placement must be proven in isolation;
cut over **on the serving machine against the shared server**, so the cutover
edit is exactly `dolt_database` in one file and the port pins are never
touched. That also matches the design's ladder, where each step but the last is
reversible — and reversibility is the point of the ladder.

## What a real cutover on the serving machine requires that this rehearsal could not prove

Named honestly, including the pieces already known to be missing:

1. **The merged database has to live where the stores are served from.** The
   rehearsal's `brain_unified` copy sat in this task's scratch; production's
   server (launchd `127.0.0.1:3307` on the MacBook) must be given the database
   *and restarted* (gap 1), and the restart window has to be planned around
   fleet activity. The 381 MB placement and the catalog check are runbook steps
   0/2 above — but the serving machine's own server has never seen this
   database.
2. **The new build has to be deployed there.** The rehearsal exec'd a build of
   this branch (`01d523ac0`); production's wrappers still exec the deployed 1.2.2
   binary. Until the binary is swapped, an old binary re-pointed at the unified
   database mints under the template's `lifespan-` prefix with no refusal
   (proven on the same head as this doc's evidence). Deploy, then re-point.
3. **The server's network binding and authentication are unresolved.** The
   rehearsal ran on a loopback-only server with `root`/no-password as it is
   configured everywhere in this federation. The design doc names binding and
   auth as unresolved; this rehearsal does not change that, and a real cutover
   needs it decided before the database holds everything.
4. **Live-wide interference during the ladder.** The rehearsal ran both stores'
   wrappers serially, single operator, no concurrent writes to the same database
   from the not-yet-cut stores. Production will have concurrent writes until the
   last store is cut; the mixed phase's actual failure modes (ordering,
   duplicate ids across the ladder window) remain unproved here.
5. **The full registry surface.** `brain stores doctor` against the *real*
   stores.yaml on the serving machine, post-cutover, is the verification of
   record; the rehearsal's registry could not resolve paths outside its scratch
   shape, so the rehearsal substitution was direct command-driving of the moved
   store (every command above) — the same evidence, one store at a time.

## Evidence

Everything cited above lives under
`/Users/mini0/fm_home/mini0-ops/data/brain-cutover-rehearsal/`:

| Path | What it holds |
|---|---|
| `scratch/evidence/before-{stories,tasks}-digest.csv` | the before digests (31 tables each) |
| `scratch/evidence/after-rollback-{stories,tasks}-digest.csv` | the rollback proofs |
| `scratch/evidence/C-stories-drive/`, `scratch/evidence/D-task/` | per-command outputs for both stores, before and after |
| `scratch/wrap/*/` | the exact wrappers before/after (`.beads/metadata.json[.before-cutover]`) |
| `report.md` | the rehearsal's own narrative report |
| `scratch/logs/` | server stdout; `scratch/logs/pids` server pids |

Command surface clocks: unfied-server ports 24500 (own) / 24501 (unified) /
24502 (fresh-copy control rehearsal); the two rehearsal stores `stories` and
`task` (`tasks` database); rehearsal beads `stories-tia`, `task-yaakg` minted in
scratch copies only.
