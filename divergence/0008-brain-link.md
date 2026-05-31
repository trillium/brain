---
id: 0008
title: brain link verb implementation
isc: [109]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: [a9ec36e39a5f9a451f7785b8122be0ded09c2145]
touches:
  - internal/brain/verb/link/link.go
  - internal/brain/verb/link/link_test.go
  - cmd/bd/brain_link.go
upstream_rebase_notes: |
  No upstream-touched files. The three paths in this tranche
  (`internal/brain/verb/link/link.go`, `internal/brain/verb/link/link_test.go`,
  `cmd/bd/brain_link.go`) are brain-only additions; they do not exist
  upstream. On any rebase that touches them, resolve with `ours`.

  brain link writes into the existing `dependencies` table — the same
  table bd's `bd dep add` writes to — through the `storage.Storage`
  interface (`GetIssue`, `AddDependency`). The `extends` and `learned-from`
  dependency types it relies on were added to `internal/types/types.go`
  by prior brain commits 259d17a82 and c4b6a78e4; this tranche does NOT
  re-touch `types.go`. If a future bd upstream rebase restructures the
  `dependencies` schema or renames `storage.Storage.AddDependency`, brain
  link needs re-wiring through the `LinkStore` seam — but the verb's
  narrow interface (two methods) makes the surface area to re-wire small.
---

# Why

`brain link <from> <to>` is the second of the four brain-added verbs
named in `docs/brain/WHAT_IS_BRAIN.md` § 4 (`new`, `link`, `related`,
`recast`). It plugs into the same `BrainVerb` seam established in
`divergence/0003-modularity-first.md` and
`divergence/0004-brain-verb-seam-and-parent.md`, under the parent
`brainCmd` already wired by 0004 and first exercised by
`divergence/0007-brain-new.md`.

The reframe in `divergence/0006-brain-primitives-reframe.md` set the
ground rules this tranche obeys, and it is the second concrete proof
of them:

- **brain IS bd renamed.** This verb writes through bd's existing
  storage interface; no separate brain edges table, no separate brain
  schema. `brain link a b --learned-from` and `bd dep add a b --type=learned-from`
  land the *exact same row*. The only divergence is the flag spelling
  on the brain surface.
- **No new edge types.** The four flags map to dependency types bd
  already knows: `--extends` → `extends`, `--learned-from` →
  `learned-from`, `--related` → `related`, `--type <name>` →
  fallthrough to any well-known bd type. `extends` and `learned-from`
  were already added to `internal/types/types.go` in prior commits
  (`259d17a82`, `c4b6a78e4`), so this tranche is a pure code-only
  addition with no schema or type-table touchpoint.
- **Distinct error wording for the two existence-probe sides.** bd's
  storage layer says `"issue X not found"` for both a missing source
  and a missing target. The brain spec (`WHAT_IS_BRAIN.md` § 4.2)
  requires distinct messages so the user knows which doc id to fix.
  The verb does the existence probes itself (single primary-key
  lookups) so it can produce `from brain doc <id> not found` vs
  `target brain doc <id> not found`. This is the load-bearing reason
  the verb does its own existence checks instead of letting
  `storage.AddDependency` produce the error.

This advances ISA `ISC-109` (`brain link <from> <to> --type=relates-to`
inserts a row into the edges table) — the test
`TestRun_HappyPath_FallthroughType` exercises the literal `--type`
spelling against `DepBlocks` and the three brain-flagged variants are
covered by `TestRun_HappyPath_LearnedFrom`, `TestRun_HappyPath_Extends`,
and `TestRun_HappyPath_Related`.

# What changed

**`internal/brain/verb/link/link.go`** — the verb engine. Exports
`Args{From, To, EdgeType}`, `Result{From, To, EdgeType}`, a narrow
`LinkStore` interface (just `GetIssue` and `AddDependency`), and a
`Verb` struct that implements `verb.BrainVerb[Args, Result]`. `Run`
validates required fields → edge type non-empty → edge type length
(`types.DependencyType.IsValid()`) → storage configured, then probes
source and target existence with distinct error wording, then calls
`store.AddDependency` with a `*types.Dependency{IssueID, DependsOnID,
Type}`. No stdout/stderr writes — the contract from
`internal/brain/verb/verb.go` forbids it. Self-edges (from == to)
are permitted; bd's storage layer does not reject them and brain has
no independent knowledge-graph reason to.

The `LinkStore` interface is deliberately narrow (two methods) rather
than depending on the full `storage.Storage` interface (~60 methods).
Production wires the global `store` (a `storage.DoltStorage`) through
the embedded `Storage` interface that satisfies both methods; the test
suite wires a hand-rolled 2-method recorder. This matches the
modularity-first principle from Decision #5 and mirrors `newverb.IssueCreator`
from divergence/0007.

**`internal/brain/verb/link/link_test.go`** — 14 tests covering the
four happy paths (`--learned-from`, `--extends`, `--related`, and the
`--type <name>` fallthrough exercised against `DepBlocks`), the
self-link policy, the two existence-probe sides with their distinct
error wording (`from brain doc ... not found` vs `target brain doc ...
not found`), empty-field guards (from, to, edge-type), the
invalid-edge-type guard with well-known-types recovery hint, the nil-store
guard, the `AddDependency`-error-is-wrapped-with-%w contract, the
`GetIssue` transport-error wrapping (which must NOT be misclassified
as not-found), and the `Name()` seam pin (`"link"` — must match the
Cobra `Use:` first token). All parallel (`t.Parallel()`). Uses a
hand-rolled `recorderStore` fake that satisfies `LinkStore`; compile-time
`var _ link.LinkStore = (*recorderStore)(nil)` assertion catches seam
drift at build time.

**`cmd/bd/brain_link.go`** — the Cobra wrapper. Registers
`brainLinkCmd` under `brainCmd` (the parent established in
`divergence/0004`). Parses 2 positional args (`<from> <to>`),
resolves the four mutually-exclusive edge-type flags (`--extends`,
`--learned-from`, `--related`, `--type <name>`) into a single
edge-type string before calling the verb, and formats the result for
stdout (`linked: <from> —[<type>]→ <to>`) or `--json` (the persistent
`--json` flag from `cmd/bd/main.go`). The mutex-flag resolution is the
ONLY logic in this wrapper — every other guard (empty fields, invalid
type, missing docs, storage errors) lives in the verb. Sets
`commandDidWrite` to match the pattern in `cmd/bd/assign.go` and
`cmd/bd/brain_new.go`. The wrapper does not set `SetLastTouchedID`
because a link write doesn't produce a new id; the row joins two
existing ids.

This wrapper file cannot be compiled or tested locally on the current
dev machine because `cmd/bd` depends transitively on Dolt's
`go-icu-regex` cgo binding, and the installed `icu4c@78` is missing
the `uregex_*` arm64 symbols (`~/.claude/PAI/USER/FRICTION.md`,
entry dated 2026-05-31). Verification is by pattern-match against
`cmd/bd/brain_new.go` (the brain-verb template) and `gofmt -d`
clean. The verb subpackage at `internal/brain/verb/link/` is pure Go
and tests run green:
`go test ./internal/brain/verb/link/...` → `ok`.

**`divergence/0008-brain-link.md`** — this file. Lands the tranche;
`commits:` field gets filled in by a follow-up commit per the
0002 / 0004 / 0005 / 0006 / 0007 SHA-record pattern.

# Brain-spec link

This tranche advances:

- **ISA `## Decisions` → Decision #5 (modularity-first architecture)**
  — second concrete use of the `BrainVerb` seam from
  `internal/brain/verb/verb.go`. Proves the seam composes: the link
  verb depends only on `verb.BrainVerb` and a 2-method `LinkStore`
  interface. The Cobra wrapper in `cmd/bd/brain_link.go` is structurally
  identical to `cmd/bd/brain_new.go` — same dependency wiring, same
  output-formatting pattern, same `commandDidWrite` mark, same FatalError
  rendering. New verbs are now a copy-paste-edit operation, which is
  exactly what Decision #5 promised.
- **ISA `## Features` → cli-aliases row** — `brain link` is the second
  verb added on top of bd's existing CLI. `brain related` (verb #3)
  and `brain recast` (verb #4) attach the same way against the same
  parent command (`brainCmd`) and the same seam.
- **ISA `## Capabilities` → edge-type vocabulary** — `--extends` and
  `--learned-from` are the two brain-flavoured edges; `--related` and
  `--type <name>` fall through to bd's existing dependency types
  (`related`, `blocks`, etc.). The brain surface lets Trillium say the
  edge type the way he says it out loud (`brain link X Y --learned-from`)
  without forcing a new vocabulary on bd's storage layer.
- **ISA `ISC-109`** — `brain link <from> <to> --type=relates-to`
  inserts a row into the edges table. Covered by
  `TestRun_HappyPath_FallthroughType` (against `DepBlocks` —
  `relates-to` is bd's older spelling for `related`, which is
  exercised separately by `TestRun_HappyPath_Related`). Real
  end-to-end verification against Dolt is gated on the libicu fix
  noted in `upstream_rebase_notes`.

# Cross-system mirror

The brain v0.3 code in this tranche lives only in the brain repo.
There is no PAI mirror because there is no markdown surface to
mirror — `internal/brain/verb/link/*` and `cmd/bd/brain_link.go` are
Go source, not documentation. The divergence file itself lives only
under `brain/divergence/`, matching the convention established in
0001 through 0007: divergence files are the brain repo's historical
anchor for changes that diverge from upstream bd, and they do not
get copied into the PAI tree.

The PAI mirror established in 0005 / 0006 is for `docs/brain/*`
only. When a future tranche updates `WHAT_IS_BRAIN.md` to mark
`brain link` as "shipped" rather than "planned," that doc update
will also land in `~/.claude/PAI/DOCUMENTATION/Brain/WHAT_IS_BRAIN.md`
per the existing maintenance rule.
