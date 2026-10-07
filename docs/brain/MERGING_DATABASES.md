# Merging several databases, and keeping the merge current

`bd brain unify` merges several Dolt databases — one per store — into one
database, and `bd brain unify replay` keeps that merged database current while
the databases it was built from are still being written to. This page is the
workflow: which verb to run when, and what each one will refuse to do. The flag
lists are in the [CLI reference](../CLI_REFERENCE.md#bd-brain-unify); the design
of the merge itself (prefix namespaces, the collision model) is in
[`../design/brain-single-database.md`](../design/brain-single-database.md) and
[`../design/brain-collision-duplication-records.md`](../design/brain-collision-duplication-records.md).
What happens to an id that more than one source holds is the section
[Duplicated ids](#duplicated-ids-identical-copies-merge-differing-copies-become-conflict-beads).

Throughout, **sources** are the databases being merged and the **merged
database** is what `build` produces (named `brain_unified` by default).

## The verbs

| Verb | What it does | Writes to |
|------|--------------|-----------|
| `plan` | Reads the sources and prints the mapping: which databases take part, who owns each id prefix, and every id that exists in more than one source with what becomes of it (merged, or a conflict bead and the ids of its copies) | nothing |
| `build` | Copies every source into a new merged database, on a Dolt server it starts itself under `--data-dir` | the merged database only |
| `replay` | Carries what the sources changed since the build (or the last replay) into the merged database | the merged database only |
| `verify` | Compares the merged database with the sources by row count, content size and an order-independent content digest, per table, per store and per namespace | nothing |

No verb ever writes to a source. They open the sources through a connection that
can only issue `SELECT`.

## Why a replay exists

A build takes time, and the sources keep moving while it runs and after it
finishes. Every bead written, edited or closed in a source after the build read
it exists only in that source. If the merged database is going to become the
reference, those changes have to be carried over first, and carried over
exactly: a bead edited in the source and not in the merged database is a
silent loss.

```
bd brain unify build  --data-dir /scratch/merged       # reads the sources as of now
        ... the sources keep being written to ...
bd brain unify replay --data-dir /scratch/merged       # carries those writes over
bd brain unify verify --data-dir /scratch/merged --reference live
```

Replay as often as you like. Each run starts where the previous one read, and a
run that finds nothing new changes nothing.

## How a replay knows where to start

`build` records, per source, the newest Dolt commit it saw before it read that
source (`brain_unify_source_commits`). A replay diffs each source from that
commit to its working set, which names the beads and the database-state tables
that changed. It does not apply the diff's values. For every changed bead it
deletes the merged database's rows for that bead in every bead-scoped table
(`issues`, and the tables keyed by `issue_id` or `parent_id`) and re-reads them
from the sources as they stand now, through the same column encoding the build
used. Inserts, updates and deletes are therefore one operation — *make the
merged rows for this bead equal what the sources hold* — and a bead deleted in
every source simply ends up with no rows.

Two details keep this from missing anything:

- The new starting point is read **before** the diffs, so a commit a source
  makes mid-replay lands in the next window rather than between two windows.
  Windows overlap; re-applying a row is harmless.
- Tables Dolt does not version (`dolt_ignore`d tables such as `wisps`) have no
  history to diff. They are reloaded whole on every replay, and the report marks
  them *reloaded whole*.

Database-state tables (`config`, `metadata`, …) are stored once per store in
the merged database, keyed by a `store` column. A changed one is replaced for
that store as a whole slice.

## Rebuild or replay?

**Replay** when the merged database was built by a build that recorded its
starting points, the set of sources is the one it was built from, and the
sources' histories are intact. That is the normal case, and it is much faster
than a build.

**Rebuild** when any of these hold — the replay refuses all of them (below):

- the merged database was built before builds recorded starting points (it has
  no `brain_unify_source_commits` table). There is nothing to start from, and a
  replay will not guess: rebuild once, and replays work from then on;
- a database joined the merge, or left it, or a store now reads from a different
  database;
- a source's history was rewritten or garbage-collected so that it no longer
  contains the commit the build recorded;
- the schema the merged database was built with no longer matches the sources'
  (the template store gained a table or a column the merged schema does not
  have). The merged schema is fixed when it is built and a replay never alters
  it;
- the merged database was built before differing copies became conflict beads
  (its `brain_unify_collisions` has no `resolution`, `copy_ids` and
  `copy_hashes` columns). Its duplicates follow the old winner rule, which a
  replay will not mix with the new model.

## What a replay refuses to do

A refusal names the store and the table (or the id) and exits non-zero. The
whole decision is made, and every source read, before the first write, and the
writes run in one transaction: **a refused replay leaves the merged database
exactly as it found it.**

| Condition | Refusal |
|-----------|---------|
| No `brain_unify_source_commits` table | built before starting points were recorded; rebuild |
| A participating store has no recorded commit | it joined after the build; rebuild |
| A recorded store no longer participates | a replay will not drop a store silently |
| A store reads from a different database than the one recorded | the mapping moved under the starting point |
| A source no longer contains its recorded commit | history was rewritten or collected; what changed since is unknowable |
| A source's history cannot be read (`dolt_log`, `dolt_diff`) | refused, naming the store and table |
| Two stores share one source database | a replay could not attribute their rows |
| A table the build imported rows from is gone from its source | what became of those rows is unknowable |
| A source table has no column that addresses a bead | its rows cannot be attributed to a bead |
| The template store has a table or column the merged schema lacks | the merged schema is fixed at build time |
| The merged database records no (or several) template stores | the table classification cannot be re-derived |
| The merged `brain_unify_collisions` lacks `resolution`, `copy_ids` or `copy_hashes` | built before conflict beads; rebuild |
| A minted copy id that two copies derive, or that an existing bead already has | the primary key would be ambiguous |
| Two copies of one id carrying the same non-empty `slug` | the merged database holds slugs unique |
| A bead-scoped table keyed by several columns none of which is the bead id | a conflict's two copies could not be given distinct keys in it |
| A row cannot be inserted (for example a primary-key clash between stores in a table that is not keyed by bead) | refused, naming the store and the merged table |

## Duplicated ids: identical copies merge, differing copies become conflict beads

`plan` lists every id that more than one source holds. What `build` does with it
depends on one test: **do the copies differ in any column?** (The stored
`content_hash` is not trusted; the rows are compared column by column.)

**Identical copies merge into one bead.** One copy stays, the others are
skipped, and the child rows of a skipped copy are left out with it. Which copy
stays does not change any content, but it is deterministic: the store that owns
the id's prefix, else the most recently updated copy, else the lexicographically
smallest store name. The skipped copy is preserved in full in
`brain_unify_collisions.losing_row` (`resolution = merged-identical`).

**Copies that differ are all kept; the id becomes a conflict bead.** Nothing is
picked and nothing is discarded:

- **Each copy becomes a bead of its own**, under a new id
  `<prefix>-<12 hex>`: the prefix of the store that authored the copy, and the
  first 12 hex digits of `sha256(original id, NUL, store)`. The same sources
  always give the same ids. The new bead carries that copy's row and every child
  row it authored (labels, comments, events, the dependencies it authored, ...)
  under the new id. Where the two copies share child-row keys - the brain store's
  copy of a bead was copied from the other store's, so their `events`,
  `comments` and `dependencies` rows carry the same uuids - the moved rows get a
  new deterministic key per copy.
- **The original id stays a real bead: the conflict bead.** It is `open`, its
  title says it is an unresolved duplicate-id conflict, its description lists
  both minted ids with the store each came from (and each copy's status,
  `updated_at` and title, and the columns that differ), it carries the
  `unify-conflict` label, and it has a `tracks` dependency to each copy. `tracks`
  is bd's existing non-blocking edge: the conflict neither blocks its copies nor
  waits on them.
- **Links other beads hold to the original id are not rewritten**, so they now
  reach the conflict bead. A bead that *blocks on* a conflicted id therefore
  blocks on an open conflict until it is resolved.
- Each conflict is recorded in `brain_unify_collisions` with
  `resolution = conflict-bead`, `copy_ids` (`{store: minted id}`) and
  `copy_hashes` (a digest of each copy's rows as they should be in the merged
  database). No `losing_row`: nothing was skipped.

There is nothing left to override, so **`--allow-collisions` is retired**. It is
still accepted by `build` and `replay` as a deprecated no-op so existing scripts
keep running. A resolution command (fold the copies, close one, close the
conflict bead) is not part of `unify`.

What a replay adds is that the resolution can change after the build, and it
re-derives it with the build's own functions, deleting every id the old and the
new resolution used:

- a **new** bead whose id another store already holds becomes a recorded
  duplicate - merged if the copies are identical, a conflict bead plus its
  copies if they differ;
- a duplicate whose copies are **edited** is re-derived: identical copies that
  now differ become a conflict (the one bead is replaced by a conflict bead and
  two copies), a conflict's copies pick up the edit, and a conflict whose copies
  became identical again merges back into one bead (the minted ids are removed);
- a duplicate that **goes away** loses its record, and the surviving copy is a
  plain bead under the original id again, its minted siblings and the conflict
  links gone.

## The verifier is the acceptance test

`verify` has two references:

- `--reference recorded` (the default) compares the merged database with the
  fingerprints the build — or the last replay — recorded. It measures the
  build, and is deterministic against sources that were frozen when it ran.
- `--reference live` recomputes the expected side from the sources **as they
  stand now**, with the same server-side expressions and the same exclusion of
  skipped copies and conflicted ids, and compares that. It is the acceptance test for a replay: a
  replay that missed a change fails it, naming the table and the namespace that
  differ, and so does any write a source makes after the replay.

For conflicts both references do the same checks, one conflict at a time, on top
of the fingerprints (which leave the conflicted and minted ids out of both sides,
so a bead the plan does not name in a minted id's namespace is still a
difference): the record names exactly the copies the sources call for; the
original id is one `open` issue whose rows, label and `tracks` dependency to each
copy are exactly the rows the tool authors, with nothing else at the id except
rows of stores that hold no copy; and each copy is one issue whose rows in every
bead-scoped table match, by a digest over rows read the same way on both sides,
the source's copy put through the build's mapper - the live source, or the digest
the build recorded.

A replay is done when `verify --reference live` passes. If it fails, the report
says where; run `replay` again (it picks up what moved since) and re-verify. On
sources that are being written to continuously a live verification can fail
for a change made between the replay and the check; that is the same failure,
not a flaw in the replay, and the remedy is another replay. Quiesce the sources
(or accept a final short replay) for the verification that gates a cutover.

A replay also writes its own measurements back (`brain_unify_source_fingerprints`)
and its own commit into the merged database's Dolt history, so the default
`verify` keeps passing after a replay.

## A worked order of operations

1. `plan` — read the mapping and the collisions: which duplicates merge, and
   which become conflict beads (with the ids of their copies).
2. `build` — once. Slow.
3. `replay` — repeatedly, while the sources are in use. Fast.
4. Stop writing to the sources.
5. `replay` — one last time.
6. `verify --reference live` — must pass before the merged database is treated as
   the reference.

If step 3 refuses, the refusal says whether the answer is a rebuild or a repair
of the sources; nothing was written.
