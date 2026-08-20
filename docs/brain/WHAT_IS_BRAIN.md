# what is brain?

> **Audience:** anyone — including future-Trillium on a phone — who needs to understand what `brain` actually is.
> **TL;DR:** **brain IS bd, renamed.** One binary. The same Go source code, installed under the name `brain` instead of `bd`. Every bd verb (`create`, `show`, `list`, `ready`, `close`, `update`, `dep add`, `prime`, `bootstrap`, ...) is reachable as `brain <verb>` because the binary answers to its installed name. brain ADDS a few verbs (`new`, `link`, `related`, `recast`) and ADDS one column-shaped convention (`kind`), and ADDS a markdown-exfiltration hook. That is the whole delta. There is no "brain layer" routing through bd. There is no "use bd for that, brain for this." There is one tool with two acceptable names, plus a small set of extra verbs glued on.

---

## 1. What brain is

```
                  one Go source tree
                          │
                  go build → one binary
                          │
              ┌───────────┴───────────┐
              ▼                       ▼
     installed as `bd`        installed as `brain`
              │                       │
   answers to `bd <verb>`   answers to `brain <verb>`
              │                       │
              └───────────┬───────────┘
                          │
                  same Cobra dispatch
                          │
                  same internal/storage
                          │
                       same Dolt DB
```

bd already ships a binary-rename mechanism. Install it as `fork`, `spoon`, `ops`, `idea`, or `brain` — the binary detects its own name at startup and answers to it. `brain create`, `brain show`, `brain ready`, `brain close` work the same way `bd create`, `bd show`, `bd ready`, `bd close` work, because **they are the same code path.**

What brain adds, on top of that rename:

1. **A `kind` discriminator** on every brain doc — `task | knowledge | both` — riding on bd's existing `issues.issue_type` column. No schema migration.
2. **Four new verbs** registered on the same Cobra root: `brain new`, `brain link`, `brain related`, `brain recast`. These are the verbs brain ADDS to the binary; bd's existing verbs continue to work unchanged.
3. **Two new edge types** — `extends`, `learned-from` — registered in `internal/types/types.go` (commit `c4b6a78e4`) alongside bd's existing 16.
4. **A markdown exfiltration hook** (`Exfiltrator` seam, not yet built — ISC-117-121) that writes one markdown file per brain doc to `~/data/knowledge/entries/{kind}/{id}.md` on every mutation.

That is the full surface area of "what's actually new." Everything else you can do in brain, you can do because the renamed bd binary already does it.

---

## 2. The mental model: one bag of brain docs

A **brain doc** is the unit. Every brain doc has:

- An **id** (e.g. `B-a7b3c`) — auto-generated, hash-based, collision-free
- A **kind** — `task` | `knowledge` | `both`
- A **title** and **body** (markdown)
- **Edges** to other brain docs (one of the 18 well-known edge types)
- **Optional task fields** — `status`, `priority`, `due`, `labels` (used when kind ∈ {task, both})

Tasks and knowledge live in the **same table**. They share the **same ID space**. They share the **same edge table**. They mix freely.

```
                      one bag of brain docs

                  ●(task)                  ●(knowledge)
                    │                        │
                    └───── related ──────────┘
                              │
                         ●(both) ── extends ── ●(knowledge)
                              │
                       learned-from
                              │
                            ●(task, closed)

   No "knowledge namespace" separate from a "task namespace".
   No "tasks live here, notes live there".
   Just brain docs with a kind tag, all in one bag, all edge-linked.
```

You filter on `kind` the way you filter on a label. `brain list --kind=task` is just `brain list --kind=task`. `brain related B-a7b3c` walks edges across all kinds and prints the subgraph regardless of whether the neighbors are tasks or notes.

This is why "a knowledge doc can be promoted into a task without losing its edges" is the natural expectation: there's nothing to "move" between namespaces, because there are no namespaces. You change one column value (`kind`) and the doc is now a task; every edge it had still points to the same ID.

---

## 3. The voice patterns brain supports

Every primitive in this doc traces back to making at least one of these natural:

| Trillium says (out loud) | Resolves to | What happens |
|---|---|---|
| "create a new brain doc to track the Dolt FK quirk I just learned" | `brain new knowledge "Dolt FK constraints are lazy until commit"` | New brain doc, kind=knowledge, markdown file appears at `entries/knowledge/<slug>.md` |
| "create a new brain task to ship the FTS5 indexer" | `brain new task "ship the FTS5 indexer"` | New brain doc, kind=task, status=open, eligible for `brain ready` |
| "based on that brain doc, let's turn that into tasks" | `brain recast B-a7b3c --to=task` (or `brain new task ...` + `brain link --extends B-a7b3c`) | The knowledge doc becomes a task in place — same ID, same edges. Or a new task is created and linked back. Choice is Trillium's, both supported. |
| "what's next re: brain" | `brain ready` (limited to kind ∈ {task, both}) | Standard bd ready, filtered by kind — surfaces open tasks with satisfied deps |
| "show me everything connected to that thing I learned about Tailscale" | `brain related K-552a --depth=2` | Graph BFS from the center node, printed as an indented tree, kinds mixed |
| "I jotted this down five minutes ago but it should really be a task" | `brain recast <id> --to=task` | One column flip, every existing edge preserved |
| "save this thought, I'll figure out if it's a task later" | `brain new both "active investigation: why is launchd quarantining the resolver"` | kind=both — appears in both task lists and knowledge lists until you decide |

Every right-hand-column verb is defined in §4 below.

---

## 4. The verbs brain adds (in detail)

These are the four verbs that did not exist on bd. Each is registered through the `BrainVerb` seam (Decision #5, `divergence/0003`, landed `5149a9e53`) and dispatched by the same Cobra root that handles bd's verbs.

> **Naming note on `brain new`:** the kind is a required positional argument (`brain new <kind> <title>`), not a flag. This matches the voice pattern Trillium actually uses ("brain new task ...", "brain new knowledge ..."), matches ISA ISC-104/105/106 which test exactly that signature, and removes a class of ambiguity — there is no "smart default" to second-guess.

### 4.1 `brain new <kind> <title>`

**One-liner:** create a brain doc with an explicit kind.

```
$ brain new task "ship the FTS5 indexer"
created: B-9c12a
   kind: task
 status: open
  title: ship the FTS5 indexer

$ brain new knowledge "Dolt FK constraints are lazy until commit"
created: B-a7b3c
   kind: knowledge
  title: Dolt FK constraints are lazy until commit

$ brain new both "active investigation: launchd quarantine of resolver"
created: B-d4f88
   kind: both
 status: open
  title: active investigation: launchd quarantine of resolver
```

Kind is required. There is no implicit default. If kind is omitted brain exits 2 with `error: kind is required, must be one of task|knowledge|both`.

**Why required (not defaulted):**
- The voice pattern Trillium uses already speaks the kind ("brain new task ...", "brain new knowledge ...").
- ISA ISC-104/105/106 test the explicit form.
- A silent default is a guess; the cost of getting kind wrong is having to `brain recast` later. Cheaper to ask once at creation.
- Alternatives considered: (a) default kind=knowledge — rejected, conflicts with ISA tests; (b) smart-detect from flags — rejected, adds magic; (c) `brain new` as alias for `bd create` — rejected, loses the kind discriminator at the most ergonomic verb.

**Scenarios:**

**Scenario: create a new knowledge doc mid-conversation**
- **Given** no brain doc currently exists for the Dolt FK quirk
- **When** Trillium says "create a new brain doc to track that FK constraint thing I just learned" → `brain new knowledge "Dolt FK constraints are lazy until commit"`
- **Then** a brain doc with kind=knowledge is inserted into Dolt with a fresh ID; the markdown file `entries/knowledge/dolt-fk-constraints-are-lazy-until-commit.md` is written by the exfiltrator with frontmatter mirroring the row and the title as the H1; stdout prints `created: B-<id>` and the row count.

**Scenario: create a task from a phone**
- **Given** Trillium is on his phone, SSH'd into the laptop via Blink+mosh+tmux
- **When** he types `brain new task "audit other cgroup-isolated services for DNS"`
- **Then** kind=task, status=open, no priority unless `--priority` was passed; appears in the next `brain ready` invocation if no blockers are linked.

**Scenario: create a "both" because the work and the lesson are inseparable**
- **Given** Trillium is mid-investigation and the postmortem is going to live next to the work
- **When** he types `brain new both "Friday cache bug + postmortem"`
- **Then** kind=both; shows up in `brain list --kind=task`, `brain list --kind=knowledge`, AND `brain ready` (because both qualifies as task-like for the queue).

**Scenario: missing kind argument**
- **Given** Trillium types `brain new "thought I had on the train"` without a kind
- **When** Cobra parses the args
- **Then** brain exits 2 with `error: kind is required, must be one of task|knowledge|both`; nothing is written; no markdown file is created.

**Scenario: invalid kind value**
- **Given** Trillium types `brain new note "..."`
- **When** the verb validates the kind argument
- **Then** brain exits 2 with `error: invalid kind "note", must be one of task|knowledge|both`; the typed-enum guard from ISC-102 catches this before any storage call.

---

### 4.2 `brain link <from> <to> [--edge-type]`

**One-liner:** create an edge between two brain docs, with brain's knowledge-edge vocabulary as first-class flags.

```
$ brain link B-a7b3c B-552a --extends
linked: B-a7b3c —[extends]→ B-552a

$ brain link B-a7b3c B-217 --learned-from
linked: B-a7b3c —[learned-from]→ B-217

$ brain link B-a7b3c B-100 --related            # bd's edge types still available
linked: B-a7b3c —[related]→ B-100
```

`brain link` writes to the same `dependencies` table that `bd dep add` writes to. The two surfaces produce identical rows. `brain link` exists because the flag spelling (`--extends`, `--learned-from`) reads more naturally for knowledge-graph work than `--type extends`.

**Scenarios:**

**Scenario: link a new insight to the work that produced it**
- **Given** brain doc `B-a7b3c` (knowledge: "Dolt FK constraints are lazy") exists; brain doc `B-217` (closed task: "Pulse module wouldn't reach mini2") exists
- **When** Trillium says "link that insight to the bug that taught me" → `brain link B-a7b3c B-217 --learned-from`
- **Then** a row `(from=B-a7b3c, to=B-217, type=learned-from)` is inserted into the edges table; the markdown exfiltrator updates both docs' frontmatter `links` arrays.

**Scenario: extend a prior knowledge doc**
- **Given** `B-552a` ("How tsnet picks DNS") exists; new doc `B-a7b3c` is a deeper-cut follow-up
- **When** Trillium runs `brain link B-a7b3c B-552a --extends`
- **Then** the edge is created; `brain related B-552a` now shows `B-a7b3c` as an incoming `extends` neighbor.

**Scenario: link using bd's edge types from the brain surface**
- **Given** two task brain docs `B-218`, `B-219`
- **When** Trillium runs `brain link B-218 B-219 --type blocks` (falling through to bd's edge-type flag)
- **Then** the row is inserted with `type=blocks`; `brain ready` correctly skips `B-218` while `B-219` is open.

**Scenario: linking nonexistent ID**
- **Given** `B-DOESNT` is not a real brain doc
- **When** Trillium runs `brain link B-a7b3c B-DOESNT --related`
- **Then** brain exits non-zero with `error: target brain doc B-DOESNT not found`; no edge row is written.

---

### 4.3 `brain related <id> [--depth=N]`

**One-liner:** walk the graph from a center brain doc and print the subgraph as an indented tree.

This is the only brain-added verb that has no bd analogue. `bd dep list <id>` prints a flat one-hop table. `brain related` does BFS with a depth cap.

```
$ brain related B-a7b3c --depth=2

B-a7b3c · Dolt FK constraints are lazy until commit          [kind=knowledge]
│
├─[extends]→ B-552a · How tsnet picks DNS                    [kind=knowledge]
│            │
│            └─[extends]→ B-300 · DNS resolver overview       [kind=knowledge]
│
├─[learned-from]→ B-217 · Pulse module wouldn't reach mini2   [kind=task, closed]
│                 │
│                 └─[caused-by]→ B-200 · launchd quarantine   [kind=task, closed]
│
└─[related]→ B-100 · DNS layering in macOS netext             [kind=knowledge]
             │
             ├─[extends]→ B-50 · netext lifecycle              [kind=knowledge]
             └─[related]→ B-77 · DNS hijacking detection       [kind=knowledge]
```

Default depth is 2. `--depth=1` shows direct neighbors only (equivalent to `bd dep list <id>` with brain's rendering). `--depth=0` shows just the center node.

**Scenarios:**

**Scenario: "what do I know about Tailscale?"**
- **Given** several brain docs about Tailscale exist, linked by `extends`, `related`, `learned-from`
- **When** Trillium runs `brain related B-552a --depth=3`
- **Then** the indented tree prints every reachable neighbor within 3 hops, kinds mixed, edge type labeled on each branch.

**Scenario: phone-driven exploration**
- **Given** Trillium is on his phone over Tailscale, SSH'd into the laptop
- **When** he types `brain related B-a7b3c` (no depth flag)
- **Then** default depth=2 BFS prints; output fits comfortably on a phone screen because depth is bounded.

**Scenario: orphan node**
- **Given** `B-orph` has no edges
- **When** Trillium runs `brain related B-orph`
- **Then** the output is the one-line header for `B-orph` followed by `(no neighbors)`; exit 0.

**Scenario: cycle in the graph**
- **Given** edges form a cycle: `B-a → extends → B-b → extends → B-a`
- **When** Trillium runs `brain related B-a --depth=10`
- **Then** the BFS visited-set guarantees each node prints once; the cycle is annotated `(already visited)` on the second appearance.

**Scenario: nonexistent center**
- **Given** `B-NOPE` is not a real brain doc
- **When** Trillium runs `brain related B-NOPE`
- **Then** brain exits non-zero with `error: brain doc B-NOPE not found`.

---

### 4.4 `brain recast <id> --to=<kind>`

**One-liner:** change the kind of an existing brain doc in place. Every edge survives. Every comment survives. Same ID.

This is the verb behind "let's turn that brain doc into tasks." A knowledge doc becomes a task by `brain recast B-a7b3c --to=task`. A `both` collapses to a `task` (or `knowledge`) the same way.

```
$ brain show B-a7b3c
B-a7b3c · Dolt FK constraints are lazy until commit
   kind: knowledge   created: 2026-05-31

$ brain recast B-a7b3c --to=task
recast: B-a7b3c  knowledge → task
status: open (defaulted on kind transition)
edges:  3 preserved (B-552a, B-217, B-100)

$ brain show B-a7b3c
B-a7b3c · Dolt FK constraints are lazy until commit
   kind: task   status: open   created: 2026-05-31
```

**Why `recast` and not `promote`:**

bd already ships `bd promote` (and `bd mol wisp promote`) for wisp → bead graduation — a different, narrower operation. To stay out of that namespace collision, brain's kind-shift verb is `brain recast`. The word also matches the operation precisely: we are recasting the doc into a different role. `brain promote` was the natural-language candidate, but namespacing matters more than reading-naturalness for a verb that will appear hundreds of times in shell history.

If you reach for `brain promote` out of habit, brain prints a hint:
```
$ brain promote B-a7b3c
error: did you mean `brain recast B-a7b3c --to=<kind>`?
       `bd promote` graduates wisps to beads; `brain recast` shifts kind.
```

**Scenarios:**

**Scenario: knowledge → task ("let's turn that into a task")**
- **Given** `B-a7b3c` is kind=knowledge with three outgoing edges (extends, learned-from, related)
- **When** Trillium says "based on that brain doc, let's turn that into a task" → `brain recast B-a7b3c --to=task`
- **Then** the row updates `issue_type='task'`, defaults `status='open'`, preserves all three edges, preserves the body and ID; markdown file at `entries/knowledge/<slug>.md` is moved to `entries/task/<slug>.md` by the exfiltrator on the next idempotent sync.

**Scenario: misclassification recovery**
- **Given** Trillium created `B-9c12a` as kind=task five minutes ago, but realized it's a research note, not work
- **When** he runs `brain recast B-9c12a --to=knowledge`
- **Then** kind flips; the `status` column is preserved but no longer participates in `brain ready` (kind=knowledge is excluded); the markdown file relocates from `entries/task/` to `entries/knowledge/`.

**Scenario: knowledge → both (active investigation)**
- **Given** `B-a7b3c` is kind=knowledge but Trillium has decided he will work on it
- **When** he runs `brain recast B-a7b3c --to=both`
- **Then** kind becomes `both`; the doc now appears in `brain list --kind=task`, `brain list --kind=knowledge`, AND `brain ready` (if status=open and deps satisfied).

**Scenario: idempotent recast**
- **Given** `B-a7b3c` is already kind=task
- **When** Trillium runs `brain recast B-a7b3c --to=task`
- **Then** exit 0 with `no-op: B-a7b3c already kind=task`; no write; no markdown churn.

**Scenario: invalid target kind**
- **Given** Trillium runs `brain recast B-a7b3c --to=archived`
- **When** the verb validates `--to`
- **Then** exit 2 with `error: invalid target kind "archived", must be one of task|knowledge|both`.

---

## 5. Every bd verb is reachable as `brain <verb>`

There is no "use bd for that." Because the binary answers to its installed name, every bd verb is a brain verb. The ones below are the ones knowledge-graph workflows hit most:

| You type | What it does | Notes |
|---|---|---|
| `brain create -t "X"` | Create a brain doc (this is the bd-style create) | `brain new <kind> "X"` is the brain-flavored verb that REQUIRES kind; `brain create` keeps bd's flag set and defaults |
| `brain show <id>` | Display a brain doc | Same code as `bd show`; rendering is kind-aware (knowledge docs group edges by edge-type, task docs by status) |
| `brain list` | List brain docs | Supports `--kind=task|knowledge|both`, plus all bd `list` flags (status, priority, label) |
| `brain ready` | Surface ready-to-work brain docs | Filtered to kind ∈ {task, both} with status=open and no blockers — **literally `bd ready`, just on the renamed binary, with kind filtering applied as a default** |
| `brain close <id>` | Close a task brain doc | Same as `bd close`; works on kind=task or kind=both |
| `brain update <id>` | Update fields on a brain doc | Same as `bd update` |
| `brain dep add <a> <b>` | Add an edge (bd-style) | Same write as `brain link`; the two are vocabulary aliases |
| `brain dep list <id>` | One-hop edge list | Flat-table sibling of `brain related --depth=1` |
| `brain prime` | Pre-flight checks before tracked work | Same as `bd prime` |
| `brain bootstrap` | Initialize a new brain DB | Same as `bd bootstrap` |
| `brain dolt push/pull` | Sync the Dolt DB | Same as `bd dolt push/pull` |
| `brain duplicates` | Detect duplicate brain docs | Same as `bd duplicates` |
| `brain mol wisp ...` | Ephemeral capture | Wrapped by `brain jot` alias namespace (ISC-251) for ergonomics |

None of these are "shims" or "wrappers." They are the exact same Cobra subcommands the bd binary exposes, available because the binary is renamed.

---

## 6. The `kind` discriminator — the only schema-shaped new idea

bd's `issues.issue_type` column is `TEXT`. bd writes `"task"` there. brain teaches the CLI three values:

```
                  issues.issue_type  (bd's existing TEXT column)
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
     "task"          "knowledge"          "both"
        │                 │                 │
   default for       written by         written by
   `brain create`    `brain new        `brain new
   and bd's existing knowledge ...`     both ...`
   verbs that
   create rows
        │                 │                 │
   appears in:       appears in:        appears in:
   - brain ready     - brain list       - all of the above
   - brain list      - brain related      (both qualifies
   - brain related     traversals          as task-shaped
     traversals      - NOT in            for ready/queue)
                       brain ready
```

Concrete behavior:

```
$ brain create -t "Cache invalidation breaks on Friday deploys"
   → issue_type = "task"           appears in `brain ready`

$ brain new knowledge "Dolt FK constraints are lazy until commit"
   → issue_type = "knowledge"      NOT in `brain ready`

$ brain new both "Friday cache bug fix + the postmortem"
   → issue_type = "both"           appears in both views, and in `brain ready`
```

**No new column. No migration.** The only code change is a typed-enum guard in the verb layer (ISC-102) that rejects values outside `{task, knowledge, both}` when written through brain-added verbs. bd-existing verbs continue to default `task` as they always have.

---

## 7. The BrainVerb seam — narrowed interpretation

Decision #5 (`divergence/0003`, landed `5149a9e53`) names five seams. `BrainVerb` is the first one. **It is narrower than the prior framing implied.**

**What `BrainVerb` is the seam for:** the verbs brain ADDS to the binary — `new`, `link`, `related`, `recast`, `jot`, `legacy`, plus any future additions. The interface contract is `Run(ctx, args) (result, error)`. The Cobra wrapper at `cmd/bd/brain_<verb>.go` is the only place that knows the concrete `Args`/`Result` types; the engine package at `internal/brain/verb/<verb>/` does the work.

**What `BrainVerb` is NOT the seam for:** bd's existing verbs (`create`, `show`, `list`, `ready`, `close`, `update`, `dep add`, `prime`, `bootstrap`, ...). Those verbs do not go through `BrainVerb`. They go through their own Cobra files at `cmd/bd/<verb>.go` and call `internal/storage` directly, exactly as they do today. The renamed binary answers to `brain <verb>` for them not because there is a routing layer, but because Cobra's subcommand dispatch doesn't care what the binary is called — it looks up `<verb>` in the registered subcommand tree.

If `BrainVerb` ever needs to swap, only the brain-added verbs feel it. bd's existing verbs are insulated by the fact that they don't depend on `BrainVerb` at all.

---

## 8. The plumbing diagram — one whole-system view

```
                          user types `brain <verb>`
                                     │
                                     ▼
                          ┌────────────────────┐
                          │  Cobra root        │
                          │  (cmd/bd/main.go)  │
                          │  binary-name-aware │
                          └─────────┬──────────┘
                                    │
                  ┌─────────────────┴─────────────────┐
                  │                                   │
                  ▼                                   ▼
        bd-existing verb path             brain-added verb path
        (create, show, list, ready,       (new, link, related,
         close, update, dep, prime,        recast, jot, legacy)
         bootstrap, dolt, ...)
                  │                                   │
                  │                                   ▼
                  │                       ┌─────────────────────────┐
                  │                       │ BrainVerb interface     │
                  │                       │ internal/brain/verb/    │
                  │                       │ (Decision #5 seam)      │
                  │                       └────────────┬────────────┘
                  │                                    │
                  └─────────────────┬──────────────────┘
                                    ▼
                          ┌──────────────────────┐
                          │ internal/storage     │  ← THE SHARED SUBSTRATE
                          │ (bd's primitives,    │     same call shape from
                          │  Dolt-backed)        │     both verb paths
                          └──────────┬───────────┘
                                     │
                      ┌──────────────┼──────────────┐
                      ▼              ▼              ▼
              ┌────────────┐  ┌────────────┐  ┌────────────────┐
              │ Dolt write │  │ Dolt read  │  │ Hook fire      │  ← brain's
              │ (INSERT/   │  │ (SELECT)   │  │ (HookFiring-   │     exfiltrator
              │  UPDATE)   │  │            │  │  Store         │     attaches
              └────────────┘  └────────────┘  │  decorator)    │     here
                                              └────────┬───────┘
                                                       │
                                                       ▼
                                              ┌──────────────────┐
                                              │ Exfiltrator      │  ISC-117-121,
                                              │ Render(node)     │  not yet built
                                              │ ↓                │
                                              │ ~/data/knowledge │
                                              │ /entries/{kind}/ │
                                              │ {id}.md          │
                                              └──────────────────┘
```

Three things this diagram makes load-bearing:

1. **bd-existing-verbs and brain-added-verbs both terminate at the same `internal/storage` layer.** They are not parallel write paths; they are siblings sharing a back-end.
2. **`BrainVerb` is a seam, not a router.** Only brain-added verbs pass through it. bd-existing verbs route directly from Cobra to storage.
3. **Exfiltration is a decorator on bd's existing `HookFiringStore`, not a parallel write path.** When the Exfiltrator lands, it intercepts the same writes bd already fires hooks on. bd's own behavior does not change.

---

## 9. The 18-edge-type vocabulary

bd's 16 well-known + brain's 2 new = 18 total in `internal/types/types.go` (the slice `WellKnownDependencyTypes()`):

```
bd's 16 (authoritative for tasks AND knowledge — brain didn't replace these):
  blocks, parent-child, conditional-blocks, waits-for, related,
  discovered-from, replies-to, relates-to, duplicates, supersedes,
  authored-by, assigned-to, approved-by, attests, tracks, until,
  caused-by, validates, delegated-from

brain's 2 (knowledge-graph additions, landed c4b6a78e4):
  extends           "this brain doc builds on that one"
  learned-from      "this insight came from that work"
```

Any of the 18 is usable from any verb that creates edges (`brain link`, `brain dep add`, etc.) on either flag-spelling. brain didn't replace the bd edge types; it registered two more values in the same slice.

---

## 10. Cross-cutting scenarios

These scenarios touch multiple primitives and demonstrate the "one bag of brain docs" model in action.

**Scenario: knowledge → task promotion preserving edges**
- **Given** `B-a7b3c` is kind=knowledge with edges (`extends → B-552a`, `learned-from → B-217`, `related → B-100`)
- **When** Trillium says "based on that brain doc, let's turn that into a task" → `brain recast B-a7b3c --to=task`
- **Then** `B-a7b3c.issue_type` is updated to `task`; all three edges remain (same `from_id`, same `to_id`, same edge types — nothing in the edges table references the kind column); `status` defaults to `open`; `brain related B-552a` and `brain related B-217` and `brain related B-100` all still surface `B-a7b3c` as a neighbor; the markdown file is moved from `entries/knowledge/<slug>.md` to `entries/task/<slug>.md` by the exfiltrator on the next sync.

**Scenario: closed task → knowledge insight (the postmortem path)**
- **Given** `B-217` is kind=task, status=closed (the Pulse-mini2 DNS bug got fixed)
- **When** Trillium runs `brain new knowledge "What I learned debugging the launchd quarantine"` → returns `B-d9e22`; then `brain link B-d9e22 B-217 --learned-from`
- **Then** the closed task and the new insight are bidirectionally reachable through `brain related`; the closed task still doesn't appear in `brain ready` (status=closed); the insight is searchable as kind=knowledge.

**Scenario: mid-conversation capture as `both`**
- **Given** Trillium is mid-debugging and isn't sure yet whether what he's typing is research or work
- **When** he runs `brain new both "active investigation: why does launchd quarantine the resolver"`
- **Then** kind=both, status=open; appears in `brain list --kind=task`, `brain list --kind=knowledge`, AND `brain ready`; later, when the investigation crystallizes, `brain recast B-d4f88 --to=task` (work to do) or `--to=knowledge` (lesson learned, no further work) collapses it cleanly.

**Scenario: phone-driven "what's next re: brain"**
- **Given** Trillium is on his phone, SSH'd into the laptop via Blink+mosh+tmux
- **When** he types `brain ready --top=5`
- **Then** `brain ready` (which is `bd ready` on the renamed binary) returns the top 5 brain docs where kind ∈ {task, both}, status=open, and no `blocks` or `waits-for` dependency is unresolved; output fits on the phone screen because the kind filter and --top cap the result set.

**Scenario: misclassification recovery (kind wrong at creation)**
- **Given** Trillium created `B-9c12a` as kind=knowledge yesterday, but every time he sees it he thinks "I should be working on this, not reading about it"
- **When** he runs `brain recast B-9c12a --to=task`
- **Then** the row updates in place; ID is preserved (no broken links from any doc that referenced `B-9c12a`); the markdown file relocates; next `brain ready` invocation includes `B-9c12a` if status=open and no blockers.

**Scenario: the question "what do I know about X?"**
- **Given** several brain docs about Tailscale DNS exist, mixed kinds, linked by `extends`, `learned-from`, `related`
- **When** Trillium runs `brain related B-552a --depth=3` (where `B-552a` is the deepest tsnet-DNS knowledge doc)
- **Then** the printed tree spans tasks and knowledge alike — the closed task that triggered the insight, the knowledge doc that explains the mechanism, the related insight about macOS network extensions — all from one verb because they all live in one bag of brain docs.

---

## 11. Quick reference card

```
   verb                      example                                        notes
  ───────────────────────   ───────────────────────────────────────────   ─────────────────────────────
   brain new <kind> "X"      brain new task "ship the FTS5 indexer"        kind required: task|knowledge|both
   brain link a b --T        brain link B-a7b3c B-217 --learned-from       18 edge types; --extends, --learned-from new
   brain related <id>        brain related B-a7b3c --depth=2               BFS tree, depth-capped
   brain recast <id>         brain recast B-a7b3c --to=task                kind change in place; preserves ID + edges

   brain ready               brain ready --top=5                            same code as bd ready; filtered to task/both
   brain show <id>           brain show B-a7b3c                             same code as bd show; kind-aware rendering
   brain list                brain list --kind=knowledge                    same code as bd list; supports --kind
   brain close <id>          brain close B-a7b3c                            same code as bd close
   brain update <id>         brain update B-a7b3c --priority=1              same code as bd update
   brain create -t "X"       brain create -t "untyped doc"                  same code as bd create; defaults kind=task
   brain dep add a b -T t    brain dep add B-a B-b --type extends           same write as brain link
   brain prime               brain prime                                    same as bd prime
   brain bootstrap           brain bootstrap                                same as bd bootstrap
   brain dolt push/pull      brain dolt push                                same as bd dolt push/pull
   brain jot save "X"        brain jot save "thought, figure out later"     wisp alias namespace (ISC-251)

   one binary               renamed to `brain` at install time              answers to both `bd` and `brain`
   one schema               kind ∈ {task, knowledge, both} on issue_type     no migration
   one edge table           18 edge types (16 bd + 2 brain)                  brain's: extends, learned-from
   one exfiltration hook    HookFiringStore decorator (ISC-117-121)          writes entries/{kind}/{id}.md, not yet built
```

---

## 12. Where to go next

- The architectural decision that named the BrainVerb seam: `divergence/0003-modularity-first.md`
- The seam itself: `internal/brain/verb/verb.go` and its tests
- The brain parent Cobra command: `cmd/bd/brain.go`
- The reframe this doc lands: `divergence/0006-brain-primitives-reframe.md`
- The canonical brain spec: `../../ISA.md` — problem, vision, ISC table, decision log
- The full divergence trail: `../../divergence/`
