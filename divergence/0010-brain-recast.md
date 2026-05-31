---
id: 0010
title: brain recast verb implementation
isc: []
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: []
touches:
  - internal/brain/verb/recast/recast.go
  - internal/brain/verb/recast/recast_test.go
  - cmd/bd/brain_recast.go
  - cmd/bd/brain_promote.go
upstream_rebase_notes: |
  No upstream-touched files; brain-only additions. The four paths in
  this tranche
  (`internal/brain/verb/recast/recast.go`,
  `internal/brain/verb/recast/recast_test.go`,
  `cmd/bd/brain_recast.go`,
  `cmd/bd/brain_promote.go`) are brain-only additions; they do not
  exist upstream. On any rebase that touches them, resolve with `ours`.

  brain recast mutates the `issues.issue_type` column (and optionally
  `issues.status`) through bd's existing `storage.UpdateIssue` write
  path — the same path `bd update --type=<kind>` uses. There is no
  schema change, no new table, no new index. If a future bd-upstream
  rebase renames `UpdateIssue` or restructures the `updates
  map[string]interface{}` shape (currently `"issue_type"` and
  `"status"` are the keys recast writes), this verb needs re-wiring
  through the narrow `RecastStore` seam — but the three-method
  interface keeps the surface area to re-wire small.

  The `brain promote` subcommand (`cmd/bd/brain_promote.go`) is pure
  UX with no upstream counterpart. bd's `promote` lives at the root
  command (`bd promote <wisp-id>` for wisp → bead graduation); the
  `brain promote` subcommand attaches under `brainCmd` and is reached
  ONLY via `brain promote ...`, never via `bd promote ...`. There is
  no shadowing risk.
---

# Why

`brain recast <id> --to=<kind>` is the fourth and final of the brain-
added verbs named in `docs/brain/WHAT_IS_BRAIN.md` § 4 (`new`, `link`,
`related`, `recast`). It plugs into the same `BrainVerb` seam
established in `divergence/0003-modularity-first.md` and
`divergence/0004-brain-verb-seam-and-parent.md`, under the parent
`brainCmd` already wired by 0004 and first exercised by
`divergence/0007-brain-new.md`, `divergence/0008-brain-link.md`, and
`divergence/0009-brain-related.md`.

What makes this tranche different from the prior three: **recast is
the only brain verb that MUTATES an existing brain doc.** `new`
creates a row. `link` writes an edge. `related` reads the graph.
Recast changes the kind discriminator on an existing row in place,
preserving every edge, every comment, the body, the title, and the
ID. It is the verb behind the voice pattern "let's turn that brain
doc into a task" — a common ask once a knowledge doc reveals a
concrete work item.

The decisions this tranche bakes in:

- **Name: `recast`, not `promote`.** bd already ships `bd promote`
  (and `bd mol wisp promote`) for wisp → bead graduation — a
  different, narrower operation. To stay out of that namespace
  collision, brain's kind-shift verb is `brain recast`. The word also
  matches the operation precisely: we are recasting the doc into a
  different role. `brain promote` was the natural-language
  candidate, but namespacing matters more than reading-naturalness
  for a verb that appears hundreds of times in shell history. To
  catch users reaching for `brain promote` out of habit, a separate
  `brain promote` redirector subcommand prints a two-line hint to
  stderr and exits non-zero. The redirector has no business logic,
  no storage, no actor — pure UX.

- **Status-defaulting rule: only knowledge sources trigger it.** The
  table is: `knowledge → task / both` defaults status to `open`
  unless the row is already `closed` (then preserved); every other
  transition (`task → *`, `both → *`) preserves status verbatim.
  The rationale is that a knowledge doc has no meaningful task-
  workflow status before the recast — picking `open` is the
  sensible default, but a deliberate closure deserves to be
  honoured. The `task → knowledge` direction is the spec's
  "misclassification recovery" scenario, where the spec says "the
  status column is preserved but no longer participates in
  `brain ready` (kind=knowledge is excluded)" — the status field
  stays, it just becomes inert for ready-queue purposes.

- **No-op semantics: kind equality is the only test.** If the
  current `issue_type` already equals `--to=<kind>`, the verb
  short-circuits with `NoOp=true`, writes NOTHING to storage, and
  reports the no-op cleanly. The Cobra wrapper does NOT set
  `commandDidWrite` on the no-op path, so no Dolt commit cycle
  fires. The verb still enumerates outgoing edges so the JSON output
  has a complete neighbourhood picture for downstream tooling that
  diffs recast JSON before/after — the read is cheap and the
  symmetry helps.

- **Edges and comments preserved by NOT touching them.** All
  edge-preservation is automatic: the verb never reads from or
  writes to the `dependencies` table for mutation purposes. By
  leaving dependencies untouched, every `(issue_id, depends_on_id,
  type)` row pointing at or away from `<id>` survives. The same
  applies to `comments`. The verb DOES enumerate outgoing edges
  via `GetDependenciesWithMetadata` so the Result's
  `EdgesPreserved` list can confirm the count to the user; that
  read happens BEFORE the `UpdateIssue` call so a transport
  failure on the read does not leave the row mutated without a
  corresponding count.

- **Markdown relocation is OUT OF SCOPE.** The spec mentions an
  `entries/knowledge/<slug>.md` file should relocate to
  `entries/task/<slug>.md` (or vice versa) on a kind change. That
  is the exfiltrator's job on its next idempotent sync; the verb
  does NOT touch the markdown surface. The verb's contract is
  row-only: change `issue_type` (and possibly `status`) on the
  Dolt row; the next exfiltrator run rebuilds the markdown tree
  from the updated rows. Documented in the package comment so the
  next reader does not look for missing file-move code.

- **No ISC match: this tranche advances no existing ISC.** The
  recast verb has no Given/When/Then scenario in `ISA.md` — the
  closest ISCs (`ISC-104..111`) cover `new`, `show`, `list`,
  `link`, `search`, and `related` but not kind-shift. The ISA
  needs a follow-up edit to add ISCs for the four spec scenarios
  in WHAT_IS_BRAIN.md § 4.4: knowledge→task, task→knowledge,
  knowledge→both, idempotent recast, invalid target. Tracking
  that as the next step on this tranche; ISC frontmatter is `[]`
  rather than fabricating ISC numbers.

# What changed

**`internal/brain/verb/recast/recast.go`** — the verb engine.
Exports `Args{ID, ToKind}`, `Result{ID, OldKind, NewKind, OldStatus,
NewStatus, EdgesPreserved, NoOp}`, a narrow `RecastStore` interface
(`GetIssue`, `GetDependenciesWithMetadata`, `UpdateIssue`), the
`Verb` struct that implements `verb.BrainVerb[Args, Result]`, and a
`New(store, actor)` constructor. `Run` validates required fields
(ID non-empty, ToKind non-empty, ToKind ∈ {task,knowledge,both},
store wired), probes existence with the brain-spec wording on
`storage.ErrNotFound`, then either short-circuits to no-op (current
kind already equals target) or computes the new status per the
kind-transition table and calls `UpdateIssue` with `issue_type`
and (when status actually changes) `status`. Edges are enumerated
via `GetDependenciesWithMetadata`, sorted by neighbour ID
alphabetically, and returned as `EdgesPreserved` — always a
non-nil slice so `--json` never emits `null` for an empty
neighbourhood. `Run` never writes to stdout/stderr — the contract
from `internal/brain/verb/verb.go` forbids it.

The `RecastStore` interface is deliberately narrow (three methods)
rather than depending on the full `storage.Storage` interface
(~60 methods). Production wires the global `store` (a
`storage.DoltStorage`) through the embedded `Storage` interface
that satisfies all three methods; the test suite wires a
hand-rolled 3-method recorder. Mirrors `newverb.IssueCreator`,
`link.LinkStore`, and `related.RelatedStore` — four verbs in, the
pattern is the modularity-first principle from Decision #5 in
practice.

**`internal/brain/verb/recast/recast_test.go`** — 18 tests covering
the spec scenarios plus plumbing guards. The mapping is:
`TestRun_KnowledgeToTask_DefaultsOpen` (scenario 1),
`TestRun_TaskToKnowledge_PreservesStatusOpen` +
`TestRun_TaskToKnowledge_PreservesStatusClosed` (scenario 2, both
status polarities), `TestRun_KnowledgeToBoth_DefaultsOpen`
(scenario 3), `TestRun_TaskToBoth_PreservesStatus` (scenario 4),
`TestRun_BothToTask_PreservesStatus` (scenario 5),
`TestRun_BothToKnowledge_PreservesStatus` (scenario 6),
`TestRun_KnowledgeClosedToTask_PreservesClosed` (scenario 7 — the
defaulting rule's exception), `TestRun_NoOp_KindAlreadyMatches`
(scenario 8 — verifies zero `UpdateIssue` calls via the recorder),
`TestRun_InvalidTargetKind` (scenario 9 — error names "invalid
target kind" and all three valid values), `TestRun_NonexistentID`
(scenario 10), `TestRun_EdgesPreserved_DeterministicOrder`
(scenario 11 — seeds reverse-alphabetical, asserts sorted output),
`TestRun_EdgesPreserved_EmptyNotNil` (scenario 12 — empty slice
not nil for the JSON contract). Plumbing guards:
`TestRun_EmptyID`, `TestRun_EmptyToKind`,
`TestRun_NilStoreReturnsError`,
`TestRun_GetIssueTransportErrorIsWrapped`,
`TestRun_GetEdgesErrorIsWrapped`,
`TestRun_UpdateIssueErrorIsWrapped`, and `TestVerbName`. All
parallel (`t.Parallel()`). Uses a hand-rolled `recorderStore` fake
that satisfies `RecastStore`; compile-time
`var _ recast.RecastStore = (*recorderStore)(nil)` assertion
catches seam drift at build time. Test result:
`go test ./internal/brain/verb/recast/...` → `ok` (18/18).

**`cmd/bd/brain_recast.go`** — the Cobra wrapper. Registers
`brainRecastCmd` under `brainCmd` (the parent established in
`divergence/0004`). Parses one positional arg (`<id>`) and one
required flag (`--to`). Invokes the verb, then either marshals the
result via `outputJSON` or renders the three-part human output
(recast line, optional status line, edges line). Calls
`CheckReadonly` (recast is a writer), sets `commandDidWrite` and
`SetLastTouchedID` ONLY on the mutating path (the no-op path
performs zero writes; flagging it as a write would force a needless
Dolt commit cycle). Matches the brain_link.go wrapper template
exactly modulo the recast-specific output shape and the no-op
branch.

**`cmd/bd/brain_promote.go`** — the namespace-collision-avoidance
hint. Registered as a subcommand on `brainCmd` (not under the
recast package). Accepts arbitrary args (`cobra.ArbitraryArgs`) so
any spelling of `brain promote ...` still hits the hint instead of
"unknown command". Prints the two-line hint to stderr and returns
a non-nil error so the exit code reflects the redirection (this is
a usage error, not a successful no-op). No business logic, no
storage, no actor.

These wrapper files cannot be compiled or tested locally on the
current dev machine because `cmd/bd` depends transitively on Dolt's
`go-icu-regex` cgo binding, and the installed `icu4c@78` is missing
the `uregex_*` arm64 symbols (`~/.claude/PAI/USER/FRICTION.md`,
entry dated 2026-05-31). Verification is by pattern-match against
`cmd/bd/brain_link.go` (the writer-verb wrapper template) and
`gofmt -d` clean. The verb subpackage at
`internal/brain/verb/recast/` is pure Go and tests run green:
`go test ./internal/brain/verb/recast/...` → `ok` (18/18).

**`divergence/0010-brain-recast.md`** — this file. Lands the
tranche; `commits:` field gets filled in by a follow-up commit per
the 0002 / 0004 / 0005 / 0006 / 0007 / 0008 / 0009 SHA-record
pattern.

# Brain-spec link

This tranche advances:

- **ISA `## Decisions` → Decision #5 (modularity-first
  architecture)** — fourth concrete use of the `BrainVerb` seam from
  `internal/brain/verb/verb.go`. The seam now has four verbs riding
  on it (`new`, `link`, `related`, `recast`); the Cobra wrapper at
  `cmd/bd/brain_recast.go` is structurally close to
  `cmd/bd/brain_link.go` (same dependency wiring, same `CheckReadonly`
  + `commandDidWrite` + `SetLastTouchedID` discipline, same
  `FatalErrorRespectJSON` rendering) modulo the recast-specific
  no-op branch and three-line output format. The seam composes
  cleanly across read and write polarities — exactly what
  Decision #5 promised.

- **ISA `## Features` → cli-aliases row** — `brain recast` completes
  the four-verb brain surface (`new`, `link`, `related`, `recast`).
  All four attach against the same parent command (`brainCmd`) and
  the same seam. The `brain promote` hint is the first UX-only
  brain subcommand; it sets a precedent for catching natural-
  language verb misses without bloating the verb registry.

- **ISA `## Capabilities` → kind discriminator** — `brain recast`
  is the verb that lets the kind discriminator (task | knowledge |
  both) be a live decision rather than a permanent one. Without
  recast, a misclassified doc would have to be deleted and
  recreated — losing its ID, edges, and audit trail. With recast,
  kind becomes a property of the doc that can evolve as Trillium's
  understanding evolves.

- **ISA `ISC-104..111`** — none of the existing ISCs cover recast
  semantics. Follow-up: add ISCs for the four scenarios in
  WHAT_IS_BRAIN.md § 4.4 (knowledge→task with status default,
  task→knowledge with status preservation, idempotent recast,
  invalid target kind). Tracking as the next step on this
  tranche.

# Cross-system mirror

The brain v0.3 code in this tranche lives only in the brain repo.
There is no PAI mirror because there is no markdown surface to
mirror — `internal/brain/verb/recast/*`, `cmd/bd/brain_recast.go`,
and `cmd/bd/brain_promote.go` are Go source, not documentation. The
divergence file itself lives only under `brain/divergence/`,
matching the convention established in 0001 through 0009:
divergence files are the brain repo's historical anchor for changes
that diverge from upstream bd, and they do not get copied into the
PAI tree.

The PAI mirror established in 0005 / 0006 is for `docs/brain/*`
only. When a future tranche updates `WHAT_IS_BRAIN.md` to mark
`brain recast` as "shipped" rather than "planned," that doc update
will also land in
`~/.claude/PAI/DOCUMENTATION/Brain/WHAT_IS_BRAIN.md` per the
existing maintenance rule.
