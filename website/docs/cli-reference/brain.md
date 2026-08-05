---
id: brain
title: bd brain
slug: /cli-reference/brain
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc brain`

## bd brain

brain v0.3 absorbs bd into a single tool that unifies tasks and
knowledge under one substrate (Dolt) with markdown as the exfiltrated
render artifact.

The 'brain' verbs (new, show, list, link, related) speak the knowledge-graph
vocabulary documented in ISA.md. They are thin aliases over bd's storage
layer, gated by the kind discriminator (task | knowledge | both).

For the bd vocabulary (create, dep, list, show, etc.) see 'bd --help'.

See ISA.md §"Decisions" → "First-Tranche Decisions" → "Decision 5
(modularity-first architecture)" for the seam this command tree implements.

```
bd brain [flags]
```

### bd brain link

brain link writes one typed edge (a row in the dependencies table)
between two existing brain docs.

Exactly one edge-type flag must be set:
  --extends         the from-doc extends/revises the to-doc
  --learned-from    the from-doc captures a lesson learned from the to-doc
  --related         the two docs are related (the catch-all edge)
  --type &lt;name&gt;     any well-known bd dependency type (escape hatch)

The flags are mutually exclusive — setting zero or more than one is a
usage error. The wrapper resolves the chosen flag to a single edge-type
string and hands it to the verb; the verb does all validation, existence
probing, and storage I/O.

Examples:
  bd brain link B-a7b3c B-217 --learned-from
  bd brain link B-a7b3c B-552a --extends
  bd brain link B-100 B-101 --related
  bd brain link B-100 B-101 --type=blocks

```
bd brain link <from> <to> [flags]
```

**Flags:**

```
      --extends        Edge type: from-doc extends/revises to-doc
      --learned-from   Edge type: from-doc captures a lesson learned from to-doc
      --related        Edge type: the two docs are related
      --type string    Edge type: any well-known bd dependency type (escape hatch)
```

### bd brain new

brain new creates a brain doc of the given kind.

&lt;kind&gt; must be one of:
  task       — work to be done; participates in ready/blocked queues
  knowledge  — a note or learning; reference-only, never "ready"
  both       — task-shaped work whose body is also the lesson (defaults open)
  isa        — an Ideal State Artifact; allocates IDs of shape &lt;prefix&gt;-isa-XXXXX
               and REQUIRES a slug (auto-generated from &lt;title&gt; when --slug is
               omitted; supply --slug explicitly when the title yields no
               alphanumerics).

The kind value rides on the existing issues.issue_type column. For kind=isa
the verb additionally sets issue.IDPrefix="isa" so the storage layer allocates
"&lt;config-prefix&gt;-isa-XXXXX" IDs (see migrations 0050/0051/0052 for the
substrate).

--slug is optional for non-isa kinds: when non-empty it is validated against
the slug regex (^[a-z0-9][a-z0-9-]&#123;0,63&#125;$) and written to the issues.slug
column; when empty the column stays NULL. Slug values must be globally unique
across all kinds — collisions exit with code 2.

Examples:
  bd brain new task "ship the FTS5 indexer"
  bd brain new knowledge "Dolt FK constraints are lazy until commit"
  bd brain new both "Friday cache bug + postmortem" --body "details..."
  bd brain new isa "Brain as ISA Substrate"
  bd brain new isa "Custom ISA" --slug=my-custom-slug

Valid kinds: task | knowledge | both | isa

```
bd brain new <kind> <title> [flags]
```

**Flags:**

```
      --body string   Optional markdown body (maps to the existing description column)
      --slug string   Optional slug (required for kind=isa; auto-generated from title when omitted)
```

### bd brain promote

brain promote is NOT a brain verb. It is a redirector for the natural-
language verb that often comes to mind when someone wants to shift a
brain doc's kind (knowledge → task, for example).

The brain verb for kind-shift is "recast":

  brain recast &lt;id&gt; --to=&lt;kind&gt;

The name "promote" was avoided because bd already uses it for wisp →
bead graduation (a different, narrower operation). Namespacing matters
more than reading-naturalness for a verb that appears hundreds of
times in shell history.

```
bd brain promote [flags]
```

### bd brain recast

brain recast shifts the kind of an existing brain doc. Every edge
survives. Every comment survives. The body survives. The ID survives.
Only the issue_type column changes — and, on certain transitions, the
status column.

--to=&lt;kind&gt; is REQUIRED. Valid values:
  task        — work to be done; participates in ready/blocked queues
  knowledge   — a note or learning; reference-only, never "ready"
  both        — task-shaped work whose body is also the lesson

Status rules:
  knowledge → task / both : status defaults to 'open' unless the row
                            was explicitly 'closed' (then preserved)
  task → knowledge / both : status preserved
  both → task / knowledge : status preserved

If the current kind already equals --to, recast is a no-op (exit 0,
no write, no markdown churn).

Markdown relocation (entries/knowledge/&lt;slug&gt;.md → entries/task/&lt;slug&gt;.md)
is OUT OF SCOPE for this verb — the exfiltrator handles it on its next
idempotent sync.

Examples:
  brain recast B-a7b3c --to=task
  brain recast B-a7b3c --to=knowledge
  brain recast B-a7b3c --to=both
  brain recast B-a7b3c --to=task --json

```
bd brain recast <id> [flags]
```

**Flags:**

```
      --to string   Target kind: one of task | knowledge | both (required)
```

### bd brain related

brain related performs a breadth-first walk of outgoing edges from
the given center brain doc and prints the reachable subgraph as an
indented tree.

The walk is bounded by --depth (default 2). --depth=0 prints just the
center; --depth=1 prints the center and direct neighbours; higher
values BFS further out. Each printed node carries its kind tag
([kind=task], [kind=knowledge], or [kind=both]) and — for closed tasks
— a ", closed" annotation. On a cycle, the second appearance of a node
is annotated "(already visited)" and the BFS does not recurse through
it again.

Edges are followed in the outgoing (from → to) direction only — the
same direction "bd dep list" prints by default and the same direction
"brain link &lt;a&gt; &lt;b&gt;" creates. This keeps the rendered tree directional
and prevents an explosion at common hub nodes; a future
"--bidirectional" flag is conceivable but not in scope.

Examples:
  bd brain related B-a7b3c
  bd brain related B-a7b3c --depth=3
  bd brain related B-a7b3c --depth=0      # print the center alone
  bd brain related B-a7b3c --json         # machine-readable tree

```
bd brain related <id> [flags]
```

**Flags:**

```
      --depth int   BFS depth cap (0 prints the center alone; higher walks further) (default 2)
```

### bd brain stores

Manage the registry of bd stores federated under brain.

The registry lives at ~/.config/pai/stores.yaml. Each registered store
can be searched via 'brain search', transferred to via 'brain transfer',
and synced via 'brain repo sync'.

Run 'brain stores env' to regenerate ~/.config/pai/stores.env for
shell wrapper scripts that need PAI_STORE_* variables.

```
bd stores [flags]
```

#### bd brain stores add

Register a store in the brain federation registry

```
bd stores add <name> <beads-dir> [flags]
```

#### bd brain stores alias

Create a second CLI variant ('alias') that points at an already-
registered store. Useful for singular/plural pairs (idea/ideas,
person/people) or short forms of long names.

Writes a wrapper at ~/.local/bin/&lt;new-name&gt; pinning the existing
store's BEADS_DIR and BD_NAME. Does NOT add a separate registry
entry — both names route to the same data because the wrapper
sets BEADS_DIR identically.

Examples:
  brain stores alias robots robot       # 'robot' command → robots store
  brain stores alias person people      # 'people' command → person store
  brain stores alias ideas idea         # 'idea' command → ideas store

To remove an alias, delete the wrapper:  rm ~/.local/bin/&lt;new-name&gt;

```
bd stores alias <existing-store> <new-name> [flags]
```

**Flags:**

```
      --bd-binary string   Path to bd that the wrapper should exec (default: "bd")
      --no-wrapper         Skip writing the wrapper at ~/.local/bin/<alias>
```

#### bd brain stores create

Provision a new connected store end-to-end. Does, in order:

  1. Create &lt;path&gt;/.beads/ and run 'dolt init' inside it.
  2. Create &lt;path&gt;/entries/ for exfiltrated markdown.
  3. Write a CLI wrapper at ~/.local/bin/&lt;name&gt; that pins BEADS_DIR
     and BD_NAME, then exec's bd.
  4. Register the store in ~/.config/pai/stores.yaml.
  5. Regenerate ~/.config/pai/stores.env.

Default path is $HOME/data/&lt;name&gt;. Override with --path. Skip the wrapper
with --no-wrapper if you manage shell shims another way.

If any step fails after files have been written, the verb leaves the
partial state in place and exits non-zero — re-running with the same
arguments resumes idempotently.

Examples:
  brain stores create recipes
  brain stores create recipes --path /Volumes/extra/recipes
  brain stores create recipes --bd-binary /opt/homebrew/bin/bd

```
bd stores create <name> [flags]
```

**Flags:**

```
      --bd-binary string    Path to bd that the wrapper should exec (default: "bd", resolved by PATH)
      --dolt-email string   --email arg to pass to 'dolt init' (skipped if empty)
      --dolt-name string    --name arg to pass to 'dolt init' (skipped if empty)
      --no-wrapper          Skip writing the CLI wrapper at ~/.local/bin/<name>
      --path string         Filesystem root for the new store (default: $HOME/data/<name>)
```

#### bd brain stores env

Write ~/.config/pai/stores.env from the registry (for shell wrappers)

```
bd stores env [flags]
```

#### bd brain stores list

List all registered stores

```
bd stores list [flags]
```

**Flags:**

```
  -v, --verbose   Include each store's about blurb as an extra column
```

#### bd brain stores remove

Unregister a store from the brain federation registry

```
bd stores remove <name> [flags]
```

#### bd brain stores rename

Rename a connected store end-to-end. Does, in order:

  1. Rename ~/data/&lt;old-name&gt;/  →  ~/data/&lt;new-name&gt;/
     (if the path follows the default ~/data/&lt;name&gt;/ convention).
     A custom path is left in place; only the registry key changes.
  2. Rewrite the wrapper at ~/.local/bin/&lt;old-name&gt; to ~/.local/bin/&lt;new-name&gt;
     pointing at the new path. Old wrapper is removed unless --keep-old-wrapper.
  3. Update ~/.config/pai/stores.yaml: &lt;old-name&gt; → &lt;new-name&gt;.
  4. Regenerate ~/.config/pai/stores.env.

What this does NOT do:
  - The underlying Dolt database name stays the same — existing bead IDs
    keep their original prefix (e.g. agent-XXXXX stays agent-XXXXX after
    renaming 'agents' to 'robots'). The transfer-verb's builtin alias
    table maps the old name to the new for legacy lookups.
  - The exfiltrated markdown root moves with the directory, so future
    'bd render' calls write to ~/data/&lt;new-name&gt;/entries/ automatically.

Examples:
  brain stores rename agents robots
  brain stores rename fishes whales
  brain stores rename agents robots --keep-old-wrapper   # leave both names working

```
bd stores rename <old-name> <new-name> [flags]
```

**Flags:**

```
      --keep-old-wrapper   Leave ~/.local/bin/<old-name> in place pointing at the renamed path
```

#### bd brain stores render-all

Iterate every store registered in ~/.config/pai/stores.yaml and
trigger markdown exfiltration for each one. The current bd binary is
re-invoked once per store with BEADS_DIR pinned, so per-store summaries
land on stderr exactly as a stand-alone 'bd render-all' would.

Useful after:
  - Installing a new bd binary that changes the exfiltration contract
    (every-kind exfil, store-derived root, etc.).
  - Restoring a store from backup where the markdown sidecar drifted.
  - Adding a new store to the registry and wanting it populated.

A federation-level summary line is emitted on stderr at the end:
  Federation: &lt;N&gt; stores OK, &lt;M&gt; stores failed (rendered &lt;R&gt;, failed &lt;F&gt;)

JSON mode (--json) replaces the per-store stdout streams with one
structured object per store and a top-level summary.

Exit codes:
  0 — every store reported success
  1 — at least one store had per-bead failures or could not be opened

```
bd stores render-all [flags]
```

**Flags:**

```
      --json   Emit a structured JSON object instead of per-store text summaries
```

#### bd brain stores set-about

Attach a short description to a registered store, stored in the
registry (~/.config/pai/stores.yaml) only. The wrapper script is left
unchanged. View blurbs with 'brain stores list --verbose'.

Pass an empty string to clear the blurb.

Examples:
  brain stores set-about robots "agent-detected tooling defects"
  brain stores set-about ideas ""     # clear the blurb

```
bd stores set-about <store> <blurb> [flags]
```
