---
id: dep
title: bd dep
slug: /cli-reference/dep
sidebar_position: 100
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc dep`

## bd dep

Manage dependencies between issues.

When called with an issue ID and --blocks flag, creates a blocking dependency:
  bd dep &lt;blocker-id&gt; --blocks &lt;blocked-id&gt;

This is equivalent to:
  bd dep add &lt;blocked-id&gt; &lt;blocker-id&gt;

Examples:
  bd dep bd-xyz --blocks bd-abc    # bd-xyz blocks bd-abc
  bd dep add bd-abc bd-xyz         # Same as above (bd-abc depends on bd-xyz)

```
bd dep [issue-id] [flags]
```

**Flags:**

```
  -b, --blocks string    Issue ID that this issue blocks (shorthand for: bd dep add <blocked> <blocker>)
      --no-cycle-check   Skip per-edge cycle checks for speed (bulk wiring); bulk --file adds still run one final whole-graph check before commit
```

### bd dep add

Add a dependency between two issues.

The depends-on-id can be provided as:
  - A positional argument: bd dep add issue-123 issue-456
  - A flag: bd dep add issue-123 --blocked-by issue-456
  - A flag: bd dep add issue-123 --depends-on issue-456

The --blocked-by and --depends-on flags are aliases and both mean "issue-123
depends on (is blocked by) the specified issue."

The depends-on-id can be:
  - A local issue ID (e.g., bd-xyz)
  - An external reference: external:&lt;project&gt;:&lt;capability&gt;

For bulk wiring, pass newline-delimited JSON with --file. Each line must be an
object with "from" and "to" fields, and may include "type". The aliases
"issue_id" and "depends_on_id" are also accepted. Use --file - to read stdin.

External references are stored as-is and resolved at query time using
the external_projects config. They block the issue until the capability
is "shipped" in the target project.

Examples:
  bd dep add bd-42 bd-41                              # Positional args
  bd dep add bd-42 --blocked-by bd-41                 # Flag syntax (same effect)
  bd dep add bd-42 --depends-on bd-41                 # Alias (same effect)
  bd dep add gt-xyz external:beads:mol-run-assignee   # Cross-project dependency
  bd dep add bd-42 bd-41 --no-cycle-check             # Skip cycle check (bulk wiring)
  bd dep add --file deps.jsonl                        # Bulk JSONL: &#123;"from":"bd-42","to":"bd-41"&#125;

```
bd dep add [issue-id] [depends-on-id] [flags]
```

**Flags:**

```
      --blocked-by string   Issue ID that blocks the first issue (alternative to positional arg)
      --depends-on string   Issue ID that the first issue depends on (alias for --blocked-by)
      --file string         Read dependency edges from JSONL file, or '-' for stdin
      --no-cycle-check      Skip per-edge cycle checks for speed (bulk wiring); bulk --file adds still run one final whole-graph check before commit
  -t, --type string         Dependency type (blocks|tracks|related|parent-child|discovered-from|until|caused-by|validates|relates-to|supersedes) (default "blocks")
```

### bd dep cycles

Detect dependency cycles

```
bd dep cycles [flags]
```

### bd dep list

List dependencies or dependents of one or more issues with optional type filtering.

By default shows dependencies (what issues depend on). Use --direction to control:
  - down: Show dependencies (what this issue depends on) - default
  - up:   Show dependents (what depends on this issue)

Multiple IDs can be provided for batch dep listing. With --json, the output
is a flat array of dependency records across all requested issues.

Use --type to filter by dependency type (e.g., tracks, blocks, parent-child).

Examples:
  bd dep list gt-abc                     # Show what gt-abc depends on
  bd dep list gt-abc gt-def              # Batch: deps for both issues
  bd dep list gt-abc --direction=up      # Show what depends on gt-abc
  bd dep list gt-abc --direction=up -t tracks  # Show what tracks gt-abc (convoy tracking)

```
bd dep list [issue-id...] [flags]
```

**Flags:**

```
      --direction string   Direction: 'down' (dependencies), 'up' (dependents) (default "down")
  -t, --type string        Filter by dependency type (e.g., tracks, blocks, parent-child)
```

### bd dep reclassify

Find dependency rows whose target is stored as text in depends_on_external
and make the ones this database actually holds into real edges.

An edge whose target this database holds belongs in depends_on_issue_id (or
depends_on_wisp_id). A text row is invisible to every graph reader: nothing
traverses it, nothing blocks on it, and 'bd ready' lists a blocked bead as
ready. On the unified database every store's beads share one issues table, so
most text rows are this case — a cross-store edge filed as foreign because the
two ids carried different prefixes (see docs/design/brain-single-database.md,
and issueops.ProbeDepTarget for the rule that replaced the prefix comparison).

What this command does with each text row:
  - target this database holds  → move it into its typed column (a real edge);
  - target of a pair that already carries a real edge of the same type
                               → remove the redundant text row (one edge, one id);
  - target of a pair that already carries a real edge of a DIFFERENT type
                               → left alone and reported: a pair carries one
                                 type, so which one survives is your call;
  - target this database holds nowhere, and the "external:..." form
                               → left exactly as it is: depends_on_external is
                                 for genuinely foreign targets.

Without --apply nothing is written: the command prints the plan, every row it
would touch, and the exact command that would apply it. With --apply the whole
repair is one Dolt commit (staging only the edge table and issues), so it is
one 'dolt revert' away.

```
bd dep reclassify [flags]
```

**Flags:**

```
      --apply   Apply the repair (default: dry run, nothing written)
```

### bd dep relate

Create a loose 'see also' relationship between two issues.

The relates_to link is bidirectional - both issues will reference each other.
This enables knowledge graph connections without blocking or hierarchy.

Examples:
  bd relate bd-abc bd-xyz    # Link two related issues
  bd relate bd-123 bd-456    # Create see-also connection

```
bd dep relate <id1> <id2> [flags]
```

### bd dep remove

Remove a dependency

```
bd dep remove [issue-id] [depends-on-id] [flags]
```

**Aliases:** rm

### bd dep tree

Show dependency tree rooted at the given issue.

By default, shows dependencies (what blocks this issue). Use --direction to control:
  - down: Show dependencies (what blocks this issue) - default
  - up:   Show dependents (what this issue blocks)
  - both: Show full graph in both directions

Examples:
  bd dep tree gt-0iqq                    # Show what blocks gt-0iqq
  bd dep tree gt-0iqq --direction=up     # Show what gt-0iqq blocks
  bd dep tree gt-0iqq --status=open      # Only show open issues
  bd dep tree gt-0iqq --depth=3          # Limit to 3 levels deep

```
bd dep tree [issue-id] [flags]
```

**Flags:**

```
      --direction string   Tree direction: 'down' (dependencies), 'up' (dependents), or 'both'
      --format string      Output format: 'mermaid' for Mermaid.js flowchart
  -d, --max-depth int      Maximum tree depth to display (safety limit) (default 50)
      --reverse            Show dependent tree (deprecated: use --direction=up)
      --show-all-paths     Show all paths to nodes (no deduplication for diamond dependencies)
      --status string      Filter to only show issues with this status (open, in_progress, blocked, deferred, closed)
```

### bd dep unrelate

Remove a relates_to relationship between two issues.

Removes the link in both directions.

Example:
  bd unrelate bd-abc bd-xyz

```
bd dep unrelate <id1> <id2> [flags]
```
