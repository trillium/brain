# brain

Personal agent infrastructure. A persistent memory layer that AI agents can depend on, with markdown exfiltration so the same content is also easy for humans to read and grep.

**brain IS [beads](https://github.com/gastownhall/beads) (`bd`), renamed.** One Go binary, one Cobra dispatch, one Dolt store. Installed under the name `brain` instead of `bd`, the binary answers to its own name — so every bd verb (`create`, `show`, `list`, `search`, `ready`, `close`, `update`, `dep`, ...) is reachable as `brain <verb>`. On top of that rename, brain adds a small, well-scoped delta:

- **A `kind` discriminator** — every doc is `task`, `knowledge`, or `both` — riding on bd's existing `issues.issue_type` column. No schema migration. Tasks and notes share one table, one ID space, one edge graph.
- **A graph-verb namespace** — `new`, `link`, `related`, `recast` — that speaks knowledge-graph vocabulary over the same storage layer.
- **Two extra edge types** — `extends` and `learned-from` — registered alongside bd's existing dependency types.
- **Markdown exfiltration** — every brain-kind mutation renders one markdown file to disk. Dolt is the source of truth; markdown is the human-readable, grep-friendly view.

That is the whole delta. There is no "brain layer" routing through bd — it is one tool with an extra verb namespace and an exfiltration hook.

## How it ships

One Go binary, installed under one or more names. bd ships a binary-rename mechanism: install it as `bd`, `brain`, `ideas`, `robots`, whatever — two environment variables do the rest.

- **`BD_NAME`** sets the name shown in help/usage text (e.g. `BD_NAME=brain` makes `--help` read `brain` instead of `bd`).
- **`BEADS_DIR`** selects which store the invocation operates on (its `.beads/` Dolt directory).

A `brain` install is just a wrapper that pins those two and execs the binary. Nothing is compiled per-name.

## Kinds

`kind` is the one column-shaped convention brain adds. Valid values:

| kind | meaning |
| --- | --- |
| `task` | work to be done; participates in `ready` / blocked queues |
| `knowledge` | a note or learning; reference-only, never "ready" |
| `both` | task-shaped work whose body is also the lesson |

You filter on `kind` the way you filter on a label. A `knowledge` doc can be recast into a `task` without losing its edges, because there are no separate namespaces to move between — you change one column value and every edge still points at the same ID.

## Verb surface

**Inherited bd verbs** are reachable directly under the install name — they are the same code path as upstream:

```sh
brain create "ship the FTS5 indexer"      # create an issue (bd's create; aliased as `new`)
brain list --status open                  # list issues
brain search "dolt fk"                     # full-text search
brain show B-a7b3c                         # show one doc
brain ready                               # tasks with no unmet blockers
brain dep add B-100 B-101 --type blocks    # add a dependency edge
```

**brain graph verbs** live under a `brain` sub-namespace (`brain brain <verb>` when installed as `brain`, `bd brain <verb>` on a `bd` install):

```sh
# create a brain doc of an explicit kind
brain brain new knowledge "Dolt FK constraints are lazy until commit"
brain brain new task "rebuild the retrieval index"
brain brain new both "Friday cache bug + postmortem" --body "details..."

# write one typed edge between two docs (exactly one edge-type flag)
brain brain link B-a7b3c B-217  --learned-from
brain brain link B-a7b3c B-552a --extends
brain brain link B-100 B-101    --related
brain brain link B-100 B-101    --type blocks   # escape hatch: any bd dep type

# walk the graph outward from a center doc and print the subgraph as a tree
brain brain related B-a7b3c --depth 2

# change a doc's kind in place — edges, comments, body, and ID all survive
brain brain recast B-a7b3c --to task
```

`brain brain promote` is not a verb; it is a redirect that points you at `recast` (bd already uses `promote` for wisp→bead graduation, a different operation). Hoisting the graph verbs to the top level (so `brain new` reaches the kind-aware creator rather than bd's `create` alias) is in progress on the `feat/brain-verb-hoist` branch and is not yet on `main`.

## Markdown exfiltration

Every brain-kind mutation writes a markdown file with YAML frontmatter to:

```
<root>/entries/{kind}/{slug}.md
```

The default `<root>` is `~/data/knowledge`, overridable with the `BRAIN_KNOWLEDGE_ROOT` environment variable. Dolt remains canonical — edits should go through the verbs, not by hand-editing the exfiltrated markdown, which is regenerated on the next mutation. The markdown exists so the content is greppable and readable outside the tool (including on a phone).

## Federated stores

Because store selection is just `BEADS_DIR` and the display name is just `BD_NAME`, a single binary backs a whole family of same-named wrappers — `task`, `brain`, `robots`, `decisions`, `questions`, `ideas`, `person`, `handoff`, and so on. Each wrapper pins its own `.beads/` directory (typically one database per store on a shared Dolt server) so the stores stay isolated but share one binary, one query language, and one exfiltration convention. This is a deployment pattern layered on the rename mechanism, not a separate feature of the binary.

## ISA substrate

First-class support for [PAI](https://github.com/danielmiessler/PAI) Algorithm ISAs (an `isa` kind, the `isa-section` / `isa-render` / `isa-show` / `isa-list` / `isa-by-slug` verbs, and the twelve canonical ISA sections) is developed on the **`feat/isa-substrate-f1`** branch and is **not yet merged to `main`**. The deployed PAI brain binary is built from that branch; a checkout of `main` does not include those verbs or the `isa` kind.

## Status

Active personal infrastructure. APIs may break without notice. No external contributions are accepted at this time — this repo exists to let the agents I run depend on it, not as a community project.

## Upstream

Forked from [gastownhall/beads](https://github.com/gastownhall/beads). Periodic rebases bring beads fixes forward. Brain-specific code lives under `cmd/bd/brain_*.go`, `internal/brain/`, and `internal/brain/exfiltrator/`. See `divergence/` for the full design history vs. upstream, and `docs/brain/WHAT_IS_BRAIN.md` for the long-form explainer.

## License

Inherits the upstream beads license. See `LICENSE`.
