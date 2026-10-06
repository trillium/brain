# Brain unify: the mini0 run

This records a full `bd brain unify build` + `verify` executed entirely on
**mini0** against a frozen copy of production, and the result copied back to the
macbook. It is the follow-up to
[`brain-single-database.md`](brain-single-database.md), which recorded that no
verification had ever completed.

**Result: verification completed for the first time (57 s, 279 checks), and it
did not pass.** See [Outcome](#outcome). The migration is *not* sound as it
stands, and no cutover should be attempted.

---

## 1. What the build actually reads

Established by reading `internal/brainunify` rather than guessing, because the
brief's open question was whether a 19 GB transfer was needed.

`bd brain unify build` opens a **Dolt SQL connection** (`--host`/`--port`,
default `127.0.0.1:3307`) and reads rows. It never enumerates store
directories. The filesystem is touched only for the store registry and each
store's `metadata.json` / `config.yaml`.

| path on the macbook | size | read by the build? |
| --- | --- | --- |
| `~/data/<store>/.beads/backup/**` | **8.2 GB** | **no** |
| `~/data/` as a whole | 19 GB | no |
| `~/data/.beads/` (the Dolt server's data dir) | **5.8 GB** | **yes — the whole input** |

So the input is **5.8 GB, not 19 GB**, and the backup trees are irrelevant.

Also established, and load-bearing for reproducing this anywhere: `Store.DSN()`
— the per-store `dolt_server_host` / `dolt_server_port` from each
`metadata.json` — is called **zero times** in `build.go`, `verify.go`,
`plan.go`, `dbsource.go`, `server.go` and `registry.go`. Discovery uses the
single `--host`/`--port` connection. Of `metadata.json`, only `dolt_database`
(store → database) is load-bearing, plus `issue-prefix` / `BD_NAME` from
`config.yaml` (declared prefixes).

## 2. What mini0 already had

mini0 already carried the whole federation as a `brain-sync` mesh replica, and
**all 57 `main` branch hashes were identical to production** — so every
*tracked* table was already byte-identical there.

It was nevertheless **not** a complete source: it was missing **504 tables**
(9 per database) that Dolt is configured to exclude from replication via
`dolt_ignore` — `local_metadata`, `repo_mtimes`, `ignored_schema_migrations`,
`wisps`, `wisp_%`. Those hold **75 wisp rows** plus per-machine bookkeeping. A
build from the mesh replica would have been quietly short by that much, so the
data directory was copied instead.

## 3. The frozen source, and how it was proved faithful

`~/data/.beads` (851 files) was rsync'd to mini0 as
`~/brain-unify/prod-src/.beads` and served by a dedicated `dolt sql-server` on
`127.0.0.1:3391`, so that build and verify read the *same* bytes. On the
macbook, production is written continuously — the lifespan ledger commits
several beads a minute — which would otherwise make any verification failure
ambiguous.

Verification of the source copy, before spending an hour on a build:

- 851 files on both sides; rsync passes 2 and 3 copied **nothing** (stable snapshot)
- 57 databases, identical names; **1530 tables**, i.e. including the 504 the mesh replica lacks
- **all 57 copied commit hashes are real commits in production's history** (`dolt_log`), so the copy is a genuine production state and not a torn one
- **1529 of 1530 tables count-identical** to production; the single difference was `lifespan.events`, 126 898 in the copy vs 126 904 live — 6 rows the ledger wrote after the snapshot

A registry mirror was built on mini0 at `~/.config/brain/stores.yaml` with the
same 46 stores, paths rewritten under `~/brain-unify/src/<store>/.beads`, and
each store's `metadata.json` / `config.yaml` copied across with only the server
port rewritten.

## 4. The build

```
bd brain unify build --host 127.0.0.1 --port 3391 \
    --data-dir ~/brain-unify/scratch/unified --allow-collisions --timeout 4h
```

**exit 0**, **990 s (16.5 min)**, all **55/55** sources, **685** collision-loser
rows skipped (all recorded in `brain_unify_collisions`), unified database
**359 MB**, 43 tables.

Host behaviour, which was the point of moving: mini0 load 2.3–5.0 on 10 cores;
dolt resident memory rose 4.0 GB → 7.5 GB → 16.8 GB; **swap stayed at
98.81 MB, unchanged for the entire run**. The previous attempt on the macbook
was *killed by the environment two minutes in with 5.5 GB of swap in use* — a
host-resource death, and it did not reproduce here.

Per-source cost is **constant (~15.5 s) regardless of bead count** — 0 beads
(`chores`) and 4 303 beads (`db:guard`) both cost 15.5 s. The 22.6 k rows are
nearly irrelevant to wall time; the work is 55 independent fixed-cost units, so
the scaling axis is *number of stores*, not amount of data. (For the record,
moving hosts bought ~6x with no code change: ~16 s/source on mini0 vs ~96 s/source
on the loaded macbook.)

## 5. The verification

```
bd brain unify verify --host 127.0.0.1 --port 3391 \
    --data-dir ~/brain-unify/scratch/unified --timeout 3h
```

**It completed — in 57 s** — and returned `RESULT: FAIL` with **279 failing
checks**. This is the first completed verification in this work.

Two findings, and they are of completely different kinds.

### Finding A — the re-keyed tables can never pass (277 of 279 checks)

Every failing check on a `brain_unified_*` table compares:

- the **expected** side: a fingerprint of the *source* table, which has no `store` column; against
- the **actual** side: the *unified re-keyed* table, which by construction has a leading `store` column added by `NamespacedDDL`.

Different column sets cannot produce the same digest, so **these checks cannot
pass for any store, ever.** They are not evidence of data loss. Proven by
recomputing both sides independently and matching the verifier's own numbers
exactly:

| | rows | bytes | hash |
| --- | --- | --- | --- |
| source `tasks.config`, whole table | 10 | **476** | **307648475** |
| unified `brain_unified_config` for store `task` | 10 | **576** | **1552887092** |
| `verify.log` as reported | 10 vs 10 | **476 vs 576** | **307648475 vs 1552887092** |

The 100-byte difference is exactly the added `store` value rendered into each of
the 10 rows.

Corroborating shape: on the expected side only **3 distinct byte values** appear
across 55 stores; on the actual side, 19.

### Finding B — two beads silently lose their metadata (2 of 279 checks)

In namespace `task`, two beads have a populated `metadata` JSON object on the
source and **`{}`** in the unified database:

| id | source `metadata` | unified |
| --- | --- | --- |
| `task-a44d4` | 1 116 bytes, `json_valid` = 1 | 2 bytes (`{}`) |
| `task-ybur` | 1 177 bytes, `json_valid` = 1 | 2 bytes (`{}`) |

That is **2 289 bytes of real content**, and it accounts for the entire
`issues` mismatch: `all groups` differs by 2289 bytes and namespace `task`
differs by 2289 bytes — the same number. Both sides agree on **row counts
everywhere**, including these namespaces.

This failure is **silent**: `brain_unify_import_log` reports
`source_rows=5706, imported_rows=5706, skipped_rows=0` for `task`/`issues`, and
no collision is recorded for either id.

Ruled out by measurement, not assumption:

- **declared column type skew** — no: `issues` column types are identical across the template (`lifespan`), `tasks` and `task`
- **the write path** — no: inserting the real value into a json column using `quoteLiteral`'s exact escaping stores all 1 116 bytes correctly
- **a duplicate id from another store overwriting it** — no: both ids exist in exactly one database (`tasks`) on all 57
- **apostrophes** — no: 41 rows contain one, only 2 fail
- **size** — no: the second-longest `metadata` value (1 174 bytes) is unaffected
- **any distinguishing character or bigram** — no: none is unique to the two failing values

So the mechanism is **not yet identified**. What is established is that the
build reads the correct value at digest time (its own recorded source
fingerprint is the full one) and that the value does not survive into the
unified table. A determinism check was run — see
[What was not established](#7-what-was-not-established).

## 6. The copy back

`~/brain-unify/scratch/unified/` was rsync'd to
`~/fm_home/firstmate/data/task-4kuun/unified/`, and the copy itself verified
rather than assumed:

- **366 MB**, 26 files
- **per-file SHA-256 manifest identical** to the mini0 original at copy time — byte-for-byte (25 files hashed; lock and log files excluded)
- a server opened over the copy on the macbook, and **all 43 tables' row counts are identical** to the original
- 262 393 total rows; accounting read back from the copy: `brain_unify_import_log` 493, `brain_unify_source_fingerprints` 491, `brain_unify_collisions` 123, `brain_stores` 55, `brain_store_prefixes` 56

Two notes, because verification is what turned them up rather than assumption:

1. **A stray `scratch_test` database.** The diagnostic queries used to prove
   Findings A and B were issued against the unified database's own server, which
   put a `scratch_test` database (with test tables) *inside* the delivered
   directory. It was removed from both sides before delivery, and the copy was
   re-verified afterwards. Delivered contents are now exactly `brain_unified/`,
   `config.yaml`, `.dolt/`, `.doltcfg/`, `unified-server.log`.
2. **Scaffolding is not payload.** Opening a copy with a Dolt server rewrites
   that server's own files. After the verification server ran, three files
   differ from the original — the query-stats store (`.dolt/stats/…`),
   `.doltcfg/privileges.db` and `config.yaml` — while **every one of the 11
   data-bearing chunk files in `brain_unified/.dolt/noms/` is still
   byte-identical**. The copy re-opens and serves correctly: one database, 43
   tables, 22 523 issues rows, 123 recorded collisions.

## 7. Outcome

**One of the three honest outcomes: a named list of genuine defects with failing
checks.** Not "zero failures", and not "verification could not be made
reliable" — verification *was* reliable here, and it caught exactly one real
problem.

- **Defect B is a genuine migration defect**: two beads lose their `metadata`
  content, silently. This alone blocks cutover.
- **Defect A is a genuine verifier defect**: 277 of the 279 failures are an
  alarm that cannot clear, because the expected side and the actual side digest
  different column sets. It overstates the problem by two orders of magnitude
  and would mask a real regression.

The migration cannot be called sound. The verifier cannot yet be called sound
either, but it did the job it exists for.

## 8. What was not established

- **The mechanism of Defect B.** The observable facts are pinned down and the
  obvious explanations are excluded; the cause is not. A second build from the
  same frozen source into a separate data directory was run to test whether the
  same two beads are affected (deterministic) or different ones
  (nondeterministic). Its result is not recorded here.
- **Whether Defect A is only a verifier bug.** That the 277 checks can never
  pass is proven. Whether a correct comparison would also find *data* problems
  in the re-keyed tables is not — proving that needs a fixed comparison.
- **Anything about cutover.** Production was never written to: the build's only
  writable connection is the isolated server it starts under `--data-dir`.

## Reproducing

Scripts from this run are in [`scripts/brain-unify-mini0/`](../../scripts/brain-unify-mini0):

- `run-unify-mini0.sh` — starts the source server over the frozen copy, then runs build and verify with timestamped per-source/per-table progress and a memory trace
- `launch-mini0.sh` — launches the above detached, so a dropped ssh cannot kill it, and stays attached only as a watcher
- `copy-back-and-verify.sh` — copies the unified database back and verifies the copy by manifest, row counts and accounting tables

To reproduce Finding B directly, with the source on `3391` and the unified
database served on `3393`:

```sql
select id, length(metadata)
from tasks.issues
where id in ('task-a44d4','task-ybur');          -- 1116 / 1177

select id, length(metadata)
from brain_unified.issues
where id in ('task-a44d4','task-ybur');          -- 2 / 2
```

To reproduce Finding A, recompute the source table's whole-table fingerprint
with the same expression `fingerprintFields` builds
(`concat_ws(char(31), ifnull(concat(char(2), lower(hex(col))), char(1)), …)`,
`cast(col as char)` for json columns) and compare it with the `store`-prefixed
form used on the unified side — the numbers above reproduce exactly.
