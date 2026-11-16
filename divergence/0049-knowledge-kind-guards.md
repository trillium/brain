---
id: 0049
title: refuse claim, close and blocks edges on knowledge beads, with --force for triage
isc: [ISC-112, ISC-113, ISC-116]
status: landed
created: 2026-11-16
updated: 2026-11-16
commits: []                        # the commit on fm/task-fable-t1-20261116 that carries this doc
touches:
  - internal/types/types.go
  - internal/types/types_test.go
  - cmd/bd/knowledge_guard.go
  - cmd/bd/knowledge_guard_test.go
  - cmd/bd/knowledge_guard_parity_test.go
  - cmd/bd/close.go
  - cmd/bd/close_direct.go
  - cmd/bd/close_proxied_server.go
  - cmd/bd/update.go
  - cmd/bd/update_proxied_server.go
  - cmd/bd/dep.go
  - cmd/bd/dep_proxied_server.go
  - docs/CLI_REFERENCE.md
  - docs/cli-reference/close.md
  - docs/cli-reference/dep.md
  - docs/cli-reference/update.md
  - docs/brain/WHAT_BRAIN_ADDS.md
  - divergence/0049-knowledge-kind-guards.md
upstream_rebase_notes: |
  `cmd/bd/knowledge_guard.go` and its two test files are brain-only. The
  other cmd/bd files are upstream files with one brain hunk each: a
  `refuseKnowledgeKind` call after the existing close/update validators on
  both the direct and the proxied route, a `refuseKnowledgeBlocksEdge` call
  before each dep write, and a `--force` flag on `dep` and `dep add` (upstream
  has no such flag there; close and update already had one). The `--force`
  help strings on close and update were extended, which regenerates
  docs/CLI_REFERENCE.md and docs/cli-reference/{close,dep,update}.md.
  `internal/types/types.go` gains `IssueType.IsKnowledge()` next to the
  brain kinds (TypeKnowledge, TypeBoth, TypeISA) that 0004 added.
  Conflict hotspots on rebase: the guard order in `closeDirectCheckOne`,
  `closeProxiedCheckOne`, the update loop around `validateIssueUpdatable`,
  and the dep `init()` flag block.
---

# Why

brain keeps facts and work on one `issues` table, told apart by
`issue_type`: `knowledge` is a fact, `task` and bd's other work types are
work, `both` is work that is also a fact ([VISION.md](../VISION.md), the
"brain primitives" reframe in [0006](0006-brain-primitives-reframe.md)). The
retired ISA's ISC-112 and ISC-113 say a knowledge bead is never claimed or
closed and never sits on the blocking graph, and ISC-116 says every other
link stays open to it
([ISA-v03.md](../docs/brain/archive/ISA-v03.md)).

Nothing enforced that. The 2026-11-16 brain-surface report found 172 closed
and 2 blocked knowledge beads in the live stores: agents running
`bd close`, `bd update --claim` and `bd dep add` on a fact because the
verbs accepted it. The report ranked this the one real semantic gap left
in the kind model and asked for a guard with a `--force` escape for triage
(a bead filed as knowledge by mistake, a fact that turned out wrong).

# What changed

- `types.IssueType.IsKnowledge()` reports the literal `knowledge` kind.
  `both` is not knowledge for this purpose: it is work and keeps the work
  verbs. `isa` has a lifecycle of its own and is not gated.
- `cmd/bd/knowledge_guard.go` spells the refusal once:
  `cannot <verb> <id>: it is a knowledge bead (issue_type=knowledge), and
  knowledge is never claimed, closed or put on the blocking graph; pass
  --force to do it anyway`. It matches `errKnowledgeKind` with `errors.Is`.
  An issue the guard cannot read (an external ref, a cross-store id, a
  wisp) is not a verdict: the edge or verb passes to the write, which owns
  the existence check.
- `bd close` refuses a knowledge bead inside the same not-yet-closed branch
  as the pinned and gate checks, on both the direct and the proxied route,
  so a re-close of a knowledge bead that was closed before the guard stays
  the idempotent no-op it was. `-f/--force` waives it.
- `bd update --claim` refuses a knowledge bead, including one requested by
  `--type knowledge` in the same update, on both routes as a per-id policy
  failure (exit 1, never the guard-mismatch 13). Plain field edits pass;
  `--force` waives it.
- `bd dep add`, `bd dep --blocks` and `bd dep add --file` refuse a `blocks`
  edge whose either end is a knowledge bead, on both routes; the bulk form
  names the line. `related`, `parent-child`, `discovered-from`, brain's own
  `extends` and `learned-from` pass. `dep` and `dep add` gain a long-only
  `--force` flag (no `-f`, so no clash with any future short flag).
- The guard is a command-layer policy, like the template and pinned fences
  beside it, not an engine rule: `bd batch`, the GitHub sync and the
  molecule auto-close still write through the store untouched.
- A blocks endpoint that cannot be read by its route fails closed unless
  `--force`, rather than letting an unresolved cross-store knowledge bead
  onto the blocking graph. The remaining concurrent-retype window between
  this command-layer preflight and its write is a known limitation; a
  follow-up should decide whether the shared write contract needs a
  conditional knowledge-kind predicate.
- Help text on the three `--force` flags, the regenerated CLI reference
  pages, and the WHAT_BRAIN_ADDS line that said no guard existed.
- Tests: the kind predicate in `internal/types`; the helper's verdict table,
  the error spelling, the proxied claim through the recorded lifecycle
  stub, and the direct-close spelling in `cmd/bd/knowledge_guard_test.go`;
  close, claim, plain edit, `dep add` both directions, `dep --blocks`,
  `--type related`, `both`, and every `--force` path end to end on the
  in-process parity harness in `cmd/bd/knowledge_guard_parity_test.go`
  (needs the Docker Dolt test server, as every parity test does). The
  PR's evidence holds a transcript of the same sequence on an embedded
  store (direct route) and on a proxied sql-server (proxied route).

Left for follow-ups, named in the brain-surface report as T2–T5 or found
while building: `bd ready` and `--claim-next` still offer knowledge beads
(ISC-114); `bd update --status closed` and `bd create --deps` reach the
store without this guard; `file --category` (ISC-115) is not built.

# Brain-spec link

- [VISION.md](../VISION.md)
- [docs/brain/WHAT_BRAIN_ADDS.md](../docs/brain/WHAT_BRAIN_ADDS.md)
- [docs/brain/archive/ISA-v03.md](../docs/brain/archive/ISA-v03.md) (ISC-112, ISC-113, ISC-116)
