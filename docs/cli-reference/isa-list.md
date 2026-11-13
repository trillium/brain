---
id: isa-list
title: bd isa-list
slug: /cli-reference/isa-list
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc isa-list`

## bd isa-list

List all ISA-kind issues with their phase, progress, effort, and last
update. With --active, filters out ISAs whose phase is LEARN (i.e. completed
runs that are in the learning/post-mortem phase).

  bd isa-list              # text table, every ISA
  bd isa-list --active     # text table, ISAs not in LEARN phase
  bd isa-list --json       # JSON array of compact objects

Empty result: exit 0 with no output.

```
bd isa-list [flags]
```

**Flags:**

```
      --active   Filter to ISAs whose phase is not LEARN
      --json     Emit a JSON array of compact ISA objects
```
