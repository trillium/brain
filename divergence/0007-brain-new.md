---
id: 0007
title: brain new verb implementation
isc: [104, 105, 106]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: [7467d98b9350f2efc7c90da427799e839c833030]
touches:
  - internal/brain/verb/new/new.go
  - internal/brain/verb/new/new_test.go
  - cmd/bd/brain_new.go
  - internal/types/types.go
upstream_rebase_notes: |
  Touches `internal/types/types.go` to accept `knowledge` and `both`
  as valid `IssueType` values alongside bd's existing enum. On bd → brain
  rebase that touches `types.go`, the `TypeKnowledge` and `TypeBoth`
  constants and their branches in `IsValid()` must survive — they are
  the load-bearing relaxation that lets brain's verbs write through bd's
  existing storage interface with no schema migration. Resolve with
  `ours` for the brain-only additions, accept upstream changes for any
  other lines in types.go.

  The other three paths (`internal/brain/verb/new/*` and
  `cmd/bd/brain_new.go`) are brain-only. They do not exist upstream.
  On any rebase that touches them, resolve with `ours`.
---

# Why

Trillium's voice patterns (documented in `docs/brain/WHAT_IS_BRAIN.md`
§ 3) routinely begin with "create a new brain doc to ..." Until this
tranche, the only way to land that sentence was `bd create --type=task`
or `bd create --type=knowledge` — verbs whose vocabulary belongs to bd,
not to the brain framing.

`brain new <kind> <title>` is the first of the four brain-added verbs
named in `WHAT_IS_BRAIN.md` § 4 (`new`, `link`, `related`, `recast`).
It plugs into the `BrainVerb` seam established in
`divergence/0003-modularity-first.md` and `divergence/0004-brain-verb-seam-and-parent.md`,
under the parent `brainCmd` already wired by 0004.

The reframe in `divergence/0006-brain-primitives-reframe.md` set the
ground rules this tranche obeys:

- **brain IS bd renamed.** This verb writes through bd's existing
  storage interface; no separate brain schema, no separate brain table.
- **`kind` is a tag on a column bd already had.** The `kind` discriminator
  rides on `issues.issue_type` — already TEXT, already validated by
  `IssueType.IsValid()`, already round-trippable through bd's
  serialization. The change to `internal/types/types.go` is the
  minimum bd-side widening to accept `knowledge` and `both` as legal
  values; nothing else moves.
- **No magic defaults.** `kind` is a required positional argument.
  Empty kind, invalid kind, and empty title all fail before any
  storage write, with error messages that name the three accepted
  values so users recover without reading code.

This advances ISA `ISC-104` (`brain new task ...`), `ISC-105`
(`brain new knowledge ...`), and `ISC-106` (`brain new both ...`).

# What changed

**`internal/brain/verb/new/new.go`** — the verb engine. Exports
`Args{Kind, Title, Body}`, `Result{ID, Kind}`, a narrow `IssueCreator`
interface (just `CreateIssue`), and a `Verb` struct that implements
`verb.BrainVerb[Args, Result]`. `Run` validates kind → kind-in-set →
title → storage configured, then builds a `*types.Issue` with
`Status=open`, `Priority=2` (bd's create default), `IssueType=Kind`,
and `CreatedBy=actor`, calls `store.CreateIssue`, and returns the
allocated ID. No stdout/stderr writes — the contract from
`internal/brain/verb/verb.go` forbids it. The package name is
`newverb` (not `new`) so the package body can still use Go's built-in
`new()` function if it ever needs to.

The `IssueCreator` interface is deliberately narrow (one method) rather
than depending on the full `storage.Storage` interface (~60 methods).
Production wires the global `store` (a `storage.DoltStorage`) through;
the test suite wires a 1-method recorder. This matches the
modularity-first principle from Decision #5: seams as narrow as the
verb actually needs.

**`internal/brain/verb/new/new_test.go`** — 11 tests covering the
three happy paths (task / knowledge / both), the validation failures
(invalid kind, empty kind, empty title), body passthrough, nil-store
guard, storage-error wrapping with `%w` so callers can `errors.Is`,
the `Name()` seam pin (`"new"` — must match the Cobra `Use:` first
token), and `ValidKinds()` contents + defensive-copy guard. All
parallel (`t.Parallel()`). Uses a hand-rolled `recorderStore` fake
that satisfies `IssueCreator`; compile-time `var _ newverb.IssueCreator
= ...` assertion catches seam drift at build time.

**`cmd/bd/brain_new.go`** — the Cobra wrapper. Registers
`brainNewCmd` under `brainCmd` (the parent established in
`divergence/0004`). Parses 2 positional args (`<kind> <title>`),
reads the `--body` flag, builds `newverb.Args`, calls the verb's
`Run`, and formats the result for stdout or `--json` (the persistent
`--json` flag from `cmd/bd/main.go`). No business logic, no
validation — every guard lives in the verb. Sets `commandDidWrite`
and `SetLastTouchedID` to match the pattern in `cmd/bd/assign.go`.
The Cobra `Long` help text appends `newverb.ValidKinds()` at init
time so the help string can never drift from the verb's actual
guard. `Run` (not `RunE`) to match the codebase style and use the
existing `FatalError` / `FatalErrorRespectJSON` error sinks.

This wrapper file cannot be compiled or tested locally on the
current dev machine because `cmd/bd` depends transitively on Dolt's
`go-icu-regex` cgo binding, and the installed `icu4c@78` is missing
the `uregex_*` arm64 symbols (`~/.claude/PAI/USER/FRICTION.md`,
entry dated 2026-05-31). Verification is by pattern-match against
`cmd/bd/create.go` and `cmd/bd/assign.go` (both working command
files) and `gofmt -d` clean. The verb subpackage at
`internal/brain/verb/new/` is pure Go and tests run green.

**`internal/types/types.go`** — adds `TypeKnowledge IssueType =
"knowledge"` and `TypeBoth IssueType = "both"` constants and extends
the `IssueType.IsValid()` switch to accept them. Without this, the
existing `Issue.ValidateWithCustom` path in
`internal/storage/issueops/create.go:412` would reject every
`brain new knowledge ...` and `brain new both ...` write at the
storage layer. The change is six lines of code and ~10 lines of
inline comment naming this divergence as the load-bearing reason.

**`divergence/0007-brain-new.md`** — this file. Lands the tranche;
`commits:` field gets filled in by a follow-up commit per the
0002 / 0004 / 0005 / 0006 SHA-record pattern.

# Brain-spec link

This tranche advances:

- **ISA `## Vision`** — moves the "phone-driven brain doc capture"
  experience from "you'd use `bd create --type=...`" to "you say
  `brain new <kind> ...`", which is the vocabulary the rest of the
  spec is written in.
- **ISA `## Decisions` → Decision #5 (modularity-first architecture)**
  — first concrete use of the `BrainVerb` seam from
  `internal/brain/verb/verb.go`. Proves the seam holds: the verb
  package depends on `verb.BrainVerb`, not on the storage layer's
  full surface. A future tranche can swap the storage implementation
  by changing the constructor argument, not the verb's `Run`.
- **ISA `## Features` → cli-aliases row** — the `brain new` token
  is the first added verb on top of bd's existing CLI. Future
  tranches add `brain link`, `brain related`, `brain recast` against
  the same parent command (`brainCmd`) and the same seam.
- **ISA `## Capabilities` → kind discriminator** — the change to
  `internal/types/types.go` is the smallest possible relaxation of
  bd's `IssueType.IsValid()` to admit the two brain-flavoured kinds.
  No schema migration. No new column. The kind tag rides on a TEXT
  column bd already had.
- **ISA `ISC-104` / `ISC-105` / `ISC-106`** — the three Given/When/Then
  scenarios in `WHAT_IS_BRAIN.md` § 4.1 ("create a task from a phone",
  "create a knowledge doc mid-conversation", "create a both because
  the work and the lesson are inseparable") are covered by the three
  happy-path tests in `new_test.go`.

# Cross-system mirror

The brain v0.3 code in this tranche lives only in the brain repo.
There is no PAI mirror because there is no markdown surface to
mirror — `internal/brain/verb/new/*` and `cmd/bd/brain_new.go` are
Go source, not documentation. The divergence file itself lives only
under `brain/divergence/`, matching the convention established in
0001 through 0006: divergence files are the brain repo's
historical anchor for changes that diverge from upstream bd, and
they do not get copied into the PAI tree.

The PAI mirror established in 0005 / 0006 is for
`docs/brain/*` only. When a future tranche updates
`WHAT_IS_BRAIN.md` to mark `brain new` as "shipped" rather than
"planned," that doc update will also land in
`~/.claude/PAI/DOCUMENTATION/Brain/WHAT_IS_BRAIN.md` per the existing
maintenance rule.
