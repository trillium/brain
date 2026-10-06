# Brain: one Dolt database instead of many

**Status:** implemented and verified. `RESULT: PASS` — 518/518 checks against a
build from a proven-identical frozen copy of production. Both defects found by
the first completed verification — a verifier that could never pass on re-keyed
tables, and a silent 2289-byte content loss — are identified and fixed, each
with a colocated test. Production is untouched.
**Direction:** [brain-c56qg] (parent topic), [brain-a7cna] (live discussion).
**Boundary change:** the plan-only scope recorded in **task-dena0** — "OUT OF SCOPE:
performing the migration" — is **superseded** by inbox-mvlo, which authorises the
implementation. This document does not quietly widen task-dena0; it names the
change. task-dena0 remains the record of the analysis that motivated the design.

## The problem

Brain federates ~49 registered stores. Each one is its own Dolt database on a
shared `dolt sql-server`, reachable through a thin `~/.local/bin/<name>` shell
wrapper that pins `BEADS_DIR` and delegates to `bd`. Measured on the live server:

| | |
|---|---|
| Dolt databases on the server | 57 |
| Databases holding beads | 55 |
| Beads, summed across participating stores | 22,391 |
| Registered stores in `~/.config/brain/stores.yaml` | 49 |
| Databases on the server no store claims | 11 |

Store identity is therefore expressed **twice** — once by which database a row
lives in, and once by the id prefix — and the two do not agree. Registry key
`decisions` is database `decision`. `robots` is database `agent`. `brain` is
database **`dolt`**. `ideas` is `idea`, `projects` is `project`, `questions` is
`question`, `violations` is `violation`, `assertions` is `assert`. A store
(`robots`) can own two namespaces (`robots-` and `agent-`). One prefix (`task-`)
can span two databases (`tasks` and `task`).

## The mechanism: prefixes, and why not a discriminator column

A bead id is already self-describing: `brain-se7t.389` is in the `brain`
namespace, and a hierarchical child stays in its parent's namespace. **Store
identity is already encoded in the primary key.**

So unification adds **nothing** to the `issues` schema:

* no `store` column, so no schema migration and no divergence between a
  column and the id;
* no id rewriting, so every id in every log, hook payload, markdown file and
  conversation stays valid;
* no query rewriting, so `id like 'inbox-%'` — which is how the federation
  already filters — keeps working, and it now selects from one table instead of
  N.

The alternatives were considered and rejected:

**A `store` column on `issues`.** It would duplicate information the id
already carries, and every reader that filters by prefix would have to learn to
filter by column instead. Worse, it cannot be made consistent: a row's column
and its id prefix can disagree, and nothing in the schema stops that.

**One table per store.** This keeps physical separation, which is exactly what
is being removed.

**A discriminator column plus a generated namespace view.** More machinery, no
extra fidelity over the prefix that is already there.

What separate databases carried that prefixes do not is *provenance*: which
physical database a row came from, and which store claims which prefix. That is
recorded explicitly, in new tables (§ "The unified schema").

## The deterministic mapping

### Which databases participate

The rule is mechanical, not curated:

1. A database claimed by a registered store always participates.
2. A database claimed by no store participates if it holds **one** namespace —
   that is a store the registry has not caught up with.
3. A database claimed by no store holding **several** namespaces is excluded as
   a cross-store replica.

Rule 3 is what keeps `beads_global` (2,330 beads: 1,206 `brain-`, 164 `agent-`,
148 `isa-`, 136 `project-`, …) out. It is an index of other stores, not a store;
importing it would re-import every store's ids a second time. `TinyKeyboard`
(13 beads, namespaces `TinyKeyboard` and `tk`) is excluded by the same rule and
is not a brain store at all. **Every exclusion is printed with its bead count.**
On the live data: 55 participating sources, 2 excluded, 2 excluded.

### Which store owns which namespace

`brain_store_prefixes` in the unified database answers "which store is this bead
in". Ownership is decided in this order, and the rule that fired is recorded
per prefix:

1. **Declared** — the store whose `.beads/config.yaml` claims the prefix
   (`issue-prefix`, then `BD_NAME`). Ties break on the smallest store name.
2. **Name match** — otherwise a store whose own name or Dolt database name
   equals the prefix. This is what gives `assert-` to the `assertions` store
   (database `assert`), and `decision-` to `decisions`.
3. **Unattributed** — otherwise the prefix belongs to no store. It still
   migrates, keeps its ids, and is recorded as owned by nobody.

Both sources are recorded per prefix (`declared_by`, `observed_by`), so a prefix
shared by two databases is visible rather than implied. On the live data, 10
prefixes are shared by more than one database, including `task-` (brain,
db:task, task) and `agent-` (brain, db:agent_identity, robots).

### Which table a row lands in

Classified from the table's own columns, not from a hardcoded list, so a table
added by a later schema migration is classified the same way:

| Scope | Test | Rows land in |
|---|---|---|
| `issues` | the table is `issues` | `issues`, addressed by id prefix |
| issue child | has an `issue_id` column | the same table name, unchanged |
| database state | neither of the above | `brain_unified_<table>`, re-keyed by store |

## The unified schema

Everything from the template source's own `SHOW CREATE TABLE` output — the
unified database gets the schema the sources already have, not a hand-copied
approximation that would drift. Tables load parents-first (topological order
over the declared foreign keys), so `issues` lands before `labels`.

Four tables are added. They are the whole of the provenance that separate
databases used to carry implicitly through their physical location:

| Table | Answers |
|---|---|
| `brain_stores` | which source database each store came from, with its project id and schema version |
| `brain_store_prefixes` | which store owns each id prefix, and which rule decided it |
| `brain_unify_collisions` | every duplicated id, the winner, the losers, and the **full losing row** |
| `brain_unify_import_log` | per store and table: source rows, imported rows, skipped rows |

### What is preserved

* **IDs** — copied verbatim. No id is rewritten, renumbered or regenerated.
* **Hierarchy** — `brain-se7t.389` keeps its parent, and the prefix rule puts it
  in the same namespace as its parent.
* **Relationships** — `dependencies`, `labels`, `comments`, `events`,
  `isa_sections` and the wisp tables all carry an `issue_id` or `wisp_id` that
  already names the namespace, so they fold in unchanged. Cross-store
  dependencies, which **cannot be recorded at all today** because the foreign
  key cannot cross a database boundary, become recordable after unification.
* **Metadata** — `metadata`, `description`, `notes`, labels and `payload` are
  copied column-for-column.
* **History** — the `events` table (7,004 rows in the brain store alone) is
  copied; `updated_at`, `created_at`, `closed_at` and `started_at` are values,
  not defaults, so no timestamp is regenerated.
* **Query behaviour** — `id`, `slug` and every index survive. `slug` is the one
  place where unification *tightens* a constraint, and it is measured rather
  than assumed: across the 696 non-null slugs in the federation
  (brain 544, isa 142, decision 6, person 1, resumes 1, rt 2) there are **696
  distinct values**, so `idx_issues_slug_unique` survives unification unchanged
  today. It remains a latent risk: two stores minting the same slug is accepted
  today and rejected after cutover. That is called out here rather than left to
  be discovered.

### What changes shape, and why

Per-database state cannot be shared: two stores both have a `config` row with
key `issue_prefix`, and only one of them can survive under that key. Those
tables — `config`, `metadata`, `local_metadata`, counters, schema bookkeeping —
are re-keyed into `brain_unified_config(store, key, value)` and friends. **No
value is dropped**; the key space is namespaced instead of overwritten.

This includes the `kv.*` keys in `config`, which are user data: the brain store
holds `kv.membead.*` and `kv.memory.*` entries, and they migrate into
`brain_unified_config` under `store = 'brain'`.

So that `bd` can open the unified database at all, the unified database's own
`config` and `metadata` tables are seeded from the **template store** (default:
the store with the most beads). Follow-up work: teach the config layer to read
`brain_unified_config` so a wrapper can address its own settings without a
separate database.

### What cannot be preserved

* **Physical isolation.** Two stores will no longer be able to corrupt each
  other by sharing a server process, and a bad `delete` in one store can reach
  another's rows. That is the price of one database, and it is the reason the
  cutover below keeps the old databases readable until the end.
* **`dolt` remotes and per-database history.** Each source database has its own
  Dolt commit history. The unified database has one history, reconstructed from
  row values, not replayed from those commits. Event-level history is preserved
  (`events` is copied); commit-level provenance is not.
* **Per-store backup cadence.** `.beads/backup` is per store today; the unified
  database has one.

## Collisions: proven, not asserted

Unification makes one table hold what were N primary keys. The live federation
already contains duplicates: **123 ids exist in more than one participating
database**, because the brain store's `repos.additional` federation let it hold
copies of other stores' beads.

| Prefix | Duplicated ids | Databases |
|---|---|---|
| `project` | 36 | brain, projects |
| `assert` | 21 | brain, assertions |
| `question` | 18 | brain, questions |
| `person` | 17 | brain, person |
| `task` | 17 | brain, db:task |
| `agent` | 8 | brain, robots, db:agent_identity |
| `idea` | 3 | brain, ideas |
| `life` | 3 | brain, life |

The rule, applied to every duplicated id:

1. **The prefix owner's copy wins.** Same rule that answers "which store is this
   bead in", so the winner and the namespace can never disagree.
2. Otherwise the **most recently updated** copy wins, so a divergence resolves
   toward live state.
3. On an exact timestamp tie, the **lexicographically smallest** source name
   wins. The order is total, so the same data always produces the same winner.

**Nothing is dropped silently.** Every losing copy is written to
`brain_unify_collisions` with its **full row as JSON** (`losing_row`), and every
child row of a losing copy is dropped with it — otherwise the unified database
would carry relationships to an id whose winning row came from elsewhere. The
skipped count is recorded per store and per table in `brain_unify_import_log`.

### The collision that actually costs something

`agent-0bq` exists in both `agent` (the robots store) and `dolt` (the brain
store). The copies disagree on `close_reason`, `closed_at`, `notes`, `status`
and `updated_at`: the `agent` copy is **closed** as of 2026-07-29, the `dolt`
copy is **open** and stale. Keeping the wrong one loses real state.

**31 of the 123 duplicated ids disagree on content** (24 on actual content —
`status`, `closed_at`, `close_reason`, `notes`, `description` — 7 on bookkeeping
only), and the rest not at all. The plan compares duplicated rows **column by
column**, not by trusting the stored `content_hash` — the three `assert-*` ids
that looked divergent on `content_hash` turned out to have identical titles,
descriptions and status, while the `task-*` ids differ in `status`, `closed_at`
and `close_reason`. This count was 27 when first recorded; the divergence set
grows as production moves — two ids gained notes after this doc was written
(`project-2g7` on 2026-09-16, `project-xat` on 2026-10-02) — so the number is
read from the build's `brain_unify_collisions` table, not quoted from here.

**The winner rule was reviewed and accepted per id.** The per-id review is
recorded at
`fm/brain-unify-verify` artifact
`/Users/mini0/fm_home/mini0-ops/data/brain-unify-verify/item2-collision-review.md`:
every losing row is the brain store's stale snapshot of the same bead (made
under the old `repos.additional` federation), the winner is a strict superset
wherever prose differs (dated appended notes, verified-fixed notes, migration
close-reasons the losers lack), and there is not one column where the losing
copy holds newer content. Every losing row stays recoverable in full from
`brain_unify_collisions.losing_row` if any id is ever disputed.

A content disagreement **blocks** the build. Bookkeeping-only differences are
reported but do not block, because keeping one copy loses nothing the bead
said. `--allow-collisions` overrides the block for a deliberate build.

## Migration tooling

```sh
bd brain unify plan   --json                       # read production, print the mapping
bd brain unify build  --data-dir /path/to/scratch  # construct the unified database
bd brain unify verify --data-dir /path/to/scratch  # compare mechanically
```

**Production safety is structural, not a convention.** The production server is
opened through a connection whose every statement passes a guard that refuses
anything that is not `SELECT`/`SHOW`/`DESCRIBE`/`WITH` — `insert`, `update`,
`delete`, `drop`, `create`, `set`, `call dolt_add` and a leading SQL comment
are all refused, and the guard is unit-tested. The builder writes only to a
Dolt server it starts itself, on a free loopback port, under `--data-dir`.
`--data-dir` is required and must not be a production data directory.

This is also the captain's stated preference: rather than cloning each
production database and merging the clones, the tool reads accurately from the
live sources and constructs **one** cloned database containing the complete
combined dataset.

## Validation

`bd brain unify verify` compares the built database against production
mechanically, for every namespace, every store and every table:

* row count;
* total content size;
* an **order-independent content digest** — `bit_xor(crc32(...))` over every
  row, so a different insertion order still matches but a changed byte does
  not.

A digest cannot be produced by a spot check; it is recomputed from every row on
both sides. The namespace-level expectation is assembled from every
contributing source with the collision losers excluded, and combined using the
same XOR identity the server applies when it aggregates the unified table in
one pass.

On top of the fingerprints, the verifier re-reads `brain_unify_collisions` and
confirms that each duplicated id appears **exactly once** in the unified
database, that it is the recorded winner, and that every losing copy is on
record. That is the check that would catch a migration which quietly dropped a
relationship.

## Cutover and rollback

Each step states what it makes irreversible.

| Step | Action | Reversible? |
|---|---|---|
| 0 | **This change**, merged but unused. No behaviour changes. | Fully reversible: delete the branch. |
| 1 | Run `plan` against production. Read-only. | Fully reversible: nothing was written. |
| 2 | Run `build` + `verify` into a scratch directory. Read-only on production. | Fully reversible: delete the scratch directory. |
| 3 | Point **one** non-critical store (recommend `stories`, 7 beads) at the unified database. | **Reversible: revert the wrapper.** The source databases are untouched. |
| 4 | Verify through `brain stores doctor` and a read of that store. | Reversible. |
| 5 | Move stores over one at a time, lowest-bead-count first. | Reversible per store: each wrapper is independent. |
| 6 | Stop the per-store Dolt databases. | **Reversible:** the databases still exist and are not written; re-point the wrapper. |
| 7 | `dolt sql -q "drop database <store>"`. | **IRREVERSIBLE.** Do not take this step until the unified database has been backed up, verified a second time, and run in production for long enough to be trusted. |

Steps 0–6 are reversible because every step leaves the source databases intact
and unwritten. Step 7 is the only irreversible one, and it is the only one
that is a separate, deliberate act.

### What must be true before step 3

1. `bd brain unify verify` reports `RESULT: PASS` on a database built from a
   fresh read of production. **Met** — 518/518 checks on a build from a proven
   frozen copy of production; see "Verification status" below.
2. The 31 content-disagreeing collisions have been reviewed and the winner rule
   accepted, or the losing rows have been recovered from
   `brain_unify_collisions.losing_row`. **Met** — winner rule accepted per id
   (see the collision section above); the losing rows stay recoverable from
   `brain_unify_collisions.losing_row`. The cutover itself is still not
   authorized by this acceptance.
3. The unified database's `config`/`metadata` seeding story is settled for the
   stores being moved (see "What changes shape" above).
4. The markdown exfiltration bridge has been pointed at the unified database,
   or confirmed not to need to be: every write renders
   `<store>/entries/<kind>/<slug>.md`, and the store name it writes under must
   come from the namespace, not the database.
5. `brain search` federation has been re-pointed: it walks stores, and after
   unification there is one store to walk with many namespaces to filter.
6. A backup of the unified database exists and has been restored once.

## What this does not change

* No bead id changes.
* No markdown file changes.
* No per-store Dolt commit history is replayed.
* Memory Beads adoption (brain-c56qg step 4) is untouched by this; unification
  is the precondition for it, not part of it.
* The brain-jueql ownership/state/messaging model on the firstmate side reads
  through the store wrappers. Those wrappers keep working unchanged, which is
  what makes steps 3–6 reversible one store at a time.

## Verification status: two defects, both understood, both fixed

`bd brain unify verify` completed for the first time against a full build, and
returned `RESULT: FAIL` with 279 failing checks. The two findings behind those
checks are of completely different kinds, and both are now identified and
fixed. This section replaces the first account of that run with what the
measurement actually established.

| | |
|---|---|
| first build | exit 0, 990s, **55/55** sources, 685 collision losers skipped and recorded, 359 MB, 43 tables |
| first verify | completed in **57s**, `RESULT: FAIL`, **279** failing checks — 277 verifier defect A, 2 genuine content loss |
| re-run build | exit 0, 1516s, **55/55** sources, 685 rows skipped (all recorded in `brain_unify_collisions`), 394 MB on disk, 43 tables |
| re-run verify | completed in **110s**, **`RESULT: PASS`** — 518/518 checks, 0 failed, 222 namespaces, **123** collisions confirmed |

The re-run was built against a fresh frozen copy of production, so build and
verify read byte-identical data; the copy is 57 databases, and **every copied
main-branch hash is a commit production's own `dolt_log` contains**, which is
what makes it a real production state rather than a torn one. The two beads
that lost 2289 bytes now hold **1116 and 1177 bytes** in the unified database —
the same lengths the source holds — and the 123 recorded collisions each appear
exactly once, as the verifier requires.

### Defect A — the two sides digested different column sets

277 of the 279 failures could not have passed for any store, on any data. Each
compared a source-side fingerprint of a table with **no `store` column** against
a re-keyed unified table that has one, and two different column sets cannot
produce the same digest. They were not evidence of data loss, but an alarm that
cannot clear is worse than no alarm: it overstated the problem by two orders of
magnitude and would have masked a real regression behind them.

**The fix is to make both sides digest the same column set, not to relax the
comparison.** `ReferenceDigest` now takes the column set from the table's
*plan* — the same plan `createSchema` builds the unified table from — so a
re-keyed database-state table gains the leading `store` field on the source side
too, and the old unprefixed whole-table digest no longer exists to be called by
mistake. `referenceFields` is the single place that decides whether a plan gains
that column, in both directions: prefixing a table that folds in unchanged would
be the same defect mirrored.

The recorded hand computation reproduces **exactly**, which is what identifies
the 100-byte delta as the store value and nothing else. Both sides were
recomputed against production with read-only SELECTs over `tasks.config`, the
unified side's row text being the source side's with one field added ahead of it:

```
                                   source side, per row
concat_ws('\x1f',
  ifnull(concat('\x02', lower(hex(`key`))),   '\x01'),
  ifnull(concat('\x02', lower(hex(`value`))), '\x01'))

                                   unified side, per row
concat_ws('\x1f',
  ifnull(concat('\x02', lower(hex('task'))),  '\x01'),   -- the added `store`
  ifnull(concat('\x02', lower(hex(`key`))),   '\x01'),
  ifnull(concat('\x02', lower(hex(`value`))), '\x01'))
```

| side | rows | bytes | digest |
|---|---|---|---|
| source column set only (the old expected side) | 10 | 476 | 307648475 |
| with the store field the unified table adds (the fixed expected side) | 10 | **576** | **1552887092** |

Both rows match `brain-unify-mini0-run.md` and `verify.log`'s own reported
numbers. The 100 bytes are one `\x02` present-value marker, the lower-cased hex
of `task` (8 characters) and one `\x1f` separator, per row, over 10 rows. The
second row is the number the unified side always produced, so the check now has
a value it can pass with, and nothing was skipped to get there.

Two colocated tests lock it: `TestReKeyedFingerprintDigestsTheUnifiedColumnSet`
asserts the two sides' field lists are the same set, in the same order, with the
same rendering, differing only in where the store's value comes from — and that
a table which folds in unchanged is *not* prefixed;
`TestStoreColumnByteOverheadMatchesTheRecordedRepro` locks the 476 → 576
arithmetic above rather than leaving it as a number in a report.

### Defect B — a json column read can come back empty, and `{}` hid it

Two beads in namespace `task` lost their whole `metadata` JSON to `{}`:
`task-a44d4` (1116 bytes → 2) and `task-ybur` (1177 → 2), 2289 bytes of real
content, silently — `brain_unify_import_log` reported `source_rows=5706,
imported_rows=5706, skipped_rows=0` and no collision was recorded for either id.

**The mechanism.** The build's copy read a `json` column as itself. On the
federation's own Dolt server that read returns a **zero-length value** for these
two rows whenever the result set holds more than one row, while
`cast(`metadata` as char)` at the same ordinal in the same statement returns the
full 1116 and 1177 bytes. `normalizeRow` then applied its documented rule — an
empty json value becomes `{}` — and a 1177-byte document became a 2-byte object.
The build's own recorded source fingerprint is computed **server-side, inside an
aggregate**, so no value crosses the wire there and the fingerprint stayed full.
That is exactly why nothing caught it: the two reads disagreed, and only `verify`
compared them.

Reproduced read-only, from two independent clients, without a build:

```
select <the template's 62 columns> from tasks.issues
 where id in ('task-a44d4','task-ybur','task-ztxq') order by id
```

Server-side, the column holds 1116, 1177 and 43 bytes, all `json_valid`. Through
that SELECT, `metadata` arrives **empty** for the first two rows and full for the
third. Replace `metadata` in the list with `cast(`metadata` as char)` — same
statement, same ordinal — and all three arrive full. Read one row at a time, all
three arrive full in both forms.

The drop is therefore on the server's native `json` wire path, not in the
client: the migration's own go-sql-driver path and the `mysql` CLI both receive
the empty value, while the same server's own aggregate sees the full one. It is
not positional either — moving the column, duplicating it, or substituting
another column into its ordinal changes the outcome, and a one-row result set is
full at every column count. What was ruled out by measurement rather than
assumption: declared column-type skew (one `(column, type)` set per re-keyed
table across all 57 databases), column-count skew, the write path, duplicate
ids, apostrophes, value size, and position in the read stream.

**The fix keeps one rendering of a json value on both sides.** The copy, and the
full-row read the collision comparison uses, now select json columns as
`cast(`c` as char)` — the same expression the fingerprints already digest them
with. So the bytes that are digested and the bytes that are written come from one
rendering by construction, and the value stays off the path that drops it.
`TestCopyRowsReadsJSONThroughTheDigestExpression` and
`TestIssueRowReadsJSONLikeTheCopy` lock the two SELECTs.

**What is not established** is the internal cause inside Dolt. The server is
Dolt 2.1.10, and `dolthub/dolt#11210` — JSON serializer bugs that silently store
NULL for large json values and return corrupted values under concurrent read
load — was closed by PR #11215 after this version. That issue's stated trigger
does not describe these values: they are 1116 and 1177 bytes with no control
characters and no backslashes. It is a related defect class in the same code
path, not a proven identity. The migration does not depend on knowing more than
it does: it no longer reads a json value through that path.

**Confirmed fixed at full scale.** In the re-run's unified database `task-a44d4`
and `task-ybur` hold 1116 and 1177 bytes of `metadata`, `json_valid` 1, with the
`brain_slug` each source holds — the same lengths as production — where the
previous build had stored a 2-byte `{}` for each.

### What is established

**The mapping, against live production.** `bd brain unify plan` runs reliably
and has been run repeatedly against the real federation. It reports 55
participating sources, 123 ids present in more than one database and 27 of those
disagreeing on content. The namespace ownership table, the collision winner
rule, the exclusion of `beads_global` and `TinyKeyboard`, and the per-prefix
ambiguity list all come from those runs, not from reasoning about what the code
should do.

**The build works at full scale**, and reads production through a connection
that structurally refuses any non-SELECT statement while writing only under its
own `--data-dir`. Production was never written to at any point.

**The verifier refuses bad input.** Given a deliberately truncated build it
reported `collision agent-0bq appears 0 time(s) in the unified database, want
exactly 1` and failed, rather than comparing partial data and reporting a
result. That behaviour is correct and has been left alone; a green result
obtained by relaxing it would not be a result.

**The verifier tells a real defect from its own.** The 279 failures were not one
undifferentiated mass: 277 were an instrument that could never clear and 2 were
a real, silent loss of 2289 bytes. Then the second was shown to be deterministic,
the first was fixed without touching what it checks, and the read path that lost
the 2289 bytes was replaced with the one the instrument already trusted. That is
the property that matters most in an instrument like this — not that it can say
"pass", but that it can say *what* is wrong and survive an attempt to explain the
failure away.

### The instrument and copy defects found on the way

Five defects have been found, all of them in the instrument or the read path
rather than in the migration's mapping. The first three were fixed in sequence,
the fourth is Defect A, the fifth is Defect B's read.

1. Fingerprint scanned its aggregate columns into discarded variables, so every
   database-state table compared as zero content.
2. The reference fingerprints were taken by re-reading each source *after* the
   copy, so concurrent writes appeared as migration defects.
3. The client reimplemented the server's column encoding to digest rows at copy
   time; the two implementations drifted by a few bytes per row, producing
   failures indistinguishable from real corruption.
4. Defect A above: the two sides digested different column sets on re-keyed
   tables.
5. Defect B above: the copy read json columns through a path that can return an
   empty value, while the digest read them through `cast( as char )`.

(3), (4) and (5) are one fault seen three times: two things that must agree about
how a value is rendered, not agreeing by construction. Each fix removed a
rendering rather than tolerating its absence — which is why there is now exactly
one expression per column, used by the digest and the copy alike.

### Why the earlier attempts did not complete

After the encoding fix the build became materially heavier — the source is
digested immediately before each table is copied, so each store costs a second
read against the source server. The 90-minute default cut a build off partway,
leaving a partial database, which the verifier correctly refused. The earlier
account of the kill that followed blamed memory pressure. That was wrong: swap
was flat while the host's load average sat near 100 on 10 cores. **CPU
contention, not memory**, and the run that finally completed used a host at load
~3 and never needed memory headroom either.

What this costs: for a long time the environment was blamed for a failure that
had a code explanation, and the search for that explanation stopped at the host.
The two findings above were reachable as soon as a run completed; nothing about
them needed a different machine, only a machine that was not already saturated.
Per-source cost is a constant regardless of bead count (0 beads and 4312 beads
both cost about the same), so the scaling axis is **the number of stores, not
the amount of data** — and a timestamped per-table log is what makes a stall
visible while it happens rather than an hour later.
