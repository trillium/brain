---
id: 0024
title: duplication-records design — the vision's collision model, designed not built
isc: []
status: proposed
created: 2026-10-07
updated: 2026-10-07
commits: []
touches:
  - docs/design/brain-collision-duplication-records.md
  - docs/design/brain-single-database.md
  - divergence/0024-brain-collision-duplication-records.md
upstream_rebase_notes: |
  Doc-only entry; no code changes expected on rebase. brain-single-database.md
  is the document the design superseded sections of; if its collision section
  or verifier paragraph move on rebase, the references in
  docs/design/brain-collision-duplication-records.md need re-anchoring.
---

# Why

The captain approved a vision for brain on a board; on the collision question
it said two stores holding the same id become a duplication record with new
ids for both copies, verdict "In vision". What shipped in the unification
build is a different model: the winner rule (prefix owner, then newest
`updated_at`, then lexicographic source) keeps one copy under the shared id
and archives the loser verbatim in `brain_unify_collisions.losing_row`. The
31 divergent duplicated ids were reviewed column by column afterward
(`item2-collision-review.md`, `needs-decision` key `item2-winner-rule`) and
the winner rule was accepted for them. divergence/0023 recorded the gap
between the approved wording and the shipped rule as a caveat.

This entry pairs the design that closes the conceptual half of that gap —
the duplication-record model, designed against the existing plan → build →
verify pipeline, with nothing implemented, nothing cut over, and no write to
production.

# What changed

- **`docs/design/brain-collision-duplication-records.md`** (new) — the
  duplication-record model: what a duplication record is (tombstone shape,
  `copy_ids`, the original id resolves to the record and not to a bead);
  deterministic per-copy re-identification (the authoring store's prefix, a
  sha256-derived suffix, refusal rather than rewrite on a derived-id
  collision); what happens to the 31 reviewed ids and the accepted winner
  rule (the content review stands; the model itself needs a fresh captain
  approval because its cost is different); what the verifier must check
  instead of "each duplicated id appears exactly once"; child rows and
  cross-store links of both copies; and the migration path
  (recommendation: rebuild from sources, not in-place adoption).

# Acknowledged caveats

- **The design contradicts landed text on purpose and says so.** The shipped
  "Collisions: proven, not asserted" section (the winner rule and its tie
  order), the verifier's "each duplicated id appears exactly once" check, and
  the runbook's precondition item 2 (winner rule accepted) remain in
  brain-single-database.md and the runbook unchanged. The new design document
  names each superseded passage at the top rather than editing it, because
  this is a design that has not been approved — the authoritative text on the
  winner rule stays the shipped design until the captain adopts the new one.
- **The winner rule's acceptance is not reopened by this design.** The
  per-id content review established which copy's columns were truthful; that
  judgement also matters under the duplication model (it is what a record's
  `losing_row` and digest columns already record). What the model does need
  fresh is approval of the model itself, because its cost — two live rows
  where the shipped build produces one — was never on the vision board.
- **Nothing is implemented and nothing is authorized.** Design only. No
  behaviour change, no build, no verify, no cutover step, no write to any
  production path. The cutover runbook's "what must be true before step 3"
  list is untouched; nothing here moves a store.

# Deliberately not done here

The runtime read path (how `bd show <a tombstoned id>` presents the record,
its exit code, narrow vs. wide presentation), any edit to
`internal/brainunify/`, and any edit to the built unified database or the
wrappers. The design names these as follow-up work. The captain decides the
three open questions listed in the design's closing section before this
proposal turns into implementation.
