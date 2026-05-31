---
id: 0004
title: brain verb seam and parent command
isc: [ISC-111]
status: proposed
created: 2026-05-31
updated: 2026-05-31
commits: []
touches: [internal/brain/verb/verb.go, internal/brain/verb/verb_test.go, cmd/bd/brain.go, cmd/bd/brain_test.go]
upstream_rebase_notes: |
  Both files are brain-only. `internal/brain/verb/` did not exist
  upstream and has no equivalent. `cmd/bd/brain.go` adds a new top-level
  Cobra subcommand to the existing `bd` root; upstream bd has no `brain`
  verb namespace and will never collide on this path. On any bd → brain
  sync that touches `cmd/bd/`, resolve `cmd/bd/brain.go` and any
  `cmd/bd/brain_*.go` files with `ours`. The `internal/brain/` tree is
  brain-only end to end.
---

# Why

Decision #5 (modularity-first architecture, ratified 2026-05-31, see
[ISA.md](../ISA.md) §"First-Tranche Decisions") names five seams that
quarantine v0.3's eager defaults behind hours-to-swap interfaces.
`BrainVerb` is the first of those seams — the contract every brain CLI
verb implements. ISC-111 (the `brain` root command and verb-discovery
surface) is the smallest probe that proves the seam landed.

This commit is the foundation the next tranche commit (verbs new/show/list/
link/related, divergence 0005) builds against. Landing the interface and
the parent command together — before any verb — makes the seam visible
in `git log` as its own decision: "here is the door every verb walks
through." If we got the door wrong, the cost is to fix this one file;
the verbs that follow inherit the fix.

Plain English: pick the shape of the swappable seam now, prove it with
a tiny test that any concrete verb can satisfy, then build the verbs
against it.

# What changed

Four files land. Zero existing files modified.

**`internal/brain/verb/verb.go`** — defines `BrainVerb[Args, Result any]`,
a Go 1.18+ generic interface with two methods:

- `Name() string` — the verb word as it appears on the CLI.
- `Run(ctx context.Context, args Args) (Result, error)` — executes the
  verb against caller-parsed flags, returns a structured result the
  wrapper formats for stdout / JSON.

Documented constraints in the package godoc:

- The Cobra wrapper at `cmd/bd/brain_<verb>.go` is the only place that
  knows the concrete `Args`/`Result` types. The engine package never
  writes to stdout.
- Generics were chosen over an `any`-typed signature because every call
  site (the wrapper) already knows its concrete types at compile time;
  erasing them to `any` adds a cast at every call with zero callers
  benefiting. If a future tranche needs a heterogeneous `[]Verb`
  registry, a non-generic facet can be introduced alongside without
  breaking existing impls.

**`internal/brain/verb/verb_test.go`** — three tests that prove the
generic interface compiles and round-trips data:

1. `TestBrainVerb_Run_PassesArgsAndReturnsResult` — happy path.
2. `TestBrainVerb_Run_PropagatesError` — error path.
3. `TestBrainVerb_DifferentConcreteTypes` — regression guard that two
   impls with entirely different Args/Result shapes both satisfy the
   interface (the test was the answer to "is generic really an
   improvement over `any`?").

**`cmd/bd/brain.go`** — adds the `brain` parent Cobra command to
`rootCmd` via `init()`. The command takes no positional args and prints
help when invoked without a subcommand. The long help text references
all five planned verbs (new, show, list, link, related) and links back
to ISA.md so future agents reading `brain --help` learn the seam.

**`cmd/bd/brain_test.go`** — two pure-Go tests that exercise the parent
command's discovery surface:

1. `TestBrainCmd_RegisteredOnRoot` — asserts `rootCmd.Find(["brain"])`
   resolves. Without this, every verb's `brainCmd.AddCommand(...)`
   would be unreachable from the CLI.
2. `TestBrainCmd_HelpMentionsSubcommands` — asserts the help text
   names each verb and points at ISA.md. This is the documentation
   contract — if it breaks, an agent reading `brain --help` loses the
   anchor that tells them where new verbs go.

**Generics signature trade-off recorded:** see the package godoc for
the full rationale. The short version is that `BrainVerb[A, R]`
compiles cleanly against arbitrary concrete types (proven by the third
test) and keeps every wrapper's call site type-safe at compile time.

**No code modified outside the new files.** bd's existing verbs
(`create`, `show`, `list`, `dep`, ...) are untouched. The `brain`
parent command lives alongside them; the next tranche commit (0005)
adds the five child verbs.

# Brain-spec link

[ISA.md](../ISA.md) — see:

- `## Decisions` → `### First-Tranche Decisions (2026-05-31)` →
  Decision #5 (modularity-first architecture) — the seam this commit
  implements.
- `## Criteria` → `### CLI core and verb dispatch` → ISC-111
  (`brain related <id>` — partially advanced here; the parent command
  is the discovery surface that ISC-111 needs to exist for
  `brain related --help` to be reachable). Full ISC-111 coverage lands
  in divergence 0005 with the `related` verb.
- `## Features` → `cli-aliases` row — bd-flavored Cobra subcommands at
  `cmd/bd/brain_*.go`. This commit lands the parent and the seam; the
  verbs themselves land in 0005.
