---
id: isa-render
title: bd isa-render
slug: /cli-reference/isa-render
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc isa-render`

## bd isa-render

Render an ISA-kind issue to canonical markdown at
&lt;exfil-root&gt;/&lt;slug&gt;/ISA.md.

The exfil root is configurable via the BRAIN_ISA_EXFIL_ROOT environment
variable; it defaults to $&#123;HOME&#125;/.claude/PAI/MEMORY/WORK. The render is
atomic: a temp file is written first, then rename(2) makes the swap, so
readers never observe a half-written file.

The rendered file mirrors the IsaFormat v2.7 frontmatter (task, slug, effort,
phase, progress, mode, started, updated) and adds one new key — brain_id —
so downstream tools can trace markdown back to the substrate row.

isa-render is only valid for kind=isa. Calling it on any other kind exits 1.
Missing issue exits 1. Path-traversal-shaped slugs (someone INSERT'd a slug
with '/' or '..' directly into the DB) exit 2.

The path of the rendered file is printed to stdout. Exit 0 on success.

Examples:
  bd isa-render brain-isa-00001
  BRAIN_ISA_EXFIL_ROOT=/tmp/work bd isa-render brain-isa-00001

```
bd isa-render <id> [flags]
```
