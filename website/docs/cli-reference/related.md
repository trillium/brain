---
id: related
title: bd related
slug: /cli-reference/related
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc related`

## bd related

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
  brain related B-a7b3c
  brain related B-a7b3c --depth=3
  brain related B-a7b3c --depth=0      # print the center alone
  brain related B-a7b3c --json         # machine-readable tree

```
bd related <id> [flags]
```

**Flags:**

```
      --depth int   BFS depth cap (0 prints the center alone; higher walks further) (default 2)
```
