---
id: isa-render-pending
title: bd isa-render-pending
slug: /cli-reference/isa-render-pending
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc isa-render-pending`

## bd isa-render-pending

List every ISA whose on-disk markdown is stale relative to the
substrate.

Pending state is derived, not persisted — there is no separate "needs render"
column. An ISA is pending when:

  - the target ISA.md file does not exist on disk, OR
  - the file's mtime is older than the row's isa_updated_at.

Auto-render hooks on bd patch and bd isa-section attempt a synchronous
post-commit render. If that render fails (disk full, permission denied,
filesystem unavailable), the brain write is NOT rolled back — the brain row
is canonical, the markdown is a shadow — and a warning is logged to stderr.
This verb surfaces every shadow that has fallen behind, so the operator can
re-run 'bd isa-render &lt;id&gt;' or 'bd isa-render-all' to bring the disk back
into sync.

Text output (one line per stale ISA):
  &lt;id&gt;\t&lt;slug&gt;\t&lt;reason&gt;

where reason is one of:
  - "missing"
  - "stale (file: &lt;RFC3339&gt;, db: &lt;RFC3339&gt;)"
  - "path error: &lt;message&gt;"   (slug somehow contains '..' or '/' despite
                                 the F1d regex)

JSON output (--json):
  [&#123; "id", "slug", "reason", "file_mtime", "isa_updated_at" &#125;, ...]

Exit codes:
  0 — pending list emitted (empty if nothing stale)
  1 — listing failed catastrophically (DB error, etc.)

Examples:
  bd isa-render-pending
  bd isa-render-pending --json | jq '.[].id'

```
bd isa-render-pending [flags]
```

**Flags:**

```
      --json   Emit results as a JSON array
```
