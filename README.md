# brain

Personal agent infrastructure. A multi-scope memory and task system that AI agents can depend on, with markdown exfiltration so the same content is also human-readable and grep-friendly.

brain is a Go fork of [beads](https://github.com/gastownhall/beads). It keeps beads' versioned Dolt substrate and graph-shaped issue model, then layers a federation model on top: one binary, many named stores, unified search.

## The model

brain is not one store — it is a family of stores, each with a focused purpose and its own CLI wrapper. Every store is a beads `.beads/` Dolt database. Every wrapper is a thin shell script that sets `BEADS_DIR` and `BD_NAME` before dispatching to the brain binary.

| CLI         | Store path                       | Prefix      | Purpose                                      |
|-------------|----------------------------------|-------------|----------------------------------------------|
| `brain`     | `~/data/knowledge/.beads/`       | `brain-`    | Facts, learnings, concepts — the default hub |
| `task`      | `~/data/tasks/.beads/`           | `task-`     | Actionable work items (global, cross-project)|
| `project`   | `~/data/projects/.beads/`        | `project-`  | Active initiatives; links tasks + decisions  |
| `inbox`     | `~/data/inbox/.beads/`           | `inbox-`    | Frictionless capture — classify later        |
| `decide`    | `~/data/decisions/.beads/`       | `decide-`   | Decisions with rationale, dated              |
| `idea`      | `~/data/ideas/.beads/`           | `idea-`     | Greenfield, exploratory, not yet actionable  |
| `question`  | `~/data/questions/.beads/`       | `question-` | Open questions pending research or answer    |
| `assert`    | `~/data/assertions/.beads/`      | `assert-`   | AI assertion claims + verdicts               |
| `life`      | `~/data/life/.beads/`            | `life-`     | Personal context (health, habits, goals)     |

**brain is the search hub.** `brain search X` finds entries across all registered stores. Writes always go to the specific store via its wrapper — brain is never the write target for task/project/idea etc.

**Capture first, classify later.** Drop anything into `inbox` with zero classification overhead. Promote it to the right store when you have 30 seconds: `brain transfer inbox-abc task`.

**Per-project task tracking stays in the repo.** Each code repo has its own local `bd` store (`.beads/` in the repo). The `task` store is for global, cross-project work items that don't belong to a single codebase.

## Store registry

Stores are registered in `~/.config/pai/stores.yaml`. The registry drives both the binary (for `brain search` federation and `brain transfer`) and shell wrappers (via `~/.config/pai/stores.env`, generated with `brain stores env`).

```sh
brain stores create recipes              # provision new store end-to-end (dolt init, entries/, wrapper, registry, env)
brain stores add task ~/data/tasks/.beads  # register an existing dolt repo without creating files
brain stores list
brain stores env                         # regenerate ~/.config/pai/stores.env
brain stores remove recipes              # unregister (does NOT delete files)
```

`brain stores create` is idempotent — re-running with the same arguments resumes safely if a prior run was interrupted.

## What brain adds to beads

- **Multi-store federation.** `brain stores add/create/list/env`, `brain search` across all stores, `brain transfer` between stores.
- **One-shot store provisioning.** `brain stores create <name>` does dolt init + entries dir + CLI wrapper + registry write + env regen in a single idempotent command — no manual steps.
- **Kind discriminator.** One bag of docs — `kind: task | knowledge | both | isa` — so tasks and knowledge live in the same substrate with the same query layer.
- **Markdown exfiltration on every mutation, every kind.** Every write renders a markdown file to `<store>/entries/<kind>/<slug>.md` — not just the brain trio (task/knowledge/both) but every IssueType bd recognizes (bug, feature, epic, decision, message, etc.). Dolt is canonical; markdown is the human view.
- **Store-derived exfil root.** Resolution is `BRAIN_KNOWLEDGE_ROOT` → `dirname($BEADS_DIR)/entries` → `~/data/brain/entries` — so each store's markdown lands next to its own `.beads/` directory automatically.
- **On-demand re-render.** `bd render <id>` and `bd render-all` re-emit markdown from the substrate when the on-disk copy is missing, corrupted, or out of date. `render-all` prints a confirmation summary (`Exfiltrated N / M beads to <root>/entries/ (K failed)`) and supports `--json` for scripting.
- **ISA primitives.** First-class support for [PAI](https://github.com/danielmiessler/PAI) Algorithm v6.4+ ISAs — `brain new isa`, `brain isa-section`, `brain isa-render`, per-section UPSERT semantics.
- **Auto-file feature requests.** Unknown flag on a `brain` command? It files a feature request automatically and prints the ID.

## How it ships

One Go binary (`~/.local/bin/bd`), many install names. Each wrapper is a shell script:

```sh
#!/bin/sh
export BEADS_DIR="$HOME/data/tasks/.beads"
export BD_NAME="task"
exec "$HOME/.local/bin/bd" "$@"
```

Argv[0] dispatch via `BD_NAME` controls display name and brain-mode behavior.

## Versioning

Double semver: upstream beads version + brain fork version.

```
bd version 1.1.0-rc.1 (brain/0.4.0, abc1234: feat/isa-substrate-f1@abc1234)
```

- **`1.1.0-rc.1`** — upstream beads base the fork is rebased on
- **`brain/0.4.0`** — brain fork version, derived from the most recent `brain/vX.Y.Z` git tag

To cut a release:

```sh
make brain-release BUMP=patch   # tags brain/vX.Y.Z locally
make build && cp bd ~/.local/bin/bd
git push origin brain/v0.4.1    # push when online
```

## Key verbs

```sh
# Capture
brain create --type=knowledge "title" [-d "description"]
task create --title="..." --type=task
inbox create --title="..."           # classify later

# Promote
brain transfer inbox-abc task        # creates task, closes inbox entry

# Search (federated)
brain search "dolt migration"        # searches all registered stores

# Graph
brain link brain-abc task-xyz --type=informs
brain related brain-abc

# ISA (Ideal State Artifacts)
brain new isa "title" --slug=my-slug
brain isa-section brain-abc criteria --value-stdin
brain isa-render brain-abc

# Stores
brain stores list
brain stores create idea                 # one-shot provisioning
brain stores add idea ~/data/ideas/.beads  # register an existing path
brain stores env

# Render markdown (after corruption, deletion, or layout change)
bd render <id>                           # one bead
bd render-all                            # every bead; prints summary on stderr
bd render-all --json                     # structured: {rendered, failed, total, root, results[]}
```

## Status

Active personal infrastructure. APIs may break without notice. No external contributions accepted — this repo exists so agents I run can depend on it.

## Upstream

Forked from [gastownhall/beads](https://github.com/gastownhall/beads). Periodic rebases bring beads fixes forward. Brain-specific code lives under `cmd/bd/brain_*.go`, `internal/brain/`, and `divergence/`.

## License

Inherits the upstream beads license. See `LICENSE`.
