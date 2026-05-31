---
id: 0005
title: brain vs bd CLI vocabulary explainer
isc: []
status: proposed
created: 2026-05-31
updated: 2026-05-31
commits: []
touches: [docs/brain/BRAIN_VS_BD.md, docs/brain/README.md]
upstream_rebase_notes: |
  Both files are brain-only. `docs/brain/` did not exist upstream
  (bd's docs root is `docs/`, untouched by brain by convention).
  On any bd → brain rebase that touches the bd `docs/` root, resolve
  `docs/brain/*` paths with `ours` — they have no upstream counterpart.
---

# Why

Trillium asked, mid-tranche: "how is `brain new / show / list / link
related` different than beads existing commands? I need that explained
to me and a doc describing it with diagrams and stuff."

The answer was already implicit across ISA.md (vision, criteria,
features) and the prior four divergence docs (genesis, decisions, the
modularity-first architecture, the BrainVerb seam landing). But none
of those single documents read as "here is the difference, in plain
English, with diagrams a phone-reader can absorb in five minutes."

This commit lands that single document.

It is **exposition**, not a new architectural commitment. Every claim
in it traces back to a decision already ratified:

- The kind discriminator → Decision #1 (Go-only constitutional via the
  fork-direct ratification) + ISA §"Decisions" → kind discriminator.
- The thin-wrapper-over-bd-primitives architecture → Decision #5
  (modularity-first, divergence/0003).
- The two new edge types (extends, learned-from) → red/green commits
  259d17a82 and c4b6a78e4, before the divergence trail formally began.
- The BrainVerb seam diagram → divergence/0004 (the seam landing).
- The exfiltration flow diagram → ISC-117-121, named but not yet built.

If the doc and any of those sources ever drift, the upstream ratified
decision wins and this doc gets corrected.

# What changed

Two files land. Zero existing files modified except `docs/brain/README.md`
which gains a single index pointer at the top of the "What lives here"
list.

**`docs/brain/BRAIN_VS_BD.md`** — single-file explainer, eight sections:

1. The 30-second answer (one paragraph: "brain is a verb-vocabulary
   lens over bd, not a fork of bd's data model").
2. Mental-model shift (queue → graph) with side-by-side ASCII art.
3. The kind discriminator — the only schema-shaped new idea, with
   concrete `bd create` vs `brain new` examples showing the same
   `issues.issue_type` column writes.
4. Command map table — bd verb / brain verb / real difference for
   every verb in the first tranche.
5. Each of the five verbs in detail (`brain new`, `brain show`,
   `brain list`, `brain link`, `brain related`) with example output
   and the bd analogue. `brain new` includes the full BrainVerb →
   storage → HookFiringStore → Exfiltrator plumbing diagram.
6. The 18-edge-type vocabulary inventory (16 bd + 2 brain).
7. The whole-system plumbing diagram showing both CLI surfaces
   converging on `internal/storage` as the shared substrate.
8. Quick reference card and pointers to ISA + divergence trail.

The doc is intentionally phone-readable: short paragraphs, ASCII
diagrams (not Mermaid — renders everywhere), code blocks with real
commands and real-shaped output, and no abstraction Trillium hasn't
already ratified in a higher-level decision.

**`docs/brain/README.md`** — adds one line under "What lives here" so
a reader landing on the index finds BRAIN_VS_BD.md before the
expected residents that don't yet exist.

# Brain-spec link

This doc reflects, but does not advance, the following ISA elements:

- `## Vision` — the verb vocabulary lens
- `## Decisions` → Decision #5 (modularity-first) — the seam this
  vocabulary uses
- `## Features` → cli-aliases row — the surface this vocabulary
  documents
- `## Capabilities` → kind discriminator, exfiltration hook, BrainVerb
  seam, edge-type registration — all referenced inline as the
  underlying mechanisms

# Cross-system mirror

A byte-identical copy of `docs/brain/BRAIN_VS_BD.md` also lives at
`~/.claude/PAI/DOCUMENTATION/Brain/BRAIN_VS_BD.md` (a new PAI
documentation subsystem dir following the precedent of `Algorithm/`,
`Memory/`, `Pulse/`, etc.). The PAI mirror is not tracked in this
brain repo; it is part of the PAI tree (not under brain's git).
Maintenance rule: when the brain-repo copy changes, copy the new
content to the PAI mirror. The brain-repo copy is authoritative.

If divergence between the two ever becomes load-bearing, replace the
PAI mirror with a pointer file that says "see brain repo
docs/brain/BRAIN_VS_BD.md" and accept the cross-tree dereference cost.
