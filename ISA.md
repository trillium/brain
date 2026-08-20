---
task: brain v0.3 — merged knowledge + task tool on Dolt with Pulse ISR rendering
project: brain
effort: E3
phase: observe
progress: 0/51
mode: standard
started: 2026-05-31
updated: 2026-05-31T13:30
---

# Brain — Project ISA (v0.3, bd-merger)

This file supersedes the brain v0.2 ISA (which articulated body-text search, validate-tightening, and a read-only Pulse module on top of brain.json + markdown). v0.3 absorbs **bd** — a Dolt-on-git issue tracker — into a single tool named **brain**. Trillium does not want to run two CLIs. Verified ISCs from v0.2 (search, smoke, bridge) are preserved in `## Decisions` as historical record. New ISCs start at ISC-100 to honor the ID-stability rule: old IDs are never reissued.

Operational behavior for agents stays in `/Users/trilliumsmith/data/knowledge/AGENTS.md`. This ISA articulates the *thing*; AGENTS.md teaches the *workflow*.

## Problem

Today I run two tools for two kinds of memory. Brain v0.2 holds durable notes — patterns, bugs, concepts — as markdown under `entries/{category}/` with a brain.json index, 181 entries, 18 verbs, 10/10 smoke green. Bd holds work — tasks with state, deps, gates — in a Dolt database with its own CLI. The two systems share nothing. A task that produces a lesson has to be written twice. A lesson that becomes a task has to be copied across. I context-switch between two CLIs to answer one question: what am I working on and what have I learned.

There is no GUI for either. To browse brain entries on my phone I SSH in and run the CLI. Bd's surface is the same. Pulse already runs on port 31337 and renders plans, status, tmux, and wiki as phone-readable HTML, but nothing renders the second brain. The capture surface is good. The retrieval surface is broken everywhere except the laptop terminal.

Storage is also split. Brain.json is the index of record for notes. Dolt is the database of record for tasks. Two substrates means two backup stories, two query languages, and two ways for state to drift. Adding a relationship between a task and a note today requires editing both stores by hand.

## Vision

I open Pulse on my phone. I see one list. Active tasks at the top, recent notes below, a single search box across both. I tap a task, see the notes linked to it. I tap a note, see the tasks that reference it. I type `brain new task "ship the FTS5 indexer"` on my laptop and the new task appears on my phone within seconds. I type `brain new knowledge tech "what I learned shipping the indexer"` after I'm done and the lesson is linked to the task by one verb.

The substrate is Dolt. One table for nodes, one for edges, one discriminator field — `kind` — that says whether this node is a task, a piece of knowledge, or both. Every write to Dolt produces a markdown file on disk in `entries/{kind}/{slug}.md` so that if Dolt goes down tomorrow I still have every word I wrote in plain text. Pulse never talks to Dolt. Pulse reads the markdown files and a small sqlite FTS5 index built from them. That separation is the safety net: the database can be replaced, the renderer can be replaced, and the words survive.

## Out of Scope

Not in the first wave. I will earn the rest by hitting their absence.

Federation — bd's cross-machine sync over SQL deltas — is out. The bridge daemon already syncs the markdown directory. That is enough for v0.3.

Formulas — bd's `cook` and `formula` verbs that generate work from templates — are out. They are a productivity layer on top of the task substrate. The substrate has to exist first.

Bd's preflight, find-duplicates, stale, batch, and query verbs are out. Some come back later. The first wave is the substrate, the CLI verb set that covers daily use, the exfiltration pipeline, and the Pulse module. Nothing else.

Migrating the 181 existing brain v0.2 entries into Dolt before the v0.3 CLI ships is out. The old store stays reachable through a `brain legacy ...` namespace until cutover is complete. Then a one-shot migration script moves them in.

A web UI for writing is out. Pulse is read-only browsing. Writes stay in the CLI so that the CLI remains the only mutator and the graph stays honest.

A graph visualizer is out. Cool. Not load-bearing. Deferred indefinitely.

## Principles

The truths that hold regardless of which database or renderer we pick.

**Single source of truth.** Dolt holds the canonical row for every node. Markdown files are derived. brain.json — the old v0.2 index — is retired. There is one place state lives and one writer that mutates it.

**Derived views.** The markdown files under `entries/{kind}/{slug}.md` and the sqlite FTS5 index are both derived from Dolt. They can be regenerated from scratch by a reconciler. If they drift, the reconciler is the answer, not a manual edit.

**Kind is a write-mode, not a type.** A node is a row. The `kind` field — task, knowledge, or both — tells the CLI which verbs apply, which constraints fire, and which directory the markdown lands in. It does not require two schemas. One table, one row shape, one place to add a field.

**Voice-first editing.** Every markdown file stays plain enough that I can dictate edits through Talon. No HTML in bodies. Frontmatter is YAML with quoted strings where needed. Headings, paragraphs, lists, code fences. That is the vocabulary.

**Plain markdown survives the tool.** If brain disappears tomorrow, the markdown files in `entries/` are still readable in any editor, on any machine, with no migration. That property is non-negotiable.

**Bias toward capture.** Friction kills a memory backup. One verb, one keystroke per capture.

**Forgetting is irreversible.** Archive, do not delete. The graph never loses nodes.

## Constraints

The immovable mandates that scope every implementation choice.

The brain binary is Go — built as a fork of bd. No npm, no npx, no Python, no Node-via-npm runtime. Helpers, daemons, indexers, migration scripts that run inside the brain binary are Go; the throwaway brain v0.2 TS at `~/data/knowledge/` is retired after migration parity. (This constraint was Bun+TS-only in the v0.2 ISA; the fork-direct decision recorded in `## Decisions` 2026-05-31 supersedes that.)

The CLI is the only writer. Humans and agents both mutate state exclusively via `./brain`. Direct edits to Dolt tables outside the CLI are forbidden; direct edits to markdown files are detected on next reconcile and overwritten.

Pulse reads markdown plus sqlite only. Pulse never opens a Dolt connection. The wire between Pulse and the truth substrate is a directory of markdown files and one sqlite index file. That is the contract.

brain v0.2 stays functional until cutover. The 181 entries remain reachable through `brain legacy ...` verbs throughout v0.3 development. The v0.2 `test-smoke.sh` stays green throughout the migration.

Markdown stays voice-editable. Frontmatter fields are human-typeable strings, dates, simple lists. No machine-only blobs in frontmatter.

No `claude` subprocess inline. `CLAUDECODE` env blocks nested sessions. Agent automation runs in separate processes.

Cross-machine sync rides `com.pai.bridge`. The retired `com.pai.sync` rsync layer is dead and must not be revived.

## Goal

Ship brain v0.3: one Go CLI, one Dolt substrate, with every node — task or knowledge or both — captureable, searchable, and inspectable from the CLI, and with the 203 existing v0.2 entries imported into Dolt without loss. v0.3.0 ships the `brain legacy *` read-only namespace alongside the new substrate; v0.3.1 removes legacy after parity and adds the Pulse `/brain/*` module for phone-readable browsing. Done for v0.3 means: Go binary builds, smoke green (new + v0.2 baseline), legacy reads work, and full capture → list → search → related → state → reconcile cycle is verifiable from the CLI on any machine I own. (Pulse + phone-browse are v0.3.1 — see First-Tranche Decision #3.)

## Criteria

Atomic binary tool probes. All new IDs start at ISC-100 to preserve v0.2 ID stability.

### Dolt schema and migrations

- [ ] ISC-100: `dolt sql -q "describe nodes"` shows columns `id, kind, slug, frontmatter, body, state, created, updated` with correct types
- [ ] ISC-101: `dolt sql -q "describe edges"` shows columns `from_id, to_id, edge_type` with `edge_type` constrained to {depends-on, blocks, supersedes, relates-to, extends, learned-from}
- [ ] ISC-102: `dolt sql -q "select kind from nodes group by kind"` returns only values in {task, knowledge, both}
- [ ] ISC-103: A fresh checkout of the brain repo can run a single `brain init` command and end up with a working Dolt database, empty schemas applied, no manual steps

### CLI core and verb dispatch

- [x] ISC-104: `brain new task "ship the indexer"` exits 0 and inserts a row with kind=task into Dolt — landed divergence/0007
- [x] ISC-105: `brain new knowledge "what I learned"` exits 0 and inserts a row with kind=knowledge into Dolt — landed divergence/0007. **Amended 2026-05-31** per divergence/0011: the reframe (divergence/0006) reset the verb signature to `<kind> <title>`, dropping the `category` positional arg. Category-as-flag is deferred future work.
- [x] ISC-106: `brain new both "active investigation"` exits 0 and inserts a row with kind=both — landed divergence/0007
- [ ] ISC-107: `brain show <id>` reads from Dolt and prints frontmatter plus body to stdout
- [ ] ISC-108: `brain list --kind=task` returns only task and both rows; `brain list --kind=knowledge` returns only knowledge and both rows
- [x] ISC-109: `brain link <from> <to> --type=relates-to` inserts a row into the edges table — landed divergence/0008. Verb also accepts `--extends`, `--learned-from`, `--related` as first-class flags.
- [ ] ISC-110: `brain search "phrase"` queries the FTS5 sqlite index and returns ranked results across both kinds
- [x] ISC-111: `brain related <id>` reads edges and returns connected nodes ordered by edge type — landed divergence/0009. BFS with depth cap (default 2), cycle detection, deterministic ordering.

### Jot alias namespace (brain-flavored wisp wrappers)

- [ ] ISC-251: `brain jot save "phrase"` exits 0 and creates an ephemeral wisp via the underlying `bd mol wisp create` mechanism; `brain jot list`, `brain jot show <id>`, `brain jot promote <id>`, and `brain jot gc` are likewise thin Cobra aliases. Implementation lives in `cmd/bd/brain_jot.go` (~50 LOC). No new storage paths.

### Kind-sharded verb constraints

- [ ] ISC-112: `brain state <id> --to=ready` succeeds when the target node has kind=task or kind=both; refuses with a clear error when kind=knowledge
- [ ] ISC-113: `brain depends-on <a> <b>` succeeds when both nodes are kind=task or kind=both; refuses with a clear error when either is kind=knowledge
- [ ] ISC-114: `brain ready` lists only kind=task and kind=both nodes whose dependencies are satisfied
- [ ] ISC-115: `brain file <id> --category=tech` succeeds when kind=knowledge or kind=both; refuses on kind=task with a clear error
- [ ] ISC-116: Shared verbs (`search`, `link`, `show`, `related`) accept any kind without error

### Post-reframe CLI additions (divergence/0006 + 0010 + 0011)

- [x] ISC-151: `brain recast <id> --to=<kind>` shifts an existing brain doc's kind in place — same ID, same edges, same comments, same body. Status defaults to `open` on knowledge→task / knowledge→both transitions when current status is not closed; preserved on all other transitions. Idempotent (no-op when current kind equals target). Landed divergence/0010.
- [x] ISC-152: `brain promote <args>` prints a redirect hint pointing at `brain recast` and `bd promote`, exit non-zero. UX-only namespace-collision affordance — `bd promote` graduates wisps to beads; `brain recast` shifts kind. Landed divergence/0010 (`cmd/bd/brain_promote.go`).

### Exfiltration write hook

- [x] ISC-117: After `brain new task "x"`, a markdown file exists at `entries/task/{slug}.md` with frontmatter mirroring the Dolt row and body matching exactly. Landed divergence/0012 (`internal/brain/exfiltrator/` + `internal/storage/brain_exfiltration_decorator.go`).
- [x] ISC-118: After `brain new knowledge "x"`, a markdown file exists at `entries/knowledge/{slug}.md`. Wording amended (`tech` positional dropped) consistent with ISC-105's post-reframe amendment in divergence/0011 — the verb is now `brain new <kind> <title>`, no category positional. Landed divergence/0012.
- [x] ISC-119: After `brain edit <id>` changes a frontmatter field, the markdown file is rewritten and the new field is present. Decorator re-renders on every `UpdateIssue`. Landed divergence/0012.
- [x] ISC-120: The exfiltration hook completes within 500ms of the Dolt write returning. `BenchmarkRender` measures 10.4ms/op on M1 Max — two orders of magnitude under budget. Landed divergence/0012.
- [x] ISC-121: If the exfiltration hook fails partway, a write-ahead checkpoint at `entries/.checkpoint.json` records the pending node id so `brain reconcile` can finish the job. Checkpoint is written BEFORE the on-disk render and cleared AFTER success. Landed divergence/0012.

### Idempotent reconciler

- [ ] ISC-122: `brain reconcile` walks the Dolt nodes table and rewrites every markdown file from scratch
- [ ] ISC-123: Running `brain reconcile` twice in a row produces byte-identical markdown files (idempotence)
- [ ] ISC-124: `brain reconcile` removes orphan markdown files whose node id is not in Dolt
- [ ] ISC-125: `brain reconcile --check` exits non-zero when any markdown file disagrees with its Dolt row (drift detector, no writes)

### FTS5 index

- [ ] ISC-126: `brain reindex` reads every markdown file under `entries/{kind}/` and writes an sqlite FTS5 database at `entries/.search.sqlite`
- [ ] ISC-127: `brain search "phrase"` returns p50 latency under 100ms on a corpus of 10000 nodes
- [ ] ISC-128: The FTS5 schema indexes title, tags, kind, category, and body as separately weighted columns
- [ ] ISC-129: A title hit ranks above a body-only hit for the same query

### Pulse `/brain/*` module (DEFERRED v0.3.1)

All ISCs in this section are deferred to v0.3.1 per the Go-only constitutional decision (see `## Decisions` → First-Tranche Decisions, 2026-05-31). v0.3 ships CLI-only; Pulse is Next.js (TypeScript) and falls outside the v0.3 single-language constraint.

- [ ] ISC-130 (DEFERRED v0.3.1): `curl -s http://localhost:31337/brain/` returns HTTP 200 with HTML listing recent nodes across both kinds
- [ ] ISC-131 (DEFERRED v0.3.1): `curl -s http://localhost:31337/brain/{slug}` returns the rendered markdown for that node, themed like `/plans/` and `/status/`
- [ ] ISC-132 (DEFERRED v0.3.1): `curl -s http://localhost:31337/brain/api/search?q=phrase` returns JSON results from the FTS5 index in under 100ms
- [ ] ISC-133 (DEFERRED v0.3.1): The Pulse `/brain/*` module source contains zero references to mysql, dolt, or any Dolt client (grep confirms)
- [ ] ISC-134 (DEFERRED v0.3.1): The Pulse `/brain/` page is phone-readable: viewport meta tag set, no horizontal scroll at 375px width
- [ ] ISC-135 (DEFERRED v0.3.1): After `brain new` writes a node, the corresponding Pulse page is reachable within 3 seconds (ISR revalidation via `revalidatePath`)
- [ ] ISC-136 (DEFERRED v0.3.1): Pulse `/brain/{slug}` pages render bidirectional links as clickable anchors to other `/brain/{slug}` pages

### v0.2 to v0.3 migration (soft-sunset model)

Per First-Tranche Decision #4 (2026-05-31), v0.3.0 ships with the `brain legacy *` namespace **read-only and live**, alongside the new Go importer. The legacy namespace is hard-deprecated and removed in v0.3.1 after parity is proven. Importer is Go (`cmd/bd/brain_legacy.go`), satisfying Decision #1 (Go-only). The 203 v0.2 entries (corrected count) remain reachable throughout the proving window.

- [ ] ISC-137: `brain migrate-v02` (Go one-shot in `cmd/bd/brain_legacy.go`) reads every entry under the old `entries/{category}/{id}.md` layout and inserts a kind=knowledge row in Dolt for each
- [ ] ISC-138: After migration, the Dolt node count matches the v0.2 entry count exactly (no losses, no duplicates)
- [ ] ISC-139: Every wikilink in a migrated entry resolves to a Dolt row id (no dangling links)
- [ ] ISC-140 (v0.3.0): `brain legacy show <old-id>` reads the v0.2 brain.json index read-only and continues working throughout v0.3.0; legacy namespace ships in the v0.3.0 release artifact
- [ ] ISC-141 (v0.3.1): After v0.3.1 parity verification, `brain legacy` verbs are removed; brain.json is moved to `entries/.archive/brain-v02.json` as historical record. Removal lands in v0.3.1, not v0.3.0.

### Smoke and integration

- [ ] ISC-142: `bash test-smoke.sh` covers a full capture → link → search → related → state → reconcile cycle and exits 0
- [ ] ISC-143: The v0.2 baseline smoke probes still pass after migration (regression guard)

### Voice editability and mobile readability

- [ ] ISC-144: Every frontmatter field in every markdown file is human-typeable: dates as `YYYY-MM-DD`, tags as flat YAML lists, no nested objects, no base64 blobs
- [ ] ISC-145: The Pulse `/brain/` list page and detail page render without horizontal scroll at 375px viewport width

### Anti-criteria

- [ ] ISC-146: Anti: The 181 existing brain v0.2 entries are NOT lost or corrupted during migration. Pre-migration entry count equals post-migration `kind=knowledge` Dolt row count.
- [ ] ISC-147: Anti: The CLI is NOT the only way to view or search content. Pulse `/brain/*` provides equivalent browse and search without a terminal.
- [ ] ISC-148: Anti: Pulse is NOT coupled to Dolt. The Pulse module reads only markdown files and the FTS5 sqlite index. Grep across the Pulse `/brain/*` source for `mysql|dolt-client|sql-import` returns zero matches.
- [ ] ISC-149: Anti: Markdown files are NOT machine-only. A human can open any file, read it, and edit it through Talon voice without the CLI mediating.
- [ ] ISC-150: Anti: The brain v0.3 runtime is NOT Python, Java, Node-via-npm, or a hybrid of multiple runtimes. The brain binary is the bd Go fork — `go.mod` present; no `package.json` (Node), no `requirements.txt` (Python), no `pom.xml`/`build.gradle` (Java); no second runtime is invoked from the brain binary at runtime.

## Test Strategy

Each ISC is verified by a single binary tool probe.

| isc | type | check | tool |
| --- | --- | --- | --- |
| ISC-100 | schema | `dolt sql -q "describe nodes"` columns match spec | dolt |
| ISC-101 | schema | `dolt sql -q "describe edges"` columns match spec | dolt |
| ISC-102 | schema | `dolt sql -q "select distinct kind from nodes"` values in {task, knowledge, both} | dolt + jq |
| ISC-103 | setup | fresh checkout → `brain init` → schema applied, exit 0 | Bash |
| ISC-104 | cli | `brain new task "x"` exit code, then `dolt sql -q "select count(*) from nodes where kind='task'"` | Bash + dolt |
| ISC-105 | cli | `brain new knowledge tech "x"` exit code, then Dolt row check | Bash + dolt |
| ISC-106 | cli | `brain new both "x"` exit code, then Dolt row check | Bash + dolt |
| ISC-107 | cli | `brain show <id>` prints frontmatter + body, exit 0 | Bash + grep |
| ISC-108 | cli | `brain list --kind=task` and `--kind=knowledge` filter correctly | Bash + jq |
| ISC-109 | cli | `brain link a b --type=relates-to`, then check edges table | Bash + dolt |
| ISC-110 | cli | `brain search "phrase" --json` returns ranked results | Bash + jq |
| ISC-111 | cli | `brain related <id> --json` orders by edge type | Bash + jq |
| ISC-112 | constraint | `brain state <knowledge-id> --to=ready` exits non-zero with kind error | Bash |
| ISC-113 | constraint | `brain depends-on <knowledge-id> <task-id>` exits non-zero | Bash |
| ISC-114 | cli | `brain ready --json` returns only task and both kinds | Bash + jq |
| ISC-115 | constraint | `brain file <task-id> --category=tech` exits non-zero | Bash |
| ISC-116 | cli | shared verbs run cleanly on any kind | Bash |
| ISC-117 | exfiltration | after `brain new task "x"`, `test -f entries/task/<slug>.md` | Bash |
| ISC-118 | exfiltration | after `brain new knowledge tech "x"`, `test -f entries/knowledge/<slug>.md` | Bash |
| ISC-119 | exfiltration | `brain edit <id> --tag=foo`, then grep file for `foo` | Bash + grep |
| ISC-120 | performance | `time` exfiltration hook on synthetic write | Bash + hyperfine |
| ISC-121 | resilience | crash hook mid-write, run `brain reconcile`, file recovers | Bash |
| ISC-122 | reconciler | `rm -rf entries/`, `brain reconcile`, files regenerated | Bash |
| ISC-123 | reconciler | `brain reconcile && cp -r entries a && brain reconcile && diff -r a entries` no diff | Bash + diff |
| ISC-124 | reconciler | seed orphan file, `brain reconcile`, file removed | Bash |
| ISC-125 | reconciler | corrupt one file, `brain reconcile --check` exits non-zero | Bash |
| ISC-126 | index | `brain reindex`, `test -f entries/.search.sqlite` | Bash |
| ISC-127 | performance | 10k synthetic nodes, `time brain search "q"` p50 | Bash + hyperfine |
| ISC-128 | index | `sqlite3 entries/.search.sqlite ".schema"` shows weighted columns | sqlite3 + grep |
| ISC-129 | quality | seed title-match and body-match entries, search ranks title first | Bash + jq |
| ISC-130 | pulse | `curl -s -o /dev/null -w "%{http_code}" http://localhost:31337/brain/` returns 200 | curl |
| ISC-131 | pulse | `curl /brain/<slug>` HTML contains the node title | curl + grep |
| ISC-132 | pulse | `time curl /brain/api/search?q=x` p50 < 100ms | curl + hyperfine |
| ISC-133 | pulse | grep Pulse `/brain/*` source for `mysql|dolt-client|sql-import` | grep |
| ISC-134 | pulse | curl `/brain/`, grep response for viewport meta | curl + grep |
| ISC-135 | pulse | `brain new`, sleep 3s, curl detail page, content present | Bash + curl |
| ISC-136 | pulse | curl detail page, grep for `<a href="/brain/` anchors | curl + grep |
| ISC-137 | migration | `brain migrate-v02`, check Dolt row count vs v0.2 entry count | Bash + dolt |
| ISC-138 | migration | exact-count assertion: 181 in, 181 out (or whatever pre-migration count is) | Bash |
| ISC-139 | migration | scan migrated entries for wikilinks, cross-check against Dolt ids | Bash + jq |
| ISC-140 | migration | `brain legacy show <old-id>` reads brain.json successfully | Bash |
| ISC-141 | migration | post-cutover, `brain legacy` verb returns "removed" message; `test -f entries/.archive/brain-v02.json` | Bash |
| ISC-142 | smoke | `bash test-smoke.sh` exits 0 with v0.3 verbs covered | Bash |
| ISC-143 | regression | v0.2 baseline probes within `test-smoke.sh` still pass | Bash |
| ISC-144 | editability | yamllint or hand-grep frontmatter: dates ISO, tags flat, no nested objects | bun ts script |
| ISC-145 | mobile | curl `/brain/`, render in headless Chrome at 375px, check no horizontal scroll | bun + playwright |
| ISC-146 | anti | pre-migration count vs post-migration `kind=knowledge` count equal | Bash + dolt |
| ISC-147 | anti | walk every verifiable browse and search action through Pulse without terminal | curl |
| ISC-148 | anti | grep Pulse `/brain/*` source for `mysql\|dolt-client\|sql-import` | grep |
| ISC-149 | anti | hand-edit one markdown file, run CLI verbs, edit survives until next reconcile | Bash |
| ISC-150 | anti | `test -f go.mod && ! test -f package.json && ! test -f requirements.txt` at brain repo root | Bash |
| ISC-251 | cli | `brain jot save "x"` exit code, then `bd mol wisp list` shows the row; `brain jot promote <id>` succeeds and the row appears as a permanent bead | Bash + bd |

## Features

Work breakdown from v0.2 to v0.3.

| name | description | satisfies | depends_on | parallelizable | size (post-audit) |
| --- | --- | --- | --- | --- | --- |
| schema-extend | reuse `issues` as nodes; add `extends`/`learned-from` consts to `internal/types/types.go`; kind-enum guard | ISC-100-103 | — | yes | TRIVIAL — Forge has TDD red/green in progress |
| cli-aliases | Cobra subcommands `brain new/show/list/link/related` mapping to existing bd verbs with brain-flavored flags | ISC-104-109, ISC-111 | schema-extend | yes | SMALL — thin Cobra wrappers |
| kind-guards | refuse `state --to=ready` / `depends-on` / `file --category` when issue_type mismatches | ISC-112-116 | cli-aliases | partly | TRIVIAL — guard at command layer |
| exfiltration-hook | `BrainExfiltrationDecorator` stacked on `HookFiringStore`; markdown render on every mutation + checkpoint | ISC-117-121 | cli-aliases | partly | SMALL — ~150-250 Go LOC |
| reconciler | idempotent walk, orphan removal, drift check mode (`brain reconcile [--check]`) | ISC-122-125 | exfiltration-hook | no | MEDIUM — ~200-400 Go LOC |
| fts5-indexer | new `internal/storage/fts/` package; build FTS5 from markdown; `brain search` query path | ISC-110, ISC-126-129 | exfiltration-hook | partly | MEDIUM — ~300-500 Go LOC |
| pulse-brain-module (DEFERRED v0.3.1) | Next.js ISR module at `/brain/*` reading markdown + sqlite. Deferred per Go-only constraint (First-Tranche Decision #1, 2026-05-31); v0.3 ships CLI-only. | ISC-130-136 | fts5-indexer | partly | DEFERRED to v0.3.1 |
| migration-script | one-shot Go importer: v0.2 brain.json + frontmatter → bd issues rows; legacy namespace | ISC-137-141 | reconciler | no | SMALL — ~150 Go LOC |
| smoke-extension | brain-v0.3 probes added to bd's test infrastructure | ISC-142, ISC-143 | migration-script | no | SMALL |
| editability-mobile-guards | frontmatter linter, mobile viewport check | ISC-144, ISC-145 | pulse-brain-module | yes | TRIVIAL |
| anti-coverage | enforcement greps + assertions for ISC-146-150 (note ISC-150 now expects `go.mod`, not bans it) | ISC-146-150 | migration-script | yes | TRIVIAL |
| jot-alias | brain-flavored Cobra alias namespace over `bd mol wisp` + `bd promote` + `bd mol wisp gc` — `brain jot save/list/show/promote/gc`. New file `cmd/bd/brain_jot.go`. Decided 2026-05-31 (First-Tranche Decision #2); supersedes the prior "TRIVIAL or drop" indecision. | ISC-251 | cli-aliases | yes | TRIVIAL — ~50 Go LOC |

## Decisions

- 2026-05-31: brain v0.3 absorbs bd. The merged tool keeps the name **brain**, not `bd-brain`. Trillium said "just brain" — the tool absorbs functionality, the name stays. Frontmatter `project: brain`.
- 2026-05-31: Dolt is the single source of truth. Markdown files under `entries/{kind}/{slug}.md` are derived render artifacts. brain.json (v0.2's index) is retired and moved to `entries/.archive/brain-v02.json` after cutover.
- 2026-05-31: One `nodes` table, one `edges` table, one `kind` discriminator. kind ∈ {task, knowledge, both}. Not two schemas, not two databases. The kind field decides which verbs apply and which directory the markdown lands in.
- 2026-05-31: Exfiltration over Pulse-reads-Dolt-directly. Pulse stays Dolt-unaware. The wire between Pulse and the substrate is markdown files plus one sqlite FTS5 index. This is the safety net: Dolt can be replaced, Pulse can be replaced, the words survive.
- 2026-05-31: FTS5 sqlite over Dolt's fulltext capability for search. FTS5 is decoupled from the substrate choice, ships fast queries, and keeps Pulse Dolt-unaware. Index is rebuilt by `brain reindex` and incrementally updated by the exfiltration hook.
- 2026-05-31: Pulse `/brain/*` follows the established `/plans/*` and `/wiki/*` precedent — Next.js ISR, `generateStaticParams` reads markdown filenames, post-write hook calls `revalidatePath('/brain/{slug}')`.
- 2026-05-31: bd's federation, formulas, preflight, find-duplicates, stale, batch, and query verbs are deferred. First wave is substrate + verbs + exfiltration + Pulse + migration. Earn the rest by hitting their absence.
- 2026-05-31: Brain v0.2 stays functional through the migration window via a `brain legacy ...` namespace. Cutover happens when v0.3 smoke is green and the 181 entries are migrated with exact-count parity. The old smoke test stays green throughout.
- 2026-05-31: ID-stability honored. v0.2 used ISC-1 through ISC-38. v0.3 ISCs start at ISC-100 so v0.2 IDs can be referenced historically without ambiguity. Verified v0.2 ISCs (ISC-3, ISC-4, ISC-11, ISC-12, ISC-13, ISC-15, ISC-18) keep their meanings as historical record.
- 2026-05-31: ID ranges reserved: ISC-100-103 schema; ISC-104-111 CLI core; ISC-112-116 kind constraints; ISC-117-121 exfiltration; ISC-122-125 reconciler; ISC-126-129 FTS5; ISC-130-136 Pulse; ISC-137-141 migration; ISC-142-143 smoke; ISC-144-145 editability/mobile; ISC-146-150 anti.
- 2026-05-31: **Substrate direction confirmed by Trillium: Dolt = source of truth + markdown = exfiltrated render artifact.** This deliberately supersedes prior brain synthesis R7 (markdown=source + SQLite=derived). Trillium chose this side of the trade-off after both were surfaced. Reason: Pulse must consume markdown as its rendering substrate; Dolt's query / ACID / audit properties are more useful on the canonical store than on a derived index. Accepted cost: the exfiltration sync layer (post-write hook + idempotent `brain reconcile`). R7 is superseded, not refuted — the trade-off it identified is real; Trillium picked the side with the sync cost over the side with the query-power cost.
- 2026-05-31: **Implementation direction: fork bd in-place (Go), brain becomes a Go superset of bd.** Trillium's reframe: language is irrelevant to him because the assistant writes the code. Selection axis becomes dependability. Fork-direct wins on every dependability sub-axis: one writer to Dolt (no two-process race or `dolt sql-server` lifecycle), one transaction boundary spanning task+knowledge+edge writes, derived markdown rebuildable from canonical store via `brain reconcile`, reuse of bd's hardened Dolt connection + migration tooling, fewer integration seams to fail. Pulse stays a read-only markdown consumer (proven pattern from `/plans/`, `/wiki/`, `/status/`). Accepted cost: upstream `gastownhall/beads` rebase work — bounded by the existing 56-commit local divergence muscle.
- 2026-05-31: **Lift posture: MEDIUM.** Concrete delta from Explore survey (`~/code/beads` Go surface + `~/data/knowledge` TS surface):
  - Schema delta — TRIVIAL-to-SMALL: bd's `dependencies.type` is already a TEXT column with 16 well-known edge types including `relates-to`, `supersedes`, `related`, `discovered-from`. Brain only needs to add `extends` and possibly `learned-from` (or remap to `discovered-from`). bd's `issues` table has `issue_type VARCHAR(32)` which can host the `kind` discriminator as `knowledge | task | both` with zero schema churn.
  - New subcommands — SMALL each: 18 brain v0.2 verbs port to Cobra subcommands in `cmd/bd/brain_*.go`. Cost breakdown: 3 trivial (link, links, related) + 6 small (capture, file, new, today, ready, inbox) + 5 medium (add, reindex, search, validate, jot) + 1 large (jot — defer to v0.3.1). Rough sum ~1200-1800 Go LOC additions.
  - Exfiltration hook — SMALL: `internal/storage/hook_decorator.go` already wraps every mutation with `on_create`/`on_update`/`on_close` events via `HookFiringStore`. Add a `BrainExfiltrationDecorator` above it; ~150-250 LOC including markdown templating.
  - FTS5 cache — MEDIUM: bd's existing search is SQL LIKE on title; no FTS5 in tree, no conflict. New `internal/storage/fts/` package, ~300-500 LOC. v0.2 weights (`title=8, tags=4, id=3, body=1, fuzzy-penalty=0.4`) port directly to FTS5 column weights.
  - Reconciler (`brain reconcile`) — MEDIUM: ~200-400 LOC; walks Dolt → renders markdown idempotently.
  - Migration of 203 entries (corrected from 181) — SMALL one-shot: `brain import` reads brain.json + frontmatter, INSERTs rows; ~150 LOC.
  - **Total Go LOC delta: ~2500-4000 lines on top of 165k existing (~1.5-2.5% codebase growth).** Not a rewrite.
  - **Rebase exposure: MODERATE.** Fork is 56 commits ahead of upstream with 12 active local branches (feature/dolt-mode-config, fix/backup-remote-server-path, etc.). Schema-touching brain changes need rebase planning against active branches; muscle exists from current TDD-style commit pattern.
  - **Risk areas:** (a) `jot` scratch-note subsystem is the only large port (295 TS LOC → ~400-600 Go LOC, ephemeral cache management) — defer to v0.3.1. (b) brain v0.2 free-form tag arrays need a tags-table decision in Dolt (json column vs. join table). (c) upstream schema-touching landing during brain build forces rebase work.

- 2026-05-31: **Capability-inheritance audit completed. bd's actual surface is larger than the prior lift estimate assumed; several ISCs collapse to thin facades, one (jot) collapses entirely.** See `## Capability Audit` section below for per-ISC mapping. Headline findings:
  - **jot is not a new tool.** bd already ships `bd mol wisp` (ephemeral entities in main DB with `Ephemeral=true`, not synced via git, 1h-default TTL gc with cascade-aware deletion, list/old-detection) + `bd promote <wisp-id>` (wisp → permanent bead, preserves ID/labels/dependencies/events/comments, adds promotion comment). brain v0.2's seven jot verbs (start/show/list/save/edit/discard/update) map 1:1 onto bd primitives. brain jot becomes a thin alias layer (~50 LOC Cobra wrappers) rather than the 400-600 LOC ephemeral-cache implementation the prior lift estimate budgeted. Answer to "do we even need jot?" — no, not as a separate implementation; yes, as a brain-flavored alias namespace if Trillium wants the verb `brain jot` to read naturally.
  - **Edges nearly free.** bd's `dependencies.type` is a TEXT column with 16 well-known types including 4 of brain's 6 (`relates-to`, `supersedes`, `discovered-from`, plus `blocks` ↔ `depends-on` inverse). Only `extends` and `learned-from` need adding — Forge's TDD red commit `259d17a8` adds the failing tests; the green half (consts in `types.go`) is in working tree, ready to commit. ISC-101 collapses to ~20 LOC + tests.
  - **kind discriminator is free.** bd's `issues.issue_type VARCHAR(32)` hosts `kind ∈ {task, knowledge, both}` with zero schema migration. ISC-102 collapses to a typed-enum guard in the verb layer.
  - **Read verbs are renames.** `brain show`/`list`/`related` map to bd's existing `bd show`/`list`/`show --deps`. Add brain-flavored Cobra aliases; no new query logic.
  - **Write verbs are flag passthrough.** `brain new task/knowledge/both` map to `bd create --type=<kind>`. Brain adds: kind-aware default labels, slug derivation, kind-gate guards (ISC-112/113/115).
  - **Exfiltration hook seam is built-in.** `internal/storage/hook_decorator.go`'s `HookFiringStore` already wraps every mutation with `on_create`/`on_update`/`on_close`. Brain's markdown-render decorator stacks on top — no new mutation interception machinery.
  - **What still has to be built fresh:** FTS5 cache (~300-500 LOC, bd has no FTS5 today), the markdown-render decorator (~150-250 LOC), the reconciler (`brain reconcile` ~200-400 LOC), the v0.2 → Dolt one-shot migration (~150 LOC), the Pulse `/brain/*` module (Next.js read-only, mirrors `/plans/*`).
- 2026-05-31: **Lift posture revised MEDIUM → MEDIUM-LOW** after the audit. Jot drops from 400-600 LOC to ~50 LOC (-400). Several verbs go from "port" to "alias" (-300). Total Go delta estimate revised to **~1500-2500 LOC on top of 165k existing (~0.9-1.5% codebase growth)**. Not a rewrite; this is closer to a feature branch than a fork-and-build.

### Capability Audit (2026-05-31)

Per-ISC tag of inheritance from bd:

| ISC range | Capability | bd provides | Brain delta |
| --- | --- | --- | --- |
| ISC-100 | `nodes` table | bd's `issues` table is the nodes table — already has id, body, status, created, updated, comments, labels, dependencies. | Reuse `issues`. Add `kind` (via `issue_type`) and `slug` (derivable from ID or add column). No new table. |
| ISC-101 | `edges` table with 6 edge types | bd's `dependencies` table with TEXT `type` and 16 well-known types covers 4 of 6 (relates-to, supersedes, discovered-from, blocks/depends-on inverse). | Add 2 consts (`extends`, `learned-from`) in `internal/types/types.go`. Forge's `259d17a8` red commit + working-tree green = ready to land. |
| ISC-102 | `kind` discriminator ∈ {task, knowledge, both} | `issues.issue_type VARCHAR(32)` already discriminates. | Add typed-enum guard for the 3 brain values. |
| ISC-103 | `brain init` bootstrap | `bd init` exists with migrations runner. | Cobra alias `brain init` → `bd init`. |
| ISC-104-106 | `brain new task/knowledge/both` | `bd create --type=<t>` exists with `--label`, `--ephemeral`, full flag set. | Thin Cobra subcommand maps `brain new <kind>` to `bd create --type=<kind>`. |
| ISC-107 | `brain show <id>` | `bd show <id>` exists. | Cobra alias. |
| ISC-108 | `brain list --kind=<k>` | `bd list --type=<t>` already supports IssueType filter. | Cobra alias with flag rename. |
| ISC-109 | `brain link <a> <b> --type=<t>` | `bd dep add <a> <b> --type=<t>` exists. | Cobra alias. |
| ISC-110 | `brain search "phrase"` | bd has SQL LIKE on title — not FTS5. | Build FTS5 cache (new `internal/storage/fts/` package). |
| ISC-111 | `brain related <id>` | `bd show <id> --deps` or `bd dep list <id>` exists. | Cobra alias. |
| ISC-112-115 | Kind-sharded verb guards | bd has no kind-gate; state-machine doesn't check `issue_type`. | Guard layer in brain Cobra commands — refuse when `issue_type` mismatches. Small. |
| ISC-116 | Shared verbs accept any kind | Automatic — bd's verbs already accept any `issue_type`. | Zero. |
| ISC-117-119 | Exfiltration to markdown on every write | `HookFiringStore` decorator fires `on_create`/`on_update`/`on_close` for every mutation. | Add `BrainExfiltrationDecorator` that subscribes to those events and writes `entries/{kind}/{slug}.md`. ~150-250 LOC. |
| ISC-120 | Exfiltration latency <500ms | Hook decorator is in-process synchronous wrap. | Measure; should be well under. |
| ISC-121 | Write-ahead checkpoint for crash recovery | bd has no equivalent. | Build small checkpoint file at `entries/.checkpoint.json`. |
| ISC-122-125 | Reconciler (`brain reconcile`) | bd has no markdown-render path. | Build fresh. ~200-400 LOC. |
| ISC-126-129 | FTS5 sqlite index | bd has no FTS5. | Build fresh `internal/storage/fts/`. ~300-500 LOC. |
| ISC-130-136 | Pulse `/brain/*` read-only module | Pulse exists at port 31337 with `/plans/*` precedent. | New module, mirrors plans-viewer. No bd code involved (Pulse is TS/Next.js). |
| ISC-137-141 | v0.2 → Dolt one-shot migration | Not bd's problem. | Build a Go one-shot that reads brain.json + frontmatter, INSERTs `issues` rows. ~150 LOC. |
| ISC-142-143 | Smoke + regression | bd has its own test infrastructure. | Extend with brain-specific probes. |
| ISC-144-145 | Voice / mobile acceptance | Not code, acceptance criteria. | Frontmatter linter + headless-Chrome 375px probe. |
| ISC-146-150 | Anti-criteria enforcement | Greps + assertions. | Small. Note ISC-150 was flipped (Go is now correct, not forbidden). |

**Verbs not in brain v0.2 that bd brings for free** (worth considering as bonus capabilities, not new ISCs unless Trillium wants them surfaced):

- **wisp / promote** — ephemeral capture with promote-to-permanent. Maps onto brain jot if we want the verb to read brain-flavored, or use `bd mol wisp` directly.
- **comments** — per-issue audit trail. Brain didn't have these in v0.2; could become first-class for knowledge nodes (`brain comment <id> "..."`).
- **labels** — bd already has a tag system. Maps to brain v0.2 tags 1:1.
- **dependencies graph queries** — `bd ready`, `bd blocked`, `bd dep graph`. Brain v0.2 had no graph queries; these come free.
- **status / priority** — bd already tracks both. Brain v0.2 had simple state; bd's richer model is a strict superset.
- **events / audit log** — bd's `events` table records everything. Brain v0.2 had no event log; this is a free upgrade.
- **dolt push/pull** — git-backed sync of the canonical store. Brain v0.2 sync rode `com.pai.bridge` over markdown; bd adds a SQL-delta sync option if we ever want it (deferred per Decisions 2026-05-31).
- **JSON output everywhere** — bd's `--json` flag is universal. Brain v0.2 had partial JSON output; now uniform.

### First-Tranche Decisions (2026-05-31)

Four architectural decisions ratified by Trillium 2026-05-31 in response to the ask: "sensible defaults that center around the tool being dependable, written in one programming language, with all choices documented." Decision #1 is the constitutional gate; #3 and #4 follow from it; #2 is a UX continuity pick that fits the gate cleanly. See `divergence/0002-first-tranche-decisions.md` for the paired divergence-trail entry.

**Decision 1 — v0.3 is Go-only (constitutional).**

- **What:** The brain v0.3 surface is the `brain` Go binary. No TypeScript, JavaScript, Python, or other language in the v0.3 critical path. Web/UI surfaces (Pulse `/brain/*` module) deferred until v0.3.1.
- **Why:** Dependability via single-language constraint. One toolchain, one test runner, one deployment artifact, one set of failure modes. Reduces v0.3 release variance and shrinks the verifiable surface.
- **How to apply:** Reject any ISC that introduces non-Go code into the v0.3 critical path. This is the constitutional gate; Decisions #3 and #4 below cascade from it.

**Decision 2 — `brain jot` is a Cobra alias namespace (~50 LOC).**

- **What:** Implement `brain jot save/list/show/promote/gc` as thin Cobra wrappers over bd's existing `mol wisp create/list/show` + `promote` + `mol wisp gc` commands. New file: `cmd/bd/brain_jot.go`. Zero new storage paths.
- **Why:** Ergonomic continuity with brain v0.2. The muscle memory is `brain jot save`, not `brain mol wisp create`. The bd verbs already cover the entire jot pattern (verified by the Capability Audit above); wrapping them preserves UX without duplicating mechanism. ~50 LOC is trivially maintainable.
- **How to apply:** ISC-251 (`jot-alias`) added to the Criteria + Test Strategy + Features sections. Depends on ISC-104-109 CLI aliases landing first. Supersedes the prior "TRIVIAL or drop" indecision in the Capability Audit and the `project-brain-v03-lift` project memory.

**Decision 3 — Pulse `/brain/*` module deferred to v0.3.1.**

- **What:** ISCs 130-136 (Pulse module) marked DEFERRED in the ISA with a `v0.3.1` milestone tag. v0.3 ships CLI-only.
- **Why:** Pulse is Next.js (TypeScript). Including it in v0.3 violates Decision #1 (Go-only). Excluding it ships v0.3 ~2-3 weeks faster with a smaller verifiable surface. v0.3.1 picks Pulse up as its own focused tranche.
- **How to apply:** ISC-130 through ISC-136 section header and per-ISC rows tagged `(DEFERRED v0.3.1)`. The v0.3 first tranche stops at ISC-126-129 (FTS5).

**Decision 4 — v0.2 → v0.3 cutover via soft sunset (legacy ships in v0.3.0, deprecates in v0.3.1).**

- **What:** v0.3.0 ships with `brain legacy *` read-only namespace + a one-shot Go importer (`cmd/bd/brain_legacy.go`). The 203 v0.2 entries remain reachable through migration. v0.3.1 removes the legacy namespace after parity verification.
- **Why:** Dependability via safety net. A hard cutover at v0.3.0 risks data loss if the importer has edge-case bugs. Soft sunset keeps both surfaces live during the proving period. Importer is Go (not the throwaway TS brain-v0.2 codebase), so it satisfies Decision #1.
- **How to apply:** ISC-137-141 updated to reflect dual-namespace ship at v0.3.0 with read-only legacy access; hard-deprecation moves to v0.3.1. Migration section retitled "v0.2 to v0.3 migration (soft-sunset model)."

**Decision 5 — Modularity-first architecture (build forward, swap cheaply).**

- **What:** v0.3 picks defaults eagerly and ships working code rather than wait on ratification, but every default is quarantined behind one of five named interface boundaries so reversal is hours, not days. The five seams: **`BrainVerb`** — per-verb CLI behavior, one Cobra file per verb at `cmd/bd/brain_<verb>.go` (new, show, list, link, related, jot, legacy, etc.). **`Exfiltrator`** — `Render(node) error`; default writes markdown to `~/data/knowledge/entries/{kind}/{id}.md`, wired via a HookFiringStore-style decorator. **`Reconciler`** — `Reconcile(ctx, scope) (Report, error)`; default diffs filesystem vs Dolt and applies idempotent fixes. **`SearchBackend`** — `Index(node); Search(query) []Result`; default is sqlite FTS5 in `internal/storage/fts/`. **`LegacyImporter`** — `Import(source) (Report, error)`; default reads brain v0.2 JSON + markdown into Dolt. Each interface lives in its own `internal/brain/<thing>/` package so the bd codebase stays unpolluted.
- **Why:** Trillium's direction 2026-05-31: "It is OK if we pick the wrong thing... as long as I know the choices that have been made and we can address them. Make sure to design the code so it is modular — we would like to be able to swap out parts and plug in other parts in case any of the parts we have built are not correct." Modularity is the safety net that lets every other First-Tranche decision stay cheap: Go-only stays cheap because each subsystem is one interface and one default; the soft sunset stays cheap because the importer is one interface, not a re-shaped storage layer; the `.brain/` config dir + binary rename (Decision #3 coexistence) becomes a one-file swap instead of a refactor.
- **How to apply:** Future Forge runs implement each ISC against its named seam. CLI alias ISCs (104-109) go through `BrainVerb` + `cmd/bd/brain_<verb>.go`. FTS5 ISCs (126-129) go through `SearchBackend`. Reconcile / exfiltrate ISCs (117-125) go through `Exfiltrator` + `Reconciler`. Legacy migration ISCs (137-141) go through `LegacyImporter`. Storage ISCs (100, 200-204) stay in `internal/storage/`. Each new engine package gets its own test surface. See `divergence/0003-modularity-first.md` for the paired divergence-trail entry, including a Modularity Test section naming three concrete swap scenarios future-Trillium can audit to check whether modularity is real or aspirational.

### Preserved from v0.2 (historical, do not re-verify)

- v0.2 ISC-3 (body-text search), ISC-4 (JSON ranking), ISC-11 (title boost), ISC-12 (p50 < 100ms warm), ISC-13 (typo tolerance) were verified 2026-05-31 against the brain v0.2 in-process search index. v0.3 supersedes these with FTS5-backed search (ISC-126-129). The v0.2 search ranking weights (title=8, tags=4, id=3, body=1) inform FTS5 column weights but are not re-applied verbatim.
- v0.2 ISC-15 (`com.pai.bridge` loaded on MacBook) and ISC-18 (Anti: `com.pai.sync` not loaded) remain operationally true and are inherited by v0.3 as cross-machine sync prerequisites.

## Changelog

- 2026-05-31T13:30 — Decision #5 added: modularity-first architecture; five named interface boundaries (BrainVerb, Exfiltrator, Reconciler, SearchBackend, LegacyImporter); per-verb Cobra file convention. See `divergence/0003-modularity-first.md`.
- 2026-05-31 — **conjecture**: the bd-brain merger needed a new tool name (bd-brain or similar) to avoid confusion with the prior brain. **refutation**: Trillium said "just brain." **learning**: the tool absorbs the functionality; the name stays. **criterion_now**: ISA frontmatter is `project: brain`, not `project: bd-brain`. CLI binary stays `./brain`. bd is retired in name as well as in process.
- 2026-05-31 — **conjecture**: Pulse could query Dolt directly for fresher reads. **refutation**: coupling Pulse to Dolt means Pulse breaks when Dolt is down, and the markdown-survives-the-tool property is lost. **learning**: derived views are a safety property, not a performance compromise. **criterion_now**: ISC-148 enforces zero Dolt client references in Pulse `/brain/*` source.
- 2026-05-31 — **conjecture**: the kind discriminator could be modeled as two separate tables (`tasks`, `knowledge`) joined by a polymorphic edge table. **refutation**: that fragments the row shape and makes shared verbs (search, link, related) write two queries every time. **learning**: kind is a write-mode for verbs, not a type for storage. **criterion_now**: ISC-100 enforces a single `nodes` table; ISC-102 enforces kind values.
- 2026-05-31 — **conjecture**: spawning the Architect to supersede an uncommitted ISA was safe. **refutation**: the prior-turn prompt drifted substrate direction (Dolt=source instead of the prior synthesis R7's markdown=source); the Architect followed instructions and overwrote the prior v0.2 ISA; the file was untracked in git, so the prior 27kb of v0.2 articulation was unrecoverable. **learning**: any writer agent operating on an existing ISA must first commit it; uncommitted artifacts have no rollback path. **criterion_now**: follow-up — add Anti-ISC during BUILD that any superseding write to an ISA requires the prior version to be committed (or stashed) first. Logged here as a learning, deferred from the ISC list until BUILD opens it.
- 2026-05-31 — **conjecture**: implementation-language choice (TS harness around bd vs Go fork of bd) was a tradeoff in Trillium's daily ergonomics. **refutation**: Trillium reframed — he is not the one writing the code, so the language is cost-free at the executor side. Dependability becomes the only deciding axis. **learning**: when the executor is the implementer, "which language does the user prefer" is a phantom tradeoff; the real axes are single-writer property, transaction atomicity, integration-seam count, and rebase exposure. **criterion_now**: Decisions log records fork-direct selection on dependability grounds; Features section is reinterpreted with Go semantics (Cobra subcommands in `cmd/bd/brain_*.go`, schema migrations in `internal/storage/schema/migrations/`, hook decorator at `internal/storage/hook_decorator.go`); brain v0.2 TS codebase becomes throwaway after migration parity (ISC-137-141).
- 2026-05-31 — **conjecture**: v0.3 should ship Pulse + a TypeScript importer alongside Go core for capability parity at release. **refutation**: that violates the single-language dependability constraint Trillium asked for; one toolchain reduces failure modes. **learning**: cutting scope to one language for v0.3 ships faster with a smaller verifiable surface; Pulse and migration parity become v0.3.1 work. **criterion_now**: First-Tranche Decisions (2026-05-31) — #1 Go-only, #2 jot alias (~50 LOC), #3 Pulse deferred to v0.3.1, #4 legacy namespace soft-sunsets (ships read-only in v0.3.0, removed in v0.3.1). See `divergence/0002-first-tranche-decisions.md`.
- 2026-05-31 — **conjecture**: jot was a 295-line v0.2 subsystem that would port to ~400-600 Go LOC because ephemeral cache management is genuinely complex. **refutation**: bd already ships the entire ephemeral-cache pattern as `wisps` — a parallel main-DB table with `Ephemeral=true`, TTL-aware `bd mol wisp gc` (1h default, cascade-aware, --exclude-type), `bd mol wisp list` with old-detection (>24h), and `bd promote <wisp-id>` that moves wisp → permanent bead preserving ID/labels/dependencies/events/comments and writing a promotion comment. brain v0.2 jot verbs map 1:1 onto these primitives. **learning**: capability-inheritance audits should run before any "port the old subsystem" estimate is trusted; the new substrate's idioms may already cover the old subsystem completely. **criterion_now**: jot drops from a feature row to an optional alias row in the Features table; ISC-150 was flipped (Go is now expected, not forbidden); lift posture revised MEDIUM → MEDIUM-LOW (~1500-2500 Go LOC delta, down from ~2500-4000). The bd verbs `wisp` + `promote` are the answer to "do we still need jot?" — no, not as a fresh implementation.

## Verification

Evidence per ISC; section grows as more flip green.

