---
id: q
title: bd q
slug: /cli-reference/q
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc q`

## bd q

Quick capture creates an issue and outputs only the issue ID.
Designed for scripting and AI agent integration.

Example:
  bd q "Fix login bug"           # Outputs: bd-a1b2
  ISSUE=$(bd q "New feature")    # Capture ID in variable
  bd q "Task" | xargs bd show    # Pipe to other commands

```
bd q [title] [flags]
```

**Flags:**

```
  -l, --labels strings    Labels
      --prefix string     Mint a fresh id under this prefix instead of the store's default (the prefix must be owned by this store's namespace; claim one with 'bd store-prefix add')
  -p, --priority string   Priority (0-4 or P0-P4) (default "2")
  -t, --type string       Issue type (default "task")
```
