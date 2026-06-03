# brain

Personal agent infrastructure. A persistent memory layer that AI agents can depend on, with markdown exfiltration so the same content is also easy for humans to read and grep.

brain is a Go fork of [beads](https://github.com/gastownhall/beads). It keeps beads' versioned Dolt substrate and graph-shaped issue model, then adds:

- **A unified surface for tasks, knowledge entries, and ISAs.** One bag of docs discriminated by `kind` (`task | knowledge | both | isa`), one set of verbs, one query layer.
- **Markdown exfiltration.** Every entry is rendered to a markdown file under `~/data/knowledge/entries/{kind}/{slug}.md` on every write. Dolt is the source of truth; markdown is the human-readable view.
- **ISA primitives.** First-class support for [PAI](https://github.com/danielmiessler/PAI) Algorithm v6.4+ ISAs (Ideal State Architecture documents) — including `bd new isa`, `bd isa-section`, `bd isa-render`, and per-ISA section UPSERT semantics.

## How it ships

One Go binary, two install names:

- **`bd`** — full beads-compatible surface for project-local issue stores.
- **`brain`** — wrapper script that pins `BEADS_DIR=$HOME/data/knowledge/.beads` and dispatches the brain verbs at the top level. Argv[0] dispatch via `BD_NAME=brain`.

Verbs hoisted to the top level (no `bd brain <verb>` subtree):

```
bd new <kind> <title> [--slug=...]   # create issue / knowledge / isa
bd link <from> <to> --type=<edge>    # graph edge
bd related <id>                      # walk the graph
bd recast <id> --kind=<new-kind>     # change kind discriminator
bd patch <id> --field=... --value=...
bd isa-section <id> <name> --value-stdin
bd isa-render <id>
```

See `divergence/` for the full design history vs upstream beads.

## Status

Active personal infrastructure. APIs may break without notice. No external contributions are accepted at this time — this repo exists to let agents I run depend on it, not as a community project.

## Upstream

Forked from [gastownhall/beads](https://github.com/gastownhall/beads). Periodic rebases bring beads fixes forward. Brain-specific code lives under `cmd/bd/brain_*.go`, `internal/brain/`, and `internal/storage/embeddeddolt/`.

## License

Inherits the upstream beads license. See `LICENSE`.
