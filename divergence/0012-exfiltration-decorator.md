---
id: 0012
title: brain exfiltration decorator (markdown render on every mutation)
isc: [117, 118, 119, 120, 121]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: []
touches:
  - internal/brain/exfiltrator/exfiltrator.go
  - internal/brain/exfiltrator/exfiltrator_test.go
  - internal/brain/exfiltrator/exfiltrator_bench_test.go
  - internal/storage/brain_exfiltration_decorator.go
  - internal/storage/brain_exfiltration_decorator_test.go
  - cmd/bd/brain_config.go
  - cmd/bd/main.go
  - ISA.md
upstream_rebase_notes: |
  All paths under `internal/brain/`, `cmd/bd/brain_*.go`, the
  `brain_exfiltration_decorator*.go` files under `internal/storage/`,
  and `ISA.md` are brain-only. On any bd → brain rebase, resolve them
  with `ours` — there is no upstream counterpart. The single edit in
  `cmd/bd/main.go` (the decorator stack 6-line block after the
  HookFiringStore wrap, around line 1129) IS upstream-adjacent; on
  rebase, keep the block and re-anchor it after the new HookFiringStore
  call site.
---

# Why

ISC-117 through ISC-121 (the exfiltration write hook) had been `[ ]` since
the brain v0.3 ISA was first drafted. The four CLI verbs (`brain new`,
`brain link`, `brain related`, `brain recast`) had landed in divergence
0007-0010 and writing brain docs into Dolt was already working — but
nothing on disk reflected the writes. Brain docs lived in the database
and nowhere else. Pulse couldn't see them. `grep` couldn't see them. The
safety-net property in WHAT_IS_BRAIN.md § 6 (*plain markdown survives the
tool*) was a promise the code was not yet keeping.

This entry closes that gap. Every brain-flavored mutation now writes
`entries/{kind}/{slug}.md` synchronously, before the CLI returns. The
file is the canonical render artifact for that brain doc on disk.

The reconciler half (ISC-122-125) lands next — it walks the Dolt nodes
table and regenerates every file from scratch, plus a drift-check mode.
The exfiltration hook is the steady-state path; the reconciler is the
recovery path.

# What changed

Three Go layers + one ISA flip, split into three feat commits + this
docs commit + a SHA-record commit, for atomic semantic history:

**Commit A — `feat(brain): add markdown exfiltrator package`** (`internal/brain/exfiltrator/`):

A new pure-Go package — no Dolt, no sql, no cgo. Owns three load-bearing
concerns:

1. **Slug derivation.** Issue → on-disk filename. Slug is kebab-case of
   the title at creation, persisted to `issues.metadata` JSON under key
   `brain_slug` so it stays stable across title edits. Collisions append
   the last six chars of the issue ID. Empty titles fall back to the
   bare ID.
2. **Markdown render.** Issue → YAML frontmatter (id, title, kind,
   status, priority, created, updated, labels) + H1 + body. Written
   atomically via tmp+rename so a crash never leaves a half-written file
   on disk.
3. **Checkpoint file (ISC-121).** `entries/.checkpoint.json` is written
   BEFORE the on-disk render starts and removed AFTER it succeeds. A
   crash mid-write leaves the checkpoint for `brain reconcile` to finish
   on the next run.

13 unit tests + `BenchmarkRender`. Bench measures 10.4ms/op on M1 Max
against ISC-120's 500ms budget.

**Commit B — `feat(brain): add BrainExfiltrationDecorator on DoltStorage`** (`internal/storage/brain_exfiltration_decorator.go`):

Decorator that wraps `DoltStorage` and dispatches to the exfiltrator
after every successful mutation that produces a brain-kind issue
(`kind ∈ {task, knowledge, both}`). Mirrors `HookFiringStore`'s mutation
coverage exactly — CreateIssue, CreateIssues, UpdateIssue, ReopenIssue,
UpdateIssueType, CloseIssue, AddDependency, RemoveDependency, AddLabel,
RemoveLabel, AddIssueComment, RunInTransaction.

Inside `RunInTransaction`, renders are deferred until commit and
deduplicated per issue. On rollback, all pending renders are dropped.

`UpdateIssueType` handles the brain recast scenario: when the kind
changes between two brain kinds the new file is written and the old
kind's file is removed; when the kind transitions in or out of the
brain set only the relevant side fires.

Non-brain mutations passthrough untouched — bd's own behavior does not
change. WHAT_IS_BRAIN.md § 8 calls this out: *"Exfiltration is a
decorator on bd's existing HookFiringStore, not a parallel write path."*

23 tests including transaction commit/rollback, dedup, brain↔non-brain
transitions, nil-exfiltrator passthrough.

**Commit C — `feat(brain): wire BrainExfiltrationDecorator into bd binary`** (`cmd/bd/main.go` + new `cmd/bd/brain_config.go`):

Stacks the decorator above `HookFiringStore` in the bd boot path. New
escape hatch `BRAIN_NO_EXFIL=1` disables rendering (mirrors `BD_NO_HOOKS`
for the same use cases — bulk imports, migrations). Knowledge root
resolves via `BRAIN_KNOWLEDGE_ROOT` env override, otherwise
`~/data/knowledge`. If the home dir cannot be resolved and no override
is set, the exfiltrator constructor returns nil and the decorator
becomes a passthrough — bd still works on knowledge-root-less machines.

The decorator chain after this commit:

```
rawStore → HookFiringStore → BrainExfiltrationDecorator → store
```

**Commit D (this entry)** flips ISC-117-121 from `[ ]` → `[x]` in ISA.md
and amends ISC-118's wording inline (`brain new knowledge tech "x"` →
`brain new knowledge "x"`) consistent with ISC-105's post-reframe
amendment in divergence/0011 — the verb signature is `<kind> <title>`,
no category positional.

# Brain-spec link

This entry advances the following ISA elements:

- `## Vision` — the exfiltration loop is now real. Pulse can read
  `entries/{kind}/{slug}.md` without knowing anything about Dolt.
- `## Decisions` → Decision #5 (modularity-first) — exfiltrator lives in
  its own package, the decorator lives in `internal/storage` alongside
  HookFiringStore for symmetry, and wiring lives in a thin
  `cmd/bd/brain_config.go`. Three swappable layers.
- `## Decisions` → 2026-05-31 substrate decision — Dolt is the source of
  truth, markdown is the exfiltrated render artifact. This is the code
  that makes that direction observable on disk.
- `## Criteria` → ISC-117, 118, 119, 120, 121 — see the green flips
  above.
- `## Features` → `exfiltration-hook` row — now shipped. The
  `reconciler` row depends on this and is next.
- `## Capabilities` → BrainExfiltrationDecorator (new), MarkdownExfiltrator
  (new), checkpoint file handling (new).

The four exfiltration-adjacent ISCs that remain `[ ]`:

- **ISC-122-125** (reconciler) — depends on this commit; the next
  tranche. The reconciler will walk Dolt and rewrite every file from
  scratch, remove orphans, and offer a `--check` mode for drift
  detection.

# Cross-system mirror

None. ISA.md and the brain-only Go paths stay in the brain repo. The
`cmd/bd/main.go` edit is the only upstream-adjacent change and is
documented in `upstream_rebase_notes` above.

# Decisions documented (sensible defaults for v0.3, per Trillium's "you pick" directive)

Recorded here so future-Trillium can see what was chosen on his behalf:

1. **Slug = kebab-case of title at creation, persisted in
   `issues.metadata` under `brain_slug`.** Stable across title edits —
   if Trillium renames a doc, the file does not move. Rationale: file
   moves on title edit break `grep` history, IDE caches, and external
   bookmarks. Collisions append the last six chars of the issue ID;
   empty titles fall back to the bare ID. Future Trillium can opt into
   "slug-tracks-title" with a `brain edit --rename-file` flag — the
   exfiltrator already has the Remove primitive.
2. **Knowledge root = `~/data/knowledge`, override via
   `BRAIN_KNOWLEDGE_ROOT` env.** Matches WHAT_IS_BRAIN.md § 1 literally
   and matches the existing brain v0.2 layout so a future migration
   tranche (ISC-137-141) reads the same directory v0.2 wrote.
3. **Frontmatter shape: id, title, kind, status, priority, created,
   updated, labels.** No dependencies, no comments, no description in
   frontmatter — body holds description, dependencies stay in Dolt
   (they are graph data, not document data). H1 is rendered above the
   body so the file reads naturally in any markdown viewer.
4. **Kind transitions move the file.** `brain recast knowledge → task`
   writes `entries/task/<slug>.md` and removes `entries/knowledge/<slug>.md`
   in the same render call. WHAT_IS_BRAIN.md § 5 line 297 promised this
   behavior; this commit delivers it.
5. **Checkpoint at `entries/.checkpoint.json` is write-then-clear, not
   append-only.** Pending ops are visible to the reconciler while the
   render is in flight; success removes the file entirely. Reduces disk
   churn vs an append-only log and makes "checkpoint exists" the unambiguous
   signal for "something crashed mid-write."
6. **Atomic writes via tmp + rename.** Standard pattern; no half-written
   files possible. The tmp filename includes a random nonce so two
   in-flight renders for the same slug do not collide.
7. **Synchronous, in-process renders.** No goroutine, no queue, no
   retry loop. ISC-120 demands <500ms; the benchmark holds at 10.4ms.
   If perf becomes a concern later, the decorator's signature already
   supports swapping in an async exfiltrator without touching call sites.
8. **`BRAIN_NO_EXFIL=1` escape hatch.** Mirrors `BD_NO_HOOKS=1`. Useful
   for bulk imports, migrations, and `bd` workflows that have no
   business writing markdown. Default behavior: exfiltrate. Opt-out, not
   opt-in.

# Test surface

| Layer | File | Count | Notes |
|-------|------|-------|-------|
| Exfiltrator unit | `internal/brain/exfiltrator/exfiltrator_test.go` | 13 | slug derivation, frontmatter shape, kind transitions, checkpoint lifecycle, atomic write |
| Exfiltrator bench | `internal/brain/exfiltrator/exfiltrator_bench_test.go` | 1 | `BenchmarkRender` — 10.4ms/op on M1 Max, 500ms budget per ISC-120 |
| Decorator unit | `internal/storage/brain_exfiltration_decorator_test.go` | 23 | each mutation method × happy path + error path; transaction commit/rollback/dedup; brain↔non-brain transitions; nil-exfiltrator passthrough; compile-time interface satisfaction |

All run pure-Go and green on the MacBook even with the libicu/icu4c@78
compile constraint that blocks `cmd/bd/` test runs.
