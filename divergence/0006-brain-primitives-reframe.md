---
id: 0006
title: brain primitives reframe — brain IS bd renamed, tasks and knowledge mix freely
isc: [ISC-104, ISC-105, ISC-106, ISC-107, ISC-108, ISC-109, ISC-111, ISC-114, ISC-117, ISC-118, ISC-251]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: [523accff2f31fc83d46a7a0bb0b9a3cd04cebaae]
touches:
  - docs/brain/WHAT_IS_BRAIN.md
  - docs/brain/BRAIN_VS_BD.md
  - docs/brain/README.md
  - divergence/0006-brain-primitives-reframe.md
upstream_rebase_notes: |
  All paths are brain-only. `docs/brain/` does not exist upstream.
  On any bd → brain rebase that touches `docs/brain/*`, resolve with
  `ours`. The file rename `BRAIN_VS_BD.md` → `WHAT_IS_BRAIN.md` is
  recorded by `git rm` + new file (rather than `git mv`) because the
  content was substantially rewritten — git's rename detection may or
  may not show the link in `--follow`, which is fine: divergence/0005
  is the historical anchor for the old name and 0006 is the anchor
  for the new one.
---

# Why

Trillium read `docs/brain/BRAIN_VS_BD.md` (landed `16be4657b`,
documented in `divergence/0005`) and corrected the framing on the
spot. Quoted verbatim from the reframe direction:

> brain IS bd, renamed. Not a layer. Not a wrapper. Not a namespace.
> There is one binary that gets renamed to `brain` at install time
> (bd's rename mechanism supports calling the binary anything — fork,
> spoon, ops, idea, brain). Every bd verb is reachable as `brain
> <verb>`. `brain ready` is literally `bd ready` — same code, same
> query, the binary just answers to a different name. The new
> vocabulary `brain new` / `brain link` / `brain related` are
> additional verbs added to bd, not aliases routed through a
> brain-layer.

> Tasks and knowledge are both "brain docs." They share IDs. They
> share edges. They mix freely. A brain doc can be a task. A brain
> doc can be a note. A brain doc can be both at once. The `kind`
> column (task | knowledge | both) is just a tag — you filter on it
> the way you filter on a label. There is no "knowledge namespace"
> separate from a "task namespace" — there is one brain-docs space.

> The BrainVerb seam (Decision #5, divergence/0003) is narrower than
> the prior doc implied. It is the modularity guarantee for the verbs
> brain ADDS to bd (`new`, `link`, `related`, and any future
> additions) — not a routing layer over bd's existing verbs. bd's
> existing verbs (`create`, `show`, `list`, `ready`, `dep add`,
> `close`, `update`, `prime`, `bootstrap`, ...) stay exactly as they
> are; they just answer to `brain <verb>` because the binary is
> renamed. If the BrainVerb seam needs to swap, only the brain-added
> verbs feel it.

What the old framing got wrong:

- It described brain as "a verb-vocabulary lens over bd," implying
  a translation layer. There is no translation. The binary is renamed
  and Cobra dispatches subcommands by name regardless of what the
  binary is called.
- It said "bd does NOT reimplement these — use bd" for verbs like
  `ready`, `close`, `update`. That sentence contradicts the fact that
  the renamed binary exposes those verbs as `brain ready`, `brain
  close`, `brain update` directly. There is no "use bd" — there's
  `brain ready`.
- It implied `BrainVerb` was a router over both bd-existing and
  brain-added verbs. It is not. `BrainVerb` is the seam for the verbs
  brain ADDS. bd's existing verbs route directly from Cobra to
  `internal/storage` with no involvement from `BrainVerb`.
- It implied `brain new` defaulted `kind=knowledge`. ISA ISC-104/105/
  106 test the explicit form `brain new task "..."`, `brain new
  knowledge tech "..."`, `brain new both "..."`. The "default
  knowledge" claim was a fabrication that conflicted with the tested
  surface. New framing: kind is a required positional, no default.

Why this reframe is more accurate:

- Matches how bd's binary-rename mechanism actually works.
- Matches the voice patterns Trillium uses ("create a new brain
  doc...", "based on that brain doc, let's turn that into tasks...",
  "what's next re: brain"). Those patterns assume one bag of brain
  docs, not two namespaces glued together.
- Collapses a fictional architectural layer that didn't exist in the
  code. The `BrainVerb` interface (landed `5149a9e53`) is genuinely
  the seam for added verbs; it was never the seam for `bd ready`.

# What changed

**File rename:** `docs/brain/BRAIN_VS_BD.md` → `docs/brain/WHAT_IS_BRAIN.md`.

The old title implied opposition ("brain vs bd"). The new framing
denies the opposition exists. `WHAT_IS_BRAIN.md` is the question the
doc answers. Old path is removed via `git rm`; new file is added.
PAI mirror at `~/.claude/PAI/DOCUMENTATION/Brain/` follows the same
rename (old name deleted, new name created byte-identical to the
brain-repo copy). The PAI mirror is not tracked by this brain repo's
git, so it is not in `touches` (other than the brain-repo path
itself).

**Content rewrite:** the new `WHAT_IS_BRAIN.md` has 12 sections:

1. What brain is — one-binary-two-names framing with diagram.
2. The mental model: one bag of brain docs (kind is a tag, not a
   namespace; tasks and knowledge share table, ID space, edges).
3. The voice patterns — table mapping Trillium's natural phrasings
   ("create a new brain doc to...", "based on that brain doc, let's
   turn that into tasks", "what's next re: brain") to the verbs that
   resolve them.
4. The four verbs brain adds — `new`, `link`, `related`, `recast` —
   each with definition, example invocation, example output, and
   3-5 Given/When/Then scenarios.
5. Every bd verb is reachable as `brain <verb>` — table of bd's
   existing verbs (create, show, list, ready, close, update, dep add,
   prime, bootstrap, dolt push/pull, duplicates, mol wisp) all
   reachable on the renamed binary.
6. The `kind` discriminator (task | knowledge | both) — the only
   schema-shaped new idea, riding on `issues.issue_type` with no
   migration.
7. The narrowed BrainVerb seam — Decision #5 reinterpreted: seam for
   added verbs only, not a router over bd's existing verbs.
8. The plumbing diagram — bd-existing-verbs and brain-added-verbs
   both terminate at the same `internal/storage` layer; BrainVerb is
   a seam not a router; exfiltration is a HookFiringStore decorator.
9. The 18-edge-type vocabulary (16 bd + 2 brain) — unchanged from
   the prior doc, preserved as-is.
10. Cross-cutting scenarios — knowledge → task promotion preserving
    edges, closed task → knowledge insight, mid-conversation capture
    as `both`, phone-driven "what's next re: brain", misclassification
    recovery.
11. Quick reference card.
12. Where to go next (divergence trail, ISA, seam files).

**G/W/T scenario format introduced:** every primitive has 3-5
scenarios in the shape `Given <state> / When <voice phrasing + verb>
/ Then <observable outcome>`. The Given/When lines use Trillium's
actual voice phrasings wherever they fit, so the doc reads back as
recognizable speech-to-shell mappings rather than abstract API specs.

**New verb defined: `brain recast <id> --to=<kind>`.** This is the
verb behind "let's turn that brain doc into a task." A knowledge doc
becomes a task by changing one column. ID, edges, body, comments
preserved. The exfiltrator relocates the markdown file on the next
sync. Naming chosen over `brain promote` because `bd promote` already
ships for wisp → bead graduation; the namespace collision was real
and resolved by picking a different word. Doc includes the rejected
alternative and the hint message brain prints if a user types `brain
promote` out of habit.

**Decision recorded: `brain new` requires explicit kind.** Three
alternatives considered (default knowledge, smart-detect from flags,
require explicit) — chose require explicit. Matches ISA ISC-104/
105/106. Matches Trillium's natural voice ("brain new task ...",
"brain new knowledge ..."). Removes the magic-default surface area.

**`docs/brain/README.md` update:** the "What lives here" entry for
`BRAIN_VS_BD.md` is replaced with an entry for `WHAT_IS_BRAIN.md`
that summarizes the new framing in one paragraph and points back to
this divergence doc for the rename rationale.

**No ISA changes.** This is a docs-only reframe. ISA ISC criteria
remain authoritative. If anything in this doc drifts from ISA in
future tranches, ISA wins and the doc gets corrected.

**No code changes.** `internal/brain/verb/verb.go` (the seam),
`cmd/bd/brain.go` (the parent command), and the `extends` /
`learned-from` edge-type registrations are untouched. The verb
implementations are still in scope for the next tranche.

# Brain-spec link

This doc reflects and refines, but does not advance, the following
ISA elements:

- `## Vision` — the one-list/one-search-box phone view at line 28
  ("I open Pulse on my phone. I see one list..."). The reframe makes
  the "one list" reading natural: it's not two namespaces joined for
  the phone view, it's one bag of brain docs with a kind tag.
- `## Criteria` → ISC-104/105/106 (`brain new task/knowledge/both`)
  — preserved exactly. The new doc commits to the explicit-kind form
  ISA already tests.
- `## Criteria` → ISC-107/108 (`brain show`, `brain list --kind`) —
  preserved.
- `## Criteria` → ISC-109 (`brain link`) — preserved; 4 G/W/T
  scenarios added.
- `## Criteria` → ISC-111 (`brain related`) — preserved; 5 G/W/T
  scenarios added including the cycle case.
- `## Criteria` → ISC-114 (`brain ready` kind-filtered) — preserved;
  reframed as "this is bd's `ready` on the renamed binary, filtered
  by kind."
- `## Criteria` → ISC-117/118 (exfiltration markdown files) —
  preserved; referenced in scenarios where new docs trigger markdown
  writes.
- `## Decisions` → Decision #5 (modularity-first / BrainVerb seam) —
  narrowed interpretation explicitly documented. The seam covers the
  verbs brain ADDS, not the verbs brain inherits via binary rename.
  Decision #5 itself is not modified; this is exposition of its
  scope.
- `## Capability Audit` — the lines confirming bd already ships
  `wisp`, `promote`, `mol wisp`, the kind-discriminator-is-free
  insight, the read-verbs-are-renames insight — those bullets are
  the architectural foundation the reframe stands on.

If a future tranche actually changes the verb signatures, ISA gets
updated first and this doc follows. ISA wins on drift.

# Cross-system mirror

A byte-identical copy of `docs/brain/WHAT_IS_BRAIN.md` also lives at
`~/.claude/PAI/DOCUMENTATION/Brain/WHAT_IS_BRAIN.md` (same PAI
subsystem dir established in divergence/0005). The old PAI mirror at
`~/.claude/PAI/DOCUMENTATION/Brain/BRAIN_VS_BD.md` is removed as part
of this reframe so the PAI tree stays clean. The PAI mirror is not
tracked in this brain repo; it is part of the PAI tree (not under
brain's git). Maintenance rule: when the brain-repo copy changes,
copy the new content to the PAI mirror. The brain-repo copy is
authoritative.

If divergence between the two ever becomes load-bearing, replace the
PAI mirror with a pointer file that says "see brain repo
docs/brain/WHAT_IS_BRAIN.md" and accept the cross-tree dereference
cost.
