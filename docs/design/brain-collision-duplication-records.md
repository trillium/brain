# Duplication records: when two stores hold the same id

**Status:** design only — nothing is built, no behaviour changes, nothing is
cut over, production is untouched. This document proposes; it does not land.

**What this contradicts:** sections of
[brain-single-database.md](brain-single-database.md) are superseded by the
model here — the "Collisions: proven, not asserted" section (the winner rule
and its three steps), the verifier paragraph **"each duplicated id appears
exactly once"**, and row "What must be true before step 3" item 2 (the
winner-rule acceptance). Those passages remain landed and are referenced where
they diverge. This is a deliberate, recorded contradiction, not a silent
rewrite; see the "Already landed" sentence in each superseded section of
[divergence/0025](../divergence/0025-brain-collision-duplication-records.md).

---

## Why: the approved vision, and what shipped instead

The captain's approved vision says, in "Fix the mechanism, not the symptom":

> When two stores hold the same id, that id becomes a duplication record: both
> copies receive new ids, and the collision record keeps the lineage.

What shipped is a different model. The unification build applies a **winner
rule** — prefix owner's copy wins, else the newest `updated_at`, else the
lexicographically smallest source name — and one copy survives under the
shared id as `issues` primary key. Every losing copy's full row is preserved
verbatim in `brain_unify_collisions.losing_row` with the winner's source,
divergence, and per-copy content hashes alongside it. Nothing was lost
silently, but the shape is "one keeps the id, the loser stays recoverable",
which is not "both, re-identified".

The 31 divergent duplicated ids were reviewed column by column afterward and
the winner rule was accepted for them (`needs-decision item2-winner-rule`),
with the design doc, the collision review, and the cutover runbook all resting
on that. The reviewed divergence set is at
`/Users/mini0/fm_home/mini0-ops/data/brain-unify-verify/item2-collision-review.md`
(31 `divergent=1` rows: 24 content, 7 bookkeeping-only; the doc's count of 27
was written before the federation diverged further).

The gap is substantive, and it is worth stating in one line of cost: under the
winner rule, the loser is data recovered from a side table. Under the
duplication-record model, the loser is a live bead. Queried as a bead, one
returns a nonexistent id; the other returns a real record with children that
resolve.

What follows designs the duplication-record model against the existing build
(`internal/brainunify/`: plan → build → verify) with the smallest change
surface, and names the consequences each option carries.

---

## 1. What a duplication record is

**Where it lives.** `brain_unify_collisions` becomes the duplication-record
table. Its name stays (no behaviour wraps it yet, so there is nothing to
break); its shape gains three columns, an index, and stops owning two:

- `copy_ids` JSON: `{source → new-id}` per copy, with the *record's* keys
  ordered by source. All copies appear — winner and losers alike, because
  under the new model **no copy wins**: the shared id is retired from the
  `issues` table and every participating copy is re-identified.
- `original_id_is_live` tinyint — retro flag for a build before the model
  landed. A build under the new model writes 1; an existing row from
  `brain_unify_collisions` written by the winner-rule build reads 0.
- `minted_at` datetime — when the record was written (the build's run stamp),
  so a future audit can tell a fresh build's records from a row written by an
  earlier winner-rule build.

`losing_row` is **kept, not removed**. A row's `losing_row` continues to hold
whichever copy was *not* authored by the prefix-owner's store — under the
duplication model that copy is a live bead, and the JSON is the last readable
byte-level image of the row at the moment it was captured by the build. It is
the mechanism that keeps this document honest about what the copies' content
actually was at build time independent of any runtime mutation.
`winner_hash`/`loser_hashes` are kept for the same reason: they record what
was compared, not what was decided.

**What it holds.** A duplication record is the table's existing identity
(id, prefix, owner, winner, losers, reason, divergent, winner_hash,
loser_hashes, losing_row), plus `copy_ids`. The record is the *only* place the
retired original id resolves to a bead — it names the copies' new ids, where
each copy came from, and what the copies' content was.

**Does the original id survive as a record at all?** Three shapes exist:

| Shape | Original id | What it does afterwards |
|---|---|---|
| **Tombstone** | gone as a bead id; present only inside the record row as the record's primary key | A reader asking for it gets the record, and the record alone. |
| **Alias / redirect** | resolves through the record to a designated "best" copy | A reader asking for it is redirected to one of the two newly minted ids. |
| **Both kept, plus a third "record" id** | the record carries the original id as a third, bead-shaped key, so `show <original>` returns the record, rendered like a bead | A reader asking for it gets a bead-shaped row that is not a bead. |

**Consequences.** *Tombstone* is the most honest: the id existed as a
primary-key accident; after the model it names two things, so it is retired as
an identifier and kept as the record's own address. It costs nothing at build
time and costs the reader nothing (the CLI's response to it is "this id
duplicates; here are the two copies", with a non-zero exit so scripts fail
loudly). *Alias* is convenient but re-introduces a winner rule by the back
door — some copy gets the traffic — and it re-couples the very thing the
model separates (a reader should reach a copy through its own id). "Both kept"
is honest at runtime but costs a fake bead the record carries and a shim the
steady-state view must carry forever, for ids (123 in the current federation)
that are not production traffic on their own.

**Recommendation: tombstone.** It is the reading of the approved vision that
fits: the id *becomes* the record, so the id no longer answers "which bead is
this?", and it answers "which beads used to be called this?" instead. It also
matches the standard the vision sets on honesty — "an honest gap in a record
beats a fabricated entry that closes it" — and with no aliasing there is no
mechanism to get out of sync with the record table.

**Whether a lookup attempt resolves.** A tombstoned id resolves *as a
duplication record*. `bd show agent-0bq` (after the model is adopted) can
return, with the command naming the duplication: "agent-0bq is a duplication
record: two copies were kept as `agent-abc12` (robot store) and
`brain-xyz98` (brain store). Use those instead." It must refuse the
fabrication paths: refuse to fabricate a bead from the record, refuse to
silently pick a copy. The existing command surface decides how it presents
that; this document's only claim is the id resolves *to the record*, not 404,
and not a bead.

---

## 2. What the new ids are

**Both copies are re-identified.** No copy continues to be addressed by the
shared id, so the model stays symmetric — nothing in the record is a "kept
side".

**Which prefix each copy carries.** Every copy carries the prefix of the store
that authored it. Concretely, one copy is authored by `projects`, so its new
id's prefix is `project-…`; the brain-store copy of the same row is minted
`brain-…`. Two reasons:

1. Ownership is already recorded (`brain_store_prefixes`), so a copy's prefix
   is a lookup against a table the build fills in deterministically, not
   something the build re-derives. This matches the model's own rule that
   addressability ("which store is this bead in") is answered from the prefix
   record.
2. Each copy's *narrow read view* is which store authored it, and each copy
   was written by a store's own writing surface; a bead authored in brain
   should be visible in brain's own view, not only in the namespace that
   decided the collision.

Alternatives and their costs:

| Shape | What it means | Gets you | Costs |
|---|---|---|---|
| **Author's prefix, per copy** (recommended) | a brain-authored copy of `project-2g7` is minted `brain-…`; the projects copy keeps a `project-…` shape | each copy lands in the narrow view of the store that wrote it; prefix continues to answer "which store" | reads that used to find them under `project-…` must go through the record first; the namespaces diverge from the original id's prefix |
| **Shared-prefix shape for all copies** | both copies minted `project-…` | the original family keeps one prefix; the win-rule residue ("this is an agent bead") persists | both copies look like the owner's store's rows in `issues` unless you read the record; the brain copy is an agent-store bead carrying a `project-…` prefix, which is the situation the prefix model was built to end |
| **Synthetic third prefix (`dup-…`)** | both copies minted `dup-…` | collision is maximally obvious in a listing | a new prefix the federation does not own has to mint, claim, and store; reads re-route through a prefix the stores themselves did not author |

**Deterministic, two-build-agreeable minting.** The suffix is derived, not
generated. For a copy `(original id O, authoring source A)`:

    new_id = <prefix-of-A> + "-" + first(4, hex(sha256(utf8(O + "\x00" + A))))

- `O` and `A` are read from the source rows and from `brain_stores` at plan
  time, so two builds against the same frozen copy derive the same ids
  byte-for-byte, with no clock, counter, or read order in the input.
- "\x00" keeps `(id, store)` pairs from folding together (e.g. `agent-1b` in
  store `a` vs. `agent-1b2` in store `agent`, which could collide without a
  separator present in the digest input).
- 4 hex chars (16 bits) is too small a space for ids minted at federation
  scale (22,523 issues rows at build time); the 96-bit space of 12 hex
  characters is where "every distinct `(O, A)` gets a distinct suffix" is
  effectively the case the model can actually keep, so **the suffix length is
  12**, matching the existing `abc12`-shape of the federation's minted ids and
  keeping `ExtractIssuePrefix`/`PrefixOf` (first/last hyphen rules) parsing
  the result correctly: both return the store's prefix.
- **A derived-suffix collision is a build refusal, not a rewrite.** The
  uniqueness of the `issues` primary key is the invariant; if two distinct
  `(O, A)` pairs derive the same id, the build stops with the refusal named
  (the same shape as the runtime mint-slug refusal that names what it
  refuses). A second four characters of hash suffix would be the escape
  hatch — but only if the build stopped first, so the refusal stays
  deliberate rather than silently retrying a longer digest.
- **Hierarchical ids** (`brain-se7t.389`) keep their shape through their
  positional suffix if they carry one: the derived-new-id for a hierarchical
  original carries the same positional part. The rationale is that
  `ExtractIssuePrefix`'s and `PrefixOf`'s answers for both the old and the new
  id are the same store prefix; nothing at build time re-homes a hierarchy.
- **Where lineage lives for a reader.** `brain_unify_collisions.copy_ids`
  maps `(authoring store → new id)`; the *reverse* — "which record owns this
  new id" — is answered by an index the build creates once per record row on
  `copy_ids` hashed values. The index is what keeps "fetch the two beads that
  used to be `agent-0bq`" a read of two ids rather than a scan of a JSON
  blob, which is the whole reason the copy_ids are stored in an indexable
  column group rather than only in text.

---

## 3. The 31 reviewed ids and the accepted winner rule

The review accepted the winner rule for the 31 divergent duplicated ids
(`/Users/mini0/fm_home/mini0-ops/data/brain-unify-verify/item2-collision-review.md`),
per id, with its evidence. Under the duplication-record model that acceptance
is **not reopened, and never was a vote to be reopened**: what the review
established was (a) which copy's content was the *truthful* one per id, and
(b) whether keeping the loser under the winner's id lost anything that a
reader needed. It did not decide "should there be two live rows or one."

It does mean the *review's own finding about truthfulness* carries over, and
that is worth stating plainly, because it changes what a duplication record
chooses to do with both copies on hand:

- For 24 of 31, the prefix-owner's copy is the truthful one and the loser is
  the brain store's stale federation snapshot. Under the duplication model,
  **both copies exist as live beads**: the truthful (owner-authored, current)
  copy is the one a reader finds on its own store, and the losing copy is
  live too, reachable through its own newly minted id under brain's prefix.
  It is still stale; both copies were stale **before the review's
  recommendation is what tells future reading surfaces which bead has
  current state**. That is different from the winner-rule build, which
  dropped the loser into a JSON column and did not, by default, make a
  reader face two live status fields about the same bead.
- For 7 of 31, the divergence is bookkeeping-only, and the outcome mirrors the
  winner rule: both copies live, and the difference is noise about the same
  content under two ids.

**What this needs from the captain.** Two distinct asks, and they are not the
same ask:

1. **Re-confirming the 31 per-id content review** is *not* needed. The
   review's content judgement (which copy is stale, which is newer, which
   columns actually differ) is true independent of which row wins; nothing
   re-litigates those columns.
2. **Confirming the model itself and its adoption path** is a captain call
   (this doc is not approved to build). It is a different approval from the
   vision-board verdict, because the vision states intent and this document
   states what the intent costs: two live rows where one already exists, a
   verifier check rewritten, and a reasons column rewritten.

---

## 4. What the verifier must check instead

The shipped check — "each duplicated id appears exactly once in the unified
database, and it is the recorded winner" — no longer applies. What replaces
it, replacing the shipped `confirmCollisions` (the count against the unified
`issues` table, the winner-source comparison, and the "confirmed" count):

For every row in `brain_unify_collisions`:

| # | New check | Failure it catches |
|---|---|---|
| 1 | the original id does **not** appear in `issues` (count = 0) | a build that left a winning copy under the retired id, or reinserted a loser under it |
| 2 | every copy in `copy_ids` exists in `issues`, exactly once, under its minted id | a build that minted only some copies, or minted one with the wrong id shape |
| 3 | every copy's content digest matches the digest the plan computed for that `(original id, source)` pair | a build that re-wrote a copy's content or dropped an appended note during re-identification |
| 4 | no extra `issues` row exists in either copy's prefix that traces to this record but is not named in `copy_ids` | a build whose re-map and its record diverged |
| 5 | both copies' child rows resolve (counts recurse) | a child table (whose only access path is `issue_id`) left pointing at the retired id so a protagonist row would look found while its children are not |

*(Effort is a fingerprint recomputation over the two copy ids and the original
id per record, reusing the existing per-row XOR aggregation. The check is
still order-independent and still re-reads every row on both sides — the same
guarantee the existing `confirmCollisions` gives today, applied to a different
shape.)*

**What makes a build fail**, above and beyond today's divergent-collision
block: a duplicated id found under the original id, a missing copy for a
declared copy id, a digest mismatch on a re-identified copy, and a
derived-suffix collision (see §2). Today's `--allow-collisions` still governs
its own existing choice: whether a *divergent* build may exchange its
content-identical option's content. Whether a build where the copies' column
values differ at all may go through under the duplication model is a runtime
question; the constraints in §4 above already guarantee both live later.

---

## 5. Child rows and cross-store links

Child rows ready to hand off are: `labels`, `dependencies`, `events`,
`comments`, `isa_sections`, and the wisp tables — every table whose seed
column is `issue_id` (or `wisp_id`) and which the build classifies today as
"issue child".

**Child rows of both copies.** Under the winner rule today, every child row of
a losing copy is dropped (with the losing copy itself) so the unified database
does not carry relationships to an id whose winning row came from elsewhere.
Under the duplication model, **every child row is rewritten to reference its
own copy's minted id.** This is the honest shape of what the model says: the
loser is a live bead, so the children the losing store authored for it
(labels it carried, comments made about it, dependency rows, event rows, isa
sections) are the losing copy's *observations*, and they move with it.

What does not follow — and what deserves to be stated rather than assumed:

- **The losing copy's children may include rows whose *target* was the copied
  bead only in as much as the copy was live in the losing store.** A
  `comments` row written on the brain store's `agent-0bq` "this is still open"
  comment is factual about what the brain store recorded at the time; it does
  not become a comment about the robot store's live closed bead. That is the
  correct reading, not a bug: a copy is a bead plus its own observations.
- **A build that re-writes a child row must not need to re-derive anything
  about *when* it was written.** Event rows carry timestamps; the copy
  boundary is untouched. Re-writing is strictly: in the child table, for each
  row whose `issue_id` (or `wisp_id`) equals the original id, set it to that
  copy's minted id, for the copy the originating store's snapshot holds. That
  read is already staged (source rows are read per-copy at plan time), so the
  re-write is a join at build time, not a second read pass.

**Cross-store links** (a `dependencies` row from a third store pointing at a
duplicated id it does not own). Three shapes, with consequences:

| Shape | What happens | Gets you | Costs |
|---|---|---|---|
| **Point into the record's owner copy** | a link from a third store to `agent-0bq` resolves to whichever of the two copies the `brain_store_prefixes` owner authored | one address, one resolution, no ambiguity | the link's meaning silently re-targets a specific copy, so a third store's intent ("watch this bead") is bounded to the owner's copy, not both |
| **Point into the record** | the link is re-written to reference the record (the id is the record's key) | the "what this pointed at before" question is answered by one id, in one place | every read of a dependency now has to know the record shape, not just issues-shape; corrupts the reader model "a dependency's target is a bead" |
| **Leave the original id as a link target and let it be dead** | the link keeps its text; reads that follow it return the tombstone answer the record itself gives | zero re-write at build time, so the cost of the model to the link layer is zero | a dead link is a silent failure; the model's own stance ("a link whose target exists in no store is refused when it is written... names the store that should own the target") refuses that at write time, so shipping a build that fabricates a dead link to save the bridge from re-writing is exactly the stand the model says not to take |

**Recommendation: point into the owner-authored copy.** Reasons: (a) a
cross-store link is text a *third* store wrote about a bead, and after the
model the one bead that third store meant is the truthful one — which the
collision review showed is the owner's copy in all 31 cases, and the prefix
table already answers "which store owns this prefix" deterministically; (b)
the alternative (dead link) violates what the vision says about links that
cross stores ("become an ordinary link in the one graph... the boundary it
crossed is recorded"); (c) re-pointing into the record's own identity is not
cheaper and requires every reader of `dependencies` to know the record shape.
Costs to carry: a link that re-points into the owner copy does **not**
re-point to a lost copy when the losing copy's content mattered; carry that
answer in the record's `losing_row` (which stays, verbatim per §1). If a
build's per-row divergence report matters (e.g. a tool comparing the two live
beads), the record is where a build's divergence summary lives, and the two
live ids can be read from it directly.

The record table, retrofitted this way, carries both the followers and the
referenced-from direction of every relationship that crossed the collision
boundary at all. Both the "these were the same bead once" statement (the
record) and its per-copy observations (the live children) remain addressable
with no second edge type, which is what the approved vision demands of a
collision.

---

## 6. The migration path for an already-built unified database

The unified database exists: 394 MB on disk, build read-only from a fresh
frozen copy of production, verified 518/518 checks. Two models:

| Path | What it means | Gets you | Costs |
|---|---|---|---|
| **Rebuild from sources under the new rule** | `bd brain unify build` is re-run from production with the duplication model in place of the winner rule; the existing unified database is discarded, and the new build's `brain_unify_collisions` carries `copy_ids`, tombstoned original ids, minted copies, re-pointed cross-store links, re-pointed child rows and the new verifier | deterministic, proven to be read-only against production, and it stays the "one clone of the contents" flow the design already promised; the verifier's re-identification checks apply to a fresh build, not to a patched one | the build took 16.5 minutes on the rehearsal baseline (990 s for 22,523 issues rows over 55 sources on a frozen copy), it needs a ~400 MB scratch directory, and the verifier takes ~2 min on it; a second full run is a real clock cost and needs the scratch directory again |
| **In-place adoption on the existing unified database** | read `brain_unify_collisions.losing_row` back, mint each copy from the record, insert the losing copies as new rows, tombstone the original ids (delete the winning rows, carry all rows through their minted ids) | no second production read pass, no 400 MB rebuild, ~123 row re-writes + child re-pointing inside the existing 394 MB database | live DDL against an already-built database — including a primary-key rewrite on `issues` for the re-identified winners; any inconsistency between the record's JSON and the rows it describes (the JSON was captured at one instant and its point-in-truth is not what "verify" re-derives from) is silently carried forward rather than corrected by reading the sources again |

**Recommendation: rebuild.** The unified database is not yet in production
(no cutover has run; the cutover is at step 3 of the runbook and has not been
authorized). The winner-rule build's `brain_unify_collisions` was itself
written against a frozen copy, and reading a JSON column back out to reverse
the winner rule silently drops the one thing the winner rule was built to
prove: the build's byte-level comparison, re-derived from the sources, is the
check — not a JSON archive. A rebuild is also what the record model's own
determinism claim was made against: two builds over the same frozen copy
produce the same `copy_ids` byte-for-byte, and the verifier's #2 check (every
copy exists under its minted id exactly once) is designed to be checked on a
build, not retro-applied to a live database. What it costs is named above —
one scratch directory and ~18 minutes of build time; the same cost the first
build paid. The rebuild happens before any store is re-pointed, so no live
data is affected and no rollback story is needed beyond deleting the new
scratch directory.

**What stays already-decided:** the runbook's steps 1–7 do not change shape;
step 2 ("Run `build` + `verify` into a scratch directory") is where a rebuild
under this model slots in, and the runbook's "do not take step 7 until the
unified database has been backed up, verified a second time, and run in
production" is the ceiling the model's own adoption stops short of. Nothing
here authorizes a cutover, a run against production's write path, or any
store re-point.

---

## What this deliberately leaves to the captain

1. **Approving the model itself.** The vision approved *that* the id becomes a
   duplication record, not what the model's costs are once written down.
2. **Whether a divergent build proceeds under the duplication model without
   `--allow-collisions`**, or whether the summary still refuses by default
   per the review finding that the truthful copy is the owner's in all 31.
   (This design leaves it possible, since both copies survive either way;
   what "possible" should cost in build time is the captain's call.)
3. **Whether a hierarchy child (`.N` variant) keeps the positional suffix**
   in its minted id, or whether every re-identified bead is shaped flat and
   the hierarchy is recoverable from the record only. The design assumes
   keep-the-shape for the reasons given; it is stated as a consequence, not
   decided.

## What is deliberately not designed here

- The runtime read path (`bd show agent-0bq` naming the record by exact shape,
  exit codes, wide vs. narrow presentation) — that is behaviour, and this
  document is design only.
- Any edit to the production database, to the built unified database, to the
  wrapper configuration, or to `tool` selection. Nothing here is implemented.
- Recovery of any losing row into a live bead outside this model's own flow
  (today that verb does not exist and the shipped participating build reports
  `losing_row` as read-only data; the model does not need it and does not
  depend on it later).

## Already landed, deliberately not superseded here

The **cutover runbook** does not need re-shaping: step 2 is the slot the
rebuild fills, and nothing in the runbook names the winner rule,
`brain_unify_collisions.losing_row`'s read-plane role, or "exactly once"
semantics at runtime. Where the shipped design doc states the winner rule as
its own decision and "the 31 accepted under it" as its own acceptance, the
divergence entry for this design names the specific sections it deviates from
rather than silently editing them.
