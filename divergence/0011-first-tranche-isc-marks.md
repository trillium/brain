---
id: 0011
title: first-tranche CLI verbs — ISC status update
isc: [104, 105, 106, 109, 111, 151, 152]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: []
touches:
  - ISA.md
upstream_rebase_notes: |
  ISA.md is brain-only. Upstream bd has no ISA. On any bd → brain
  rebase, resolve `ISA.md` with `ours` — there is no upstream
  counterpart and this file should never inherit upstream conflicts.
---

# Why

Four new brain verbs landed in divergence/0007-0010 (`brain new`, `brain link`,
`brain related`, `brain recast`). The ISA's ISC checkbox list was still
`[ ]` for all of them. This entry marks them green and resolves three
post-reframe spec drifts the original ISA didn't anticipate.

The reframe in divergence/0006 (WHAT_IS_BRAIN.md) was the load-bearing
shift — "brain IS bd renamed, kind is a tag" — and it tightened a few
verb signatures that the original ISA had drafted before the reframe
existed. This is the cleanup commit that reconciles ISA wording with
what actually shipped.

# What changed

Two kinds of ISA edits, both in `ISA.md` only.

**Greens (5 ISCs flipped `[ ]` → `[x]`):**

- **ISC-104** — `brain new task "..."` — verbatim match with shipped verb. Landed divergence/0007.
- **ISC-105** — `brain new knowledge "..."` — **amended wording**. The original ISC text was `brain new knowledge tech "what I learned"` with `tech` as a positional category arg. The reframe (divergence/0006, WHAT_IS_BRAIN.md § 4.1) reset the signature to `<kind> <title>` only — explicit kind, no category. The verb that shipped writes kind=knowledge correctly, satisfying the load-bearing assertion. The `tech` positional arg was removed from the ISC example to match. Category-as-flag is a future addition, not in this tranche's scope.
- **ISC-106** — `brain new both "..."` — verbatim match. Landed divergence/0007.
- **ISC-109** — `brain link <a> <b> --type=relates-to` — match via fallthrough `--type` path. The verb also exposes `--extends`, `--learned-from`, `--related` as first-class flags (per WHAT_IS_BRAIN.md § 4.2). Forge's `TestRun_HappyPath_FallthroughType` covers the ISC's exact assertion. Landed divergence/0008.
- **ISC-111** — `brain related <id>` — match. The shipped verb does BFS with depth cap (default 2), cycle detection via visited set, deterministic sibling ordering (edge type alphabetical, then neighbor ID). The ISC's "ordered by edge type" requirement is satisfied by the deterministic ordering. Landed divergence/0009.

**New reservations (2 ISCs added `[x]`):**

The reframe added two CLI surfaces that didn't exist in the original ISC plan. Reserved in the gap after the documented ranges (ISC-100-150 per the 2026-05-31 changelog entry; ISC-251 is the jot range):

- **ISC-151** — `brain recast <id> --to=<kind>` — in-place kind shift. Same ID, same edges, same comments, same body. Status defaulting rules: open is defaulted on knowledge→task / knowledge→both transitions when the current status is not closed; preserved on all other transitions. Idempotent (no-op when current kind already equals target). Landed divergence/0010.
- **ISC-152** — `brain promote <args>` redirect hint. UX-only Cobra subcommand at `cmd/bd/brain_promote.go`. Prints the namespace-collision hint pointing at `brain recast` and `bd promote`, exits non-zero. Lives outside the BrainVerb seam because it has no business logic — pure UX redirect. Landed divergence/0010.

# Brain-spec link

This entry advances the following ISA elements:

- `## Vision` — the verb vocabulary lens (now ratified by working code).
- `## Decisions` → Decision #5 (modularity-first) — every shipped verb implements `BrainVerb[Args, Result]` with a self-contained subpackage; the seam is no longer hypothetical.
- `## Criteria` → ISC-104, 105, 106, 109, 111, 151, 152 — see the green flips above.
- `## Features` → `cli-aliases` row — the first tranche of the surface is now shipped.
- `## Capabilities` → kind discriminator (writes via `IssueType` extension in `internal/types/types.go`, divergence/0007), BrainVerb seam (divergence/0004), edge-type registration (`extends`, `learned-from` already in bd schema via prior commits `259d17a82` + `c4b6a78e4`).

The five ISCs that remain `[ ]` in the CLI-core range:

- **ISC-107** (`brain show`) — not yet shipped. Likely a Cobra alias over bd's existing `bd show` since brain IS bd renamed. Future tranche.
- **ISC-108** (`brain list --kind=<k>`) — kind-filter wrapper over bd's `bd list --type=<t>`. Future tranche.
- **ISC-110** (`brain search`) — needs the FTS5 cache (`internal/storage/fts/` package, ~300-500 LOC per the Capability Audit). Separate larger tranche.

The ISC-251 jot alias namespace also remains `[ ]`; per Decision #2 it depends on the first-tranche CLI aliases landing first, which they now have. Unblocked for future work.

# Cross-system mirror

None for this entry. ISA.md is brain-only and stays in the brain repo.

# Decisions documented (sensible defaults for v0.3, per Trillium's "you pick" directive)

Recorded here so future-Trillium can see what was chosen on his behalf:

1. **Drop the category positional from `brain new`.** The reframe's two-arg signature wins over the original ISA's three-arg example. Rationale: the voice patterns ("brain new task ...", "brain new knowledge ...") never spoke a category; the verb shouldn't require one. A category flag can be added later without breaking the positional shape.
2. **Reserve ISC-151/152 in the gap range rather than re-numbering.** Original ISC ranges are load-bearing for divergence-trail readers; renumbering would invalidate cross-references. The 150-200 gap is unused per the changelog.
3. **`brain promote` is a hint subcommand, not a verb.** It's UX-only, no storage path, no BrainVerb implementation. Cleaner than burying the redirect inside `recast`'s help text or a hook.
4. **Test markers point at the divergence entry where the green landed.** Each `[x]` line carries `landed divergence/XXXX` so readers can trace the code from the ISA without grepping commits.
