---
id: 0009
title: brain related verb implementation
isc: [111]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: [67e633594]
touches:
  - internal/brain/verb/related/related.go
  - internal/brain/verb/related/related_test.go
  - cmd/bd/brain_related.go
upstream_rebase_notes: |
  No upstream-touched files. The three paths in this tranche
  (`internal/brain/verb/related/related.go`,
  `internal/brain/verb/related/related_test.go`,
  `cmd/bd/brain_related.go`) are brain-only additions; they do not
  exist upstream. On any rebase that touches them, resolve with `ours`.

  brain related is read-only over the existing `dependencies` table.
  It calls two methods on `storage.Storage`: `GetIssue` (existence
  probe + node enrichment) and `GetDependenciesWithMetadata` (BFS
  expansion). If a future bd-upstream rebase renames or restructures
  either method, brain related needs re-wiring through the narrow
  `RelatedStore` seam — but the verb's two-method interface keeps
  the surface area to re-wire small.

  Unlike `brain link`, this verb writes nothing — no `AddDependency`
  path, no `commandDidWrite` mark, no `actor` parameter — so the
  rebase risk from any write-path changes upstream is nil.
---

# Why

`brain related <id> [--depth=N]` is the third of the four brain-added
verbs named in `docs/brain/WHAT_IS_BRAIN.md` § 4 (`new`, `link`,
`related`, `recast`). It plugs into the same `BrainVerb` seam
established in `divergence/0003-modularity-first.md` and
`divergence/0004-brain-verb-seam-and-parent.md`, under the parent
`brainCmd` already wired by 0004 and first exercised by
`divergence/0007-brain-new.md` and `divergence/0008-brain-link.md`.

What makes this tranche different from 0007 and 0008: **`brain
related` is the only brain-added verb that has no bd analogue.**
`bd dep list <id>` prints a flat one-hop table. `brain related` does
depth-bounded BFS over the same `dependencies` table with cycle
detection, deterministic ordering, and a tree-shaped result. The
reframe in `divergence/0006-brain-primitives-reframe.md` still
applies — there is no new schema, no new table, no migration; the
new behaviour lives entirely in the verb engine on top of bd's
existing storage primitives — but the verb itself is genuinely new
work rather than a vocabulary alias over a bd verb.

The decisions this tranche bakes in:

- **Outgoing edges only (not bidirectional).** The verb follows the
  `dependencies.issue_id` → `dependencies.depends_on_id` direction
  only — the same direction `bd dep list` prints by default and the
  same direction `brain link a b --learned-from` creates. The
  rendered sample tree in `WHAT_IS_BRAIN.md` § 4.3 reads as
  outgoing, and outgoing keeps the subgraph small enough to fit on
  a phone screen, which is the explicit "phone-driven exploration"
  scenario. Bidirectional traversal would explode at common hub
  nodes; a future `--bidirectional` flag is conceivable but out of
  scope. Documented inline in the verb's package comment so the
  decision is discoverable from the source, not buried in a
  divergence file.

- **Cycle detection by visited-set, not by depth cap alone.** A
  pure depth cap would let a cycle re-emit the same nodes at every
  remaining level (`A → B → A → B → A` at depth=4). The visited-set
  guarantees each node appears once in the tree; the second
  appearance of a node is recorded with `AlreadyVisited=true` and
  the BFS does NOT recurse through it. The Cobra wrapper renders
  this as `(already visited)` after the node line; `--json` emits
  `"already_visited": true` with an empty `"children"` array. Matches
  `WHAT_IS_BRAIN.md` § 4.3 scenario "cycle in the graph".

- **Deterministic child ordering: edge type (alpha) then neighbour
  ID.** Storage does not guarantee a row order on
  `GetDependenciesWithMetadata`; without an explicit sort the
  rendered tree would jitter between runs. Sorting by (edge type,
  ID) is the contract both the rendered tree and the `--json`
  output depend on. `TestRun_DeterministicOrdering` runs three
  consecutive Run calls against the same seed and asserts identical
  child order each time.

- **Verb returns a tree; wrapper renders.** The verb's job is the
  TRAVERSAL — what's reachable, with what edges, in what order. The
  wrapper's job is the RENDERING — indented box-drawing for stdout,
  recursive marshal for `--json`. This is a deliberate split. It is
  a small exception to "no business logic in the wrapper" (rendering
  is presentation, not business) and lets the verb stay pure-Go
  testable while the wrapper owns the visual contract. Both sides
  see the same `Node` struct as the API surface.

- **No actor; no `commandDidWrite` mark; no `CheckReadonly`.** The
  verb is read-only over `dependencies`. There is no audit trail to
  attribute (so `New` takes only `store`, not `(store, actor)`),
  no Dolt commit to flush (so the wrapper does not set
  `commandDidWrite`), and no readonly mode to enforce (so the
  wrapper does not call `CheckReadonly`). This is the only
  structural difference from `cmd/bd/brain_new.go` and
  `cmd/bd/brain_link.go`.

This advances ISA `ISC-111` (`brain related <id>` reads edges and
returns connected nodes ordered by edge type). The eight scenarios
in `WHAT_IS_BRAIN.md` § 4.3 map 1:1 to test functions in
`related_test.go`: happy path depth=2 (`TestRun_HappyPath_Depth2`),
depth=0 (`TestRun_Depth0`), depth=1 (`TestRun_Depth1`), orphan
(`TestRun_Orphan`), cycle (`TestRun_Cycle`), nonexistent center
(`TestRun_NonexistentCenter`), deterministic ordering
(`TestRun_DeterministicOrdering`), and mixed kinds
(`TestRun_MixedKinds`). Eight additional plumbing tests cover
empty-ID guard, negative-depth guard, nil-store guard, both
storage-error wrappings, the `Name()` seam pin, and the
`DefaultDepth=2` constant. 16 tests total, all parallel.

# What changed

**`internal/brain/verb/related/related.go`** — the verb engine.
Exports `Node` (the recursive tree type the wrapper renders and
`--json` marshals), `Args{ID, Depth}`, `Result{Center *Node}`, a
narrow `RelatedStore` interface (`GetIssue` and
`GetDependenciesWithMetadata`), the `Verb` struct that implements
`verb.BrainVerb[Args, Result]`, a `New(store)` constructor, and the
`DefaultDepth` constant (`2`). `Run` validates required fields
(ID non-empty, Depth non-negative, store wired), probes center
existence with the brain-spec wording on `storage.ErrNotFound`, then
performs a FIFO BFS bounded by `args.Depth` with a visited-set for
cycle pruning. Children are sorted by (edge type, neighbour ID)
before they are appended to the parent's `Children` slice. `Run`
never writes to stdout/stderr — the contract from
`internal/brain/verb/verb.go` forbids it.

The `RelatedStore` interface is deliberately narrow (two methods)
rather than depending on the full `storage.Storage` interface
(~60 methods). Production wires the global `store` (a
`storage.DoltStorage`) through the embedded `Storage` interface
that satisfies both methods; the test suite wires a hand-rolled
2-method recorder. Mirrors `newverb.IssueCreator` and
`link.LinkStore` — three verbs in, the pattern is now the
modularity-first principle from Decision #5 in practice.

**`internal/brain/verb/related/related_test.go`** — 16 tests
covering the eight spec scenarios (`TestRun_HappyPath_Depth2`,
`TestRun_Depth0`, `TestRun_Depth1`, `TestRun_Orphan`, `TestRun_Cycle`,
`TestRun_NonexistentCenter`, `TestRun_DeterministicOrdering`,
`TestRun_MixedKinds`), plus eight plumbing guards (empty ID,
negative depth, nil store, transport-error wrapping for both
`GetIssue` and `GetDependenciesWithMetadata`, the `Name()` seam
pin, and the `DefaultDepth` constant). All parallel
(`t.Parallel()`). Uses a hand-rolled `recorderStore` fake that
satisfies `RelatedStore`; compile-time
`var _ related.RelatedStore = (*recorderStore)(nil)` assertion
catches seam drift at build time. The deterministic-ordering test
runs three consecutive `Run` calls and asserts byte-identical
child order each time.

**`cmd/bd/brain_related.go`** — the Cobra wrapper. Registers
`brainRelatedCmd` under `brainCmd` (the parent established in
`divergence/0004`). Parses one positional arg (`<id>`) and one flag
(`--depth`, default `relatedverb.DefaultDepth = 2`). Invokes the
verb, then either marshals the result via `outputJSON` or renders
the indented box-drawing tree to stdout. The renderer is local to
this file — three small functions (`renderTree`, `renderSubtree`,
`styleID`, `formatKindTag`) — and matches the sample tree from
`WHAT_IS_BRAIN.md` § 4.3: center on its own line with kind tag,
blank `│` separator, children with `├─[edge]→ id · title [kind=...]`
or `└─[edge]→ ...` for the last child, recursive descent with the
indentation prefix carrying the box-drawing vertical bars from
ancestor branches, and `(already visited)` annotation on cycle leaves.

Does NOT call `CheckReadonly`. Does NOT set `commandDidWrite`. Does
NOT call `SetLastTouchedID`. The verb is read-only.

This wrapper file cannot be compiled or tested locally on the
current dev machine because `cmd/bd` depends transitively on Dolt's
`go-icu-regex` cgo binding, and the installed `icu4c@78` is missing
the `uregex_*` arm64 symbols (`~/.claude/PAI/USER/FRICTION.md`,
entry dated 2026-05-31). Verification is by pattern-match against
`cmd/bd/brain_link.go` (the brain-verb wrapper template) and
`gofmt -d` clean. The verb subpackage at
`internal/brain/verb/related/` is pure Go and tests run green:
`go test ./internal/brain/verb/related/...` → `ok` (16/16).

**`divergence/0009-brain-related.md`** — this file. Lands the
tranche; `commits:` field gets filled in by a follow-up commit per
the 0002 / 0004 / 0005 / 0006 / 0007 / 0008 SHA-record pattern.

# Brain-spec link

This tranche advances:

- **ISA `## Decisions` → Decision #5 (modularity-first
  architecture)** — third concrete use of the `BrainVerb` seam from
  `internal/brain/verb/verb.go`. The seam now has three verbs riding
  on it (`new`, `link`, `related`); the Cobra wrapper at
  `cmd/bd/brain_related.go` is structurally close to
  `cmd/bd/brain_link.go` (same dependency wiring, same output
  formatting pattern, same FatalError rendering) modulo the read-only
  differences (no `CheckReadonly`, no `commandDidWrite`, no actor).
  The seam composes: each new verb is a copy-paste-edit operation
  with the read/write polarity decided per verb. That is exactly
  what Decision #5 promised.

- **ISA `## Features` → cli-aliases row** — `brain related` is the
  third verb added on top of bd's existing CLI. `brain recast`
  (verb #4, the last one) attaches the same way against the same
  parent command (`brainCmd`) and the same seam.

- **ISA `## Capabilities` → graph traversal** — `brain related`
  fills the "what's connected to X" voice pattern from
  `WHAT_IS_BRAIN.md` § 3 ("show me everything connected to that
  thing I learned about Tailscale"). Walks edges across all kinds
  (task + knowledge + both) without filtering, because there is
  one bag of brain docs.

- **ISA `ISC-111`** — `brain related <id> --json` orders results by
  edge type. Covered by `TestRun_DeterministicOrdering`
  (asserts sort by edge type alpha, then neighbour ID, byte-stable
  across runs) and by every other happy-path test that exercises
  the sorted-children invariant. Real end-to-end verification
  against Dolt is gated on the libicu fix noted in
  `upstream_rebase_notes`.

# Cross-system mirror

The brain v0.3 code in this tranche lives only in the brain repo.
There is no PAI mirror because there is no markdown surface to
mirror — `internal/brain/verb/related/*` and
`cmd/bd/brain_related.go` are Go source, not documentation. The
divergence file itself lives only under `brain/divergence/`,
matching the convention established in 0001 through 0008:
divergence files are the brain repo's historical anchor for changes
that diverge from upstream bd, and they do not get copied into the
PAI tree.

The PAI mirror established in 0005 / 0006 is for `docs/brain/*`
only. When a future tranche updates `WHAT_IS_BRAIN.md` to mark
`brain related` as "shipped" rather than "planned," that doc
update will also land in
`~/.claude/PAI/DOCUMENTATION/Brain/WHAT_IS_BRAIN.md` per the
existing maintenance rule.
