# Brain: one Dolt database instead of many

**Status:** implemented and validated against a clone of live data. Production is untouched.
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

**27 of the 123 duplicated ids disagree on content**; the rest differ only in
bookkeeping (`content_hash`, `updated_at`) or not at all. The plan compares
duplicated rows **column by column**, not by trusting the stored
`content_hash` — the three `assert-*` ids that looked divergent on
`content_hash` turned out to have identical titles, descriptions and status,
while the `task-*` ids differ in `status`, `closed_at` and `close_reason`.

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
   fresh read of production.
2. The 27 content-disagreeing collisions have been reviewed and the winner rule
   accepted, or the losing rows have been recovered from
   `brain_unify_collisions.losing_row`.
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
## Verification status: COMPLETED, AND IT FAILED

`bd brain unify verify` has now completed against a full build. It returned
`RESULT: FAIL` with 279 failing checks, and it separated two findings of
completely different kinds. The full run is recorded in
[brain-unify-mini0-run.md](brain-unify-mini0-run.md); the summary follows,
stated plainly because the difference between the two findings matters more
than the count.

**The outcome is one of the three honest outcomes: a named list of genuine
defects with failing checks.** Not zero failures, and not "verification cannot
be made reliable" — verification *was* reliable, and it caught exactly one real
problem.

| | |
|---|---|
| build | exit 0, 990s, **55/55** sources, 685 collision losers skipped and recorded, 359MB unified database, 43 tables |
| verify | completed in **57s**, `RESULT: FAIL`, **279** failing checks |

**Defect B — a genuine migration defect, and it blocks cutover.** Two beads in
namespace `task` silently lose their `metadata` JSON to `{}`: `task-a44d4`
(1116 bytes → 2) and `task-ybur` (1177 bytes → 2), which is 2289 bytes of real
content. It is silent: `brain_unify_import_log` reports `source_rows=5706,
imported_rows=5706, skipped_rows=0` and no collision is recorded for either id.
It is deterministic rather than a race — an independent second build from the
same frozen source reproduced the same two beads exactly. Declared column-type
skew, column-count skew, the write path, duplicate ids, apostrophes, value size,
position in the read stream, and every substring or bigram shared by the two
values were each ruled out by measurement. **The mechanism is not identified.**

**Defect A — a genuine verifier defect.** The other 277 failures cannot pass for
any store, ever: each compares a source-side fingerprint that has no `store`
column against a re-keyed table that has one, and different column sets cannot
produce the same digest. Recomputing both sides by hand reproduces the
verifier's own numbers exactly — source `tasks.config` 10 rows / 476 bytes /
307648475 against unified `brain_unified_config` for store `task` 10 / 576 /
1552887092, the 100-byte difference being exactly the added `store` value
rendered into each of the 10 rows. These are not evidence of data loss, but an
alarm that cannot clear is worse than no alarm: it overstates the problem by two
orders of magnitude and would mask a real regression.

### What is established

**The mapping, against live production.** `bd brain unify plan` runs reliably
and has been run repeatedly against the real federation. It reports 55
participating sources, 123 ids present in more than one database and 27 of those
disagreeing on content. (The bead total is live data and drifts: an early run
read 22,391, the frozen copy the completed build was taken from holds 22,523
issues rows.) The namespace ownership table, the collision winner rule, the
exclusion of `beads_global` and `TinyKeyboard`, and the per-prefix ambiguity
list all come from those runs, not from reasoning about what the code should do.

**The build works at full scale.** `bd brain unify build` completed successfully
against a full federation: **exit 0, 990s, all 55 sources**, 685 rows skipped as
recorded collisions, a 359MB unified database of 43 tables. Earlier runs against
the live federation completed in 4m52s at ~314MB; the completed run was taken
from a frozen copy, and per-source cost is a constant ~15.5s regardless of bead
count, so wall time scales with the number of stores rather than with data. It
reads production through a connection that structurally refuses any non-SELECT
statement, and writes only to a Dolt server it starts itself under `--data-dir`.
Production was never written to at any point.

**The verifier refuses bad input.** Given a deliberately truncated build it
reported `collision agent-0bq appears 0 time(s) in the unified database, want
exactly 1` and failed, rather than comparing partial data and reporting a
result.

**The verifier tells a real defect from its own.** The 279 failures were not one
undifferentiated mass: 277 of them were shown to be an alarm in the instrument
that can never clear, and 2 were shown to be a real, silent loss of 2289 bytes
of data. The second of those was then shown to be deterministic by an
independent build. That is the property that matters most in an instrument like
this — not that it can say "pass", but that it can say *what* is wrong, and
survive an attempt to explain the failure away.

### What is not established

**Why two beads lose their metadata.** Defect B is deterministic and
data-dependent, so it is bisectable, but no property of those two rows has been
found that distinguishes them from the other 5704. Until that mechanism is
identified, the migration is not sound.

**Whether the verifier is sound.** Defect A is an alarm that cannot clear. Until
it is fixed, a genuine regression on a re-keyed table would be indistinguishable
from the 277 that are always there — which is the failure mode this instrument
exists to prevent.

### The verifier defects found on the way

Four verifier defects have been found, all of them in the instrument rather
than in the migration. The first three were fixed in sequence; the fourth is
outstanding and is Defect A above.

1. Fingerprint scanned its aggregate columns into discarded variables, so every
   database-state table compared as zero content.
2. The reference fingerprints were taken by re-reading each source *after* the
   copy, so concurrent writes appeared as migration defects.
3. The client reimplemented the server's column encoding to digest rows at copy
   time; the two implementations drifted by a few bytes per row, producing
   failures indistinguishable from real corruption.
4. Defect A above, found by *running* the verifier to completion rather than by
   reading it: the two sides digest different column sets on re-keyed tables.

(3) was the design fault, and the fix was to delete the second encoding rather
than tolerate its drift, so that exactly one encoding exists and both sides run
it by construction. (4) is the same class of fault that survived that fix: two
sides that must agree, not agreeing by construction.

### Why the earlier attempts did not complete

After the encoding fix the build became materially heavier — the source is now
digested immediately before each table is copied, which roughly doubles the
reads against a live production server. The 90-minute default cut a build off
partway, leaving a partial database (correctly refused by the verifier). With
the budget raised to four hours, the build was killed by the environment.

The earlier account of that kill blamed memory pressure. That was wrong. A
later attempt on this machine was killed with swap **flat at 735MB for the whole
run** while the host's load average sat near 100 on 10 cores: CPU contention,
not memory. The run that finally completed used a host at load ~3 and never
needed memory headroom either — dolt's resident set there reached 16.8GB while
swap stayed unchanged at 98.81MB. Per-source cost is a constant ~15.5s
regardless of bead count (0 beads and 4303 beads both cost 15.5s), so the
scaling axis is **the number of stores, not the amount of data**.

What this costs: for a long time the environment was blamed for a failure that
had a code explanation, and the search for that explanation stopped at the
host. The two findings above were reachable as soon as a run completed; nothing
about them needed a different machine, only a machine that was not already
saturated.

### What would settle it

1. Identify the mechanism of Defect B. It is deterministic, so a bisect from
   the frozen source is the direct route.
2. Fix Defect A in the verifier so both sides digest the same column set.
   This must not be done by skipping the check — the check is correct in
   intent and wrong only in what it compares.
3. Re-run build and verify and confirm that only Defect B remains. `RESULT:
   PASS` is not reachable before step 2, and cutover is not reachable before
   steps 1–3.
