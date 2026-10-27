---
id: lint
title: bd lint
slug: /cli-reference/lint
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc lint`

## bd lint

Check issues for missing recommended sections based on issue type.

By default, lints all open issues. Specify issue IDs to lint specific issues.

Section requirements by type:
  bug:      Steps to Reproduce, Acceptance Criteria
  task:     Acceptance Criteria
  feature:  Acceptance Criteria
  epic:     Success Criteria
  chore:    (none)

Examples:
  bd lint                    # Lint all open issues
  bd lint bd-abc             # Lint specific issue
  bd lint bd-abc bd-def      # Lint multiple issues
  bd lint --type bug         # Lint only bugs
  bd lint --status all       # Lint all issues (including closed)


```
bd lint [issue-id...] [flags]
```

**Flags:**

```
  -s, --status string   Filter by status (default: open, use 'all' for all)
  -t, --type string     Filter by issue type (bug, task, feature, epic)
```

### bd lint hierarchy

Report parent-child edges that break the specificity ladder.

The check runs only when this store's config.yaml has
hierarchy.priority=specificity. Unset, it prints nothing and exits 0.
Other stores are unchanged.

P0 is the broadest capability. Each child along a parent-child edge uses
a higher priority number, through P4. A parent at 4 may have a child at 4.
Skips are legal. This command lists violating edges and writes nothing.
It exits non-zero when any violation exists.

Set the key with:
  bd config set hierarchy.priority specificity


```
bd lint hierarchy [flags]
```
