---
id: isa-show
title: bd isa-show
slug: /cli-reference/isa-show
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc isa-show`

## bd isa-show

Read an ISA-kind issue's full document (or a single section) directly from
the substrate.

By default isa-show emits markdown assembled from the twelve canonical
sections in spec order, suitable for piping to a pager. Use --json to emit
the stable JSON document shape consumed by tooling.

  bd isa-show isa-001                      # markdown (default)
  bd isa-show isa-001 --section=problem    # just the Problem body, no header
  bd isa-show isa-001 --json               # full JSON doc
  bd isa-show isa-001 --json --section=X   # --json wins; --section is ignored

isa-show is only valid for kind=isa. Calling it on any other kind exits 1.
Missing issue exits 1.

```
bd isa-show <id> [flags]
```

**Flags:**

```
      --json             Emit the stable JSON document shape instead of rendered markdown
      --section string   Restrict output to a single canonical section name (e.g. 'problem', 'changelog')
```
