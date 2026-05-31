# brain vs bd — what's actually different

> **Audience:** someone who already knows bd and wants to know what brain adds and why.
> **TL;DR:** brain is bd with five new CLI verbs and one new column-flavored idea (`kind`). Storage is the same Dolt database; edges use the same edge-type vocabulary; tasks still work the way bd works. brain just gives knowledge-graph users a more comfortable verb set on top, plus a markdown-exfiltration hook bd never had.

---

## 1. The 30-second answer

brain does **not** reimplement bd.

- `brain new`, `brain show`, `brain list`, `brain link`, `brain related` are **thin Cobra wrappers** that route through one Go interface (`BrainVerb`) and call the **same `internal/storage` primitives** bd's own commands call.
- The only **brand-new** behaviors are (a) defaulting `kind=knowledge` on creation, (b) printing output that groups edges by type instead of by status, and (c) a markdown side-effect on every mutation (the `Exfiltrator` hook, not yet built).
- bd's existing commands (`bd create`, `bd show`, `bd list`, `bd dep add`, `bd ready`, `bd close`, `bd update`, ...) still exist and still work. brain coexists. You can use either vocabulary against the same Dolt store.

If you remember nothing else: **brain is a verb-vocabulary lens over bd, not a fork of bd's data model.**

---

## 2. The mental-model shift

bd was built for tracking **work**. brain extends that for tracking **knowledge** in the same store.

```
        bd's world                        brain's world
        (work queue)                      (knowledge graph)

   ┌──────────────────┐               ┌──────────────────┐
   │ status=open      │               │       ●          │
   │ ────────────────  │               │       │          │
   │ [task] BD-21     │               │  ●────┴────●     │
   │ [task] BD-22 *   │               │  │ extends    │  │
   │ [task] BD-23 ◯   │               │  ●     ●─────●  │
   │ [task] BD-24 ✓   │               │  related        │
   │ ────────────────  │               │                  │
   │ ready: 2 tasks   │               │  what radiates   │
   └──────────────────┘               │  from this node? │
                                      └──────────────────┘
   "what's next?"                     "what do I know?"

   bd ready, bd list                  brain related <id>, brain list --kind=knowledge
```

bd's center of gravity is the **next task**. brain's center of gravity is the **node and its edges**. Same database, different question being asked of it.

---

## 3. The one new idea: `kind` (task | knowledge | both)

bd already has a column called `issue_type` on the `issues` table. bd stores `"task"` there. brain teaches the CLI three values for that column:

```
                  issues.issue_type  (TEXT, bd's existing column, no migration)
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
     "task"          "knowledge"          "both"
        │                 │                 │
   bd default      brain new default   brain new --kind=both
   appears in:     appears in:         appears in:
   - bd ready      - brain related     - both lists
   - bd list       - brain list -k=k   - the answer to
   - brain list      (knowledge view)    "this work item also
     -k=task                              taught me something"
```

**Concrete example:**

```
$ bd create -t "Cache invalidation breaks on Friday deploys"
   → issue_type = "task"        appears in `bd ready`

$ brain new "Dolt FK constraints are lazy until commit"
   → issue_type = "knowledge"   appears in `brain list`, NOT in `bd ready`

$ brain new "Friday cache bug fix + the postmortem" --kind=both
   → issue_type = "both"        appears in both views
```

That is the **entire** schema-level change. No new column, no migration. The migration that landed was just adding `extends` and `learned-from` to the `WellKnownDependencyTypes()` slice in `internal/types/types.go`.

---

## 4. The five verbs, side by side

### Command map

| What you want to do | bd command | brain command | Real difference |
|---|---|---|---|
| Create something | `bd create -t "X"` | `brain new "X"` | brain defaults `kind=knowledge` and (eventually) writes a markdown file |
| Look at one thing | `bd show ABC-1` | `brain show ABC-1` | brain groups output by edge-type instead of status/priority; kind-aware rendering |
| List many things | `bd list` | `brain list` | brain filters by `kind` instead of status; default view is `kind=knowledge` |
| Connect two things | `bd dep add A B --type extends` | `brain link A B --extends` | Same op, different idiom. Both write to the same `dependencies` table |
| Walk the graph from a node | *(no equivalent)* | `brain related ABC-1` | Brand new. bd has `bd dep list` (one-hop, table-shaped) but no graph traversal verb |
| See what's ready to work | `bd ready` | *(still `bd ready`)* | brain does NOT reimplement this. Use bd. |
| Close a task | `bd close ABC-1` | *(still `bd close`)* | brain does NOT reimplement this. Use bd. |
| Update fields | `bd update ABC-1` | *(still `bd update`)* | brain does NOT reimplement this. Use bd. |

### What brain explicitly chose NOT to add

`bd ready`, `bd close`, `bd update`, `bd duplicates`, `bd dolt push`, `bd dolt pull`, `bd prime`, `bd bootstrap`, `bd mol wisp`, `bd promote` — all of these are **bd's job**. brain doesn't shadow them and doesn't intercept them. If you're managing tasks, you're still in bd-land; brain just gave the knowledge-graph use case a friendlier surface.

---

## 5. Each verb in detail

### `brain new`

**One-liner:** create a knowledge node by default (or task, or both).

```
$ brain new "Tailscale MagicDNS won't resolve in cgroup-isolated processes"
created: K-a7b3c
   kind: knowledge
  title: Tailscale MagicDNS won't resolve in cgroup-isolated processes
```

**vs bd:**

```
$ bd create -t "Fix tailscale resolution in container"
created: BD-42
   type: task                      ← bd implicit default
 status: open
```

**Under the hood:**

```
$ brain new "..."
        │
        ▼
   ┌────────────────────────┐
   │  cmd/bd/brain_new.go   │  Cobra wrapper
   │  parses --kind, --tag  │  knows the concrete Args/Result types
   └────────────────────────┘
        │ delegates via BrainVerb interface
        ▼
   ┌────────────────────────┐
   │ internal/brain/verb/   │  engine: Run(ctx, args)
   │ new/new.go             │  defaults kind=knowledge if unset
   └────────────────────────┘
        │ calls bd primitive (no parallel storage)
        ▼
   ┌────────────────────────┐
   │ internal/storage/      │  bd's existing Create()
   │ (Dolt INSERT)          │  same call bd create uses
   └────────────────────────┘
        │ HookFiringStore decorator
        ▼
   ┌────────────────────────┐
   │ Exfiltrator.Render()   │  brain-only seam (later tranche)
   │ writes markdown file   │  ~/data/knowledge/entries/{kind}/{id}.md
   └────────────────────────┘
```

The `BrainVerb` boundary is the swap point: if `internal/storage/Create()` ever changes signature, only `internal/brain/verb/new/new.go` cares — `cmd/bd/brain_new.go` stays untouched.

---

### `brain show`

**One-liner:** show one node, with edges grouped by edge-type (not status/priority).

```
$ brain show K-a7b3c

K-a7b3c · Tailscale MagicDNS won't resolve in cgroup-isolated processes
kind: knowledge   created: 2026-05-31

Description
-----------
Looking at /etc/resolv.conf inside the cgroup'd process shows ...

Edges
-----
extends:
  K-552a   "How Tailscale's tsnet resolver picks DNS"
learned-from:
  BD-217   "[closed] Pulse module wouldn't reach mini2.tail-xyz.ts.net"
related:
  K-100    "DNS layering in macOS network extensions"
```

**vs bd:**

```
$ bd show BD-217

BD-217 · Pulse module wouldn't reach mini2.tail-xyz.ts.net
type: task    status: closed    priority: 2

Description
-----------
...

Dependencies
------------
blocks: BD-218
related: BD-200
parent-child: BD-150
```

Same query under the hood — both call bd's storage layer for the issue + its dependencies. The difference is the rendering: bd groups for **project management** (what's blocking what), brain groups for **knowledge navigation** (what does this build on, what did I learn from).

---

### `brain list`

**One-liner:** list nodes, filtered and grouped by `kind` instead of `status`.

```
$ brain list                              # default kind=knowledge
K-a7b3c  Tailscale MagicDNS won't resolve in cgroup-isolated processes  [3 edges]
K-552a   How Tailscale's tsnet resolver picks DNS                       [1 edge]
K-100    DNS layering in macOS network extensions                       [4 edges]

$ brain list --kind=both
K-99a    Cache invalidation breaks on Friday deploys + postmortem       [closed, 2 edges]

$ brain list --kind=task                  # equivalent to `bd list`
BD-217   [closed] Pulse module wouldn't reach mini2.tail-xyz.ts.net
BD-218   [open]   Audit other cgroup-isolated services for DNS
```

**vs bd:**

```
$ bd list
BD-217   [closed] Pulse module wouldn't reach mini2.tail-xyz.ts.net   pri:2
BD-218   [open]   Audit other cgroup-isolated services for DNS         pri:1
BD-219   [open]   Document tsnet DNS quirks                            pri:3
```

Same underlying `internal/storage` list query. brain just adds a `WHERE issue_type IN (...)` filter and changes column priorities.

---

### `brain link`

**One-liner:** create an edge between two nodes, with brain's knowledge-edge vocabulary as first-class flags.

```
$ brain link K-a7b3c K-552a --extends
linked: K-a7b3c —[extends]→ K-552a

$ brain link K-a7b3c BD-217 --learned-from
linked: K-a7b3c —[learned-from]→ BD-217

$ brain link K-a7b3c K-100 --related            # bd's original edge types still available
linked: K-a7b3c —[related]→ K-100
```

**vs bd:**

```
$ bd dep add K-a7b3c K-552a --type extends
added dependency: K-a7b3c → K-552a (type=extends)

$ bd dep add K-a7b3c BD-217 --type learned-from
added dependency: K-a7b3c → BD-217 (type=learned-from)
```

**Same write.** `brain link` is a vocabulary alias for `bd dep add`. The difference is that brain's knowledge-graph edges (`--extends`, `--learned-from`) are first-class boolean flags, while bd's project-management edges (`--blocks`, `--parent-child`) are also available but you'd typically reach for `bd dep add` for those.

#### The full edge-type vocabulary

bd's 16 well-known + brain's 2 new ones = 18 total in `internal/types/types.go` line ~821:

```
bd's 16 (still authoritative for tasks):
  blocks, parent-child, conditional-blocks, waits-for, related,
  discovered-from, replies-to, relates-to, duplicates, supersedes,
  authored-by, assigned-to, approved-by, attests, tracks, until,
  caused-by, validates, delegated-from

brain's 2 (knowledge-graph additions, landed c4b6a78e4):
  extends           "this builds on that"
  learned-from      "this insight came from that work"
```

You can use any of the 18 from either CLI surface. brain didn't replace anything; it just registered two more values in the same slice.

---

### `brain related`

**One-liner:** walk the graph from a center node and show what radiates out.

This is the **only** brain verb that has no bd equivalent. bd has `bd dep list <id>` which prints a flat table of one-hop edges. `brain related` is a graph traversal.

```
$ brain related K-a7b3c --depth=2

K-a7b3c · Tailscale MagicDNS won't resolve in cgroup-isolated processes
│
├─[extends]→ K-552a · How Tailscale's tsnet resolver picks DNS
│            │
│            └─[extends]→ K-300 · DNS resolver architecture overview
│
├─[learned-from]→ BD-217 · [closed] Pulse module wouldn't reach mini2.tail-xyz.ts.net
│                 │
│                 └─[caused-by]→ BD-200 · [closed] launchd quarantine of resolver
│
└─[related]→ K-100 · DNS layering in macOS network extensions
             │
             ├─[extends]→ K-50 · macOS network extension lifecycle
             └─[related]→ K-77 · DNS hijacking detection on macOS
```

**Why this matters:** when you ask "what do I already know about X?", you don't want a flat list — you want the tree. bd's task-tracking view doesn't need this (tasks rarely radiate; they queue). brain's knowledge-graph view does. This is the single verb that justifies brain having its own CLI namespace.

**Under the hood:** still calls bd's storage primitives (`GetDependencies()` and `GetIssue()`). The traversal logic lives in `internal/brain/verb/related/related.go` and is a textbook BFS with a depth cap and a visited-set. No new storage interface, no new SQL — just composition.

---

## 6. The plumbing — one diagram, the whole system

```
                     ┌─────────────────────────────────────────┐
                     │ The user types something                │
                     └────────┬──────────────────┬──────────────┘
                              │                  │
                              ▼                  ▼
                    ┌──────────────────┐  ┌──────────────────┐
                    │ bd <verb>        │  │ brain <verb>     │
                    │ (existing)       │  │ (new namespace)  │
                    └────────┬─────────┘  └────────┬─────────┘
                             │                     │
                             │                     ▼
                             │           ┌─────────────────────┐
                             │           │ BrainVerb interface │
                             │           │ internal/brain/verb │
                             │           │ (Decision #5 seam)  │
                             │           └────────┬────────────┘
                             │                    │
                             └──────────┬─────────┘
                                        ▼
                              ┌──────────────────────┐
                              │ internal/storage     │  ← THE SHARED SUBSTRATE
                              │ (bd's primitives,    │     bd primitives that
                              │  Dolt-backed)        │     brain delegates to
                              └──────────┬───────────┘
                                         │
                          ┌──────────────┼──────────────┐
                          ▼              ▼              ▼
                  ┌────────────┐  ┌────────────┐  ┌────────────┐
                  │ Dolt write │  │ Dolt read  │  │ Hook fire  │  ← brain attaches
                  │ (INSERT)   │  │ (SELECT)   │  │ (decorator)│     here for
                  └────────────┘  └────────────┘  └─────┬──────┘     exfiltration
                                                       │
                                                       ▼
                                              ┌──────────────────┐
                                              │ Exfiltrator      │  brain-only seam,
                                              │ Render(node)     │  not built yet
                                              │ ↓                │  (ISC-117-121)
                                              │ ~/data/knowledge │
                                              │ /entries/...md   │
                                              └──────────────────┘
```

Key takeaways from the diagram:

1. **Everything flows through `internal/storage`.** bd commands, brain commands, future engine packages — all the same bottom layer.
2. **`BrainVerb` is the seam, not a fork.** It lets us swap individual verb implementations without touching either the CLI surface or the storage layer.
3. **Exfiltration is a decorator on bd's existing `HookFiringStore`, not a parallel write path.** When it lands (next tranches), it intercepts the same writes bd already fires hooks on. bd stays unaware.

---

## 7. Where the divergence trail lives

Every code-changing commit on brain pairs with a `divergence/NNNN-*.md` doc that records what changed, why, and which ISC criteria it advances. The seam landing (commit `5149a9e53`) is documented in `divergence/0004-brain-verb-seam-and-parent.md`. The verbs themselves will land as `divergence/0005` onward, each with its own rationale and rebase notes.

If you ever rebase brain onto a new bd upstream, the divergence trail tells you which files to resolve `ours` on (brain-only paths) and which need a real merge resolution (rare — only if upstream touches the same line of `internal/types/types.go` we did).

---

## 8. Quick reference card

```
                     bd verb              brain verb              notes
  create something   bd create -t "X"     brain new "X"           brain defaults kind=knowledge
  read one node      bd show ABC-1        brain show ABC-1        brain groups by edge-type
  list many nodes    bd list              brain list              brain filters by kind
  connect two nodes  bd dep add A B -t X  brain link A B --X      same write, vocab alias
  walk graph         (no equiv)           brain related ABC-1     graph BFS w/ depth cap

  task-tracking      bd ready, bd close,  use bd, brain does
                     bd update, bd dolt,  not reimplement these
                     bd prime, ...

  edge types         16 bd + 2 brain (extends, learned-from) = 18, all usable from both surfaces

  storage            single Dolt store at .beads/dolt — bd and brain are two CLIs over one DB

  exfiltration       bd: none             brain: HookFiringStore decorator → markdown
                                          (not yet built, ISC-117-121)
```

---

## Where to go next

- The architectural decision that named the BrainVerb seam: `divergence/0003-modularity-first.md`
- The seam itself: `internal/brain/verb/verb.go` and its tests
- The brain parent Cobra command: `cmd/bd/brain.go`
- The canonical brain spec: `../../ISA.md` (problem, vision, ISC table, decisions)
- The full divergence trail: `../../divergence/`
