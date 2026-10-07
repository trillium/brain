---
id: 0024
title: isa retired — historical record; primer corrected for staleness
isc: []
status: landed
created: 2026-10-07
updated: 2026-10-07
commits: [ab64c1a00b7e8cfb83726e8559d768f561feda2c]
touches:
  - ISA.md
  - docs/brain/archive/ISA-v03.md
  - docs/brain/README.md
  - docs/brain/WHAT_IS_BRAIN.md
  - docs/brain/WHAT_BRAIN_ADDS.md
  - divergence/README.md
upstream_rebase_notes: |
  Doc-only entry; no code conflicts expected. Everything touched is
  brain-only (ISA.md, docs/brain/, divergence/): resolve `ours`.
  ISA.md was moved (not deleted) to docs/brain/archive/ISA-v03.md, so a
  rebase that touches the old path conflicts only on the delete/add side.
---

# Why

The captain-approved vision — landed verbatim as
[`VISION.md`](../VISION.md) and recorded in `divergence/0023` — states two
things that make this change, both approved "In vision" on the review board:

1. "The ISA document is retired: an ISA is a store type, not a document."
   The captain's reasoning on the card: "retire — no longer needed, ISA can
   now be its own store type." The record-type ISA already lives in the
   store (`kind=isa`, `brain new isa`, the `isa-*` verb family, IsaFormat
   v2.7 — all shipped, see `WHAT_BRAIN_ADDS.md` C-6); the root-level project
   spec document is the part that retires.
2. "The design record is maintained by the agents doing the work,
   versioned and clearly marked, and the documents carry semver." This
   entry and the version markers added to the touched documents are that
   maintenance in practice.

The retirement also surfaced a factual staleness the vision exposes:
`docs/brain/WHAT_IS_BRAIN.md` predates the federation and unification work
— it explains the brain layer over bd accurately but says nothing about the
many named stores, the one binary, the one search, or the consolidation
into a single database with namespaces.

# What changed

Documentation only; no code, no behaviour change.

- **`ISA.md` retired as a historical record, not deleted.** Moved to
  `docs/brain/archive/ISA-v03.md` with a retirement banner stating why
  (per `VISION.md`), what the living truth now is, and which of its claims
  later work outgrew (Out of Scope said federation "is out"; the store
  federation shipped — `WHAT_BRAIN_ADDS.md` §1). Its decision log,
  constraints, capability audit, ISC table and anti-criteria all survive
  there with a named home; nothing was lost and nothing was rewritten —
  the snapshot is frozen as of 2026-05-31.
- **`docs/brain/WHAT_IS_BRAIN.md` — correction pass, version 1.1.0.** Same
  structure and voice; corrected only what is now wrong or missing:
  - The exfiltration hook is no longer "not yet built — ISC-117-121": it
    shipped as `BrainExfiltrationDecorator` (divergence/0012), and the
    render root is store-derived, not hardcoded `~/data/knowledge/`.
  - `kind` gained a fourth value: `isa` (the ISA store-type record).
    Diagram, examples and error-message examples updated.
  - Added the store federation and unified-database paragraph: many named
    stores behind thin wrappers, one registry, one federated search, and
    the consolidation into `brain_unified` with prefix ownership,
    namespace-scoped reads and the explicit `--wide` mode — built and
    proven, not yet deployed. Kept to plain-English framing and pointed at
    `WHAT_BRAIN_ADDS.md` §1–2 rather than duplicating the inventory.
  - §4's "four verbs" is scoped to the four core knowledge-graph verbs;
    the rest of the brain-only verb surface is deferred to the inventory.
  - §12's pointer to the "canonical brain spec" now points at `VISION.md`
    and the archived ISA.
- **`docs/brain/README.md`** — the index reflects what now exists: the
  primer blurb updated (kinds, federation, shipped exfil hook), the
  "Canonical spec" section replaced by the retirement note with links to
  the archive, the vision, and the inventory, and an `archive/` resident.
- **`docs/brain/WHAT_BRAIN_ADDS.md`** — the two references to the retired
  ISA as "the spec" now point at `VISION.md` and the archive; the §8
  provenance line cites the archived path.
- **`divergence/README.md`** — the trail's spec-reference now points at
  `VISION.md` (the ISA was the spec when the convention was written); the
  `# Brain-spec link` contract and frontmatter comment updated to say the
  historical ISA may be cited from its archived home. The stale index
  table (only listed 0001) was completed from the on-disk entries and
  each doc's own frontmatter status.

Deliberately not done: no collision-rule change, no hooks, no event
delivery, no markdown edit-back, no code changes, and no touching of bd's
doc homes (`docs/CLI_REFERENCE.md`, `website/`) — a few lines there cite
the root `ISA.md` path and now dangle; recorded below as a residual.

# Brain-spec link

[`VISION.md`](../VISION.md) — "The design record is maintained by the
agents doing the work" ("The ISA document is retired: an ISA is a store
type, not a document"). The retired record itself:
[`docs/brain/archive/ISA-v03.md`](../docs/brain/archive/ISA-v03.md).
