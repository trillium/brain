---
id: isa-render-all
title: bd isa-render-all
slug: /cli-reference/isa-render-all
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc isa-render-all`

## bd isa-render-all

Re-render every ISA in the substrate to its canonical exfil path.

The exfil root and atomicity semantics match bd isa-render. With --since,
only ISAs whose isa_updated_at is at or after the supplied RFC3339 timestamp
are re-rendered — useful for incremental refreshes.

For each ISA, one tab-separated line is printed to stdout:
  &lt;id&gt;\t&lt;path&gt;\t&lt;status&gt;

where status is "rendered", "skipped-divergent", or "failed: &lt;reason&gt;".

"skipped-divergent" means the on-disk ISA.md is disk-canonical (it lacks this
row's brain_id frontmatter — a hand-authored file the render must not clobber);
that row is left untouched and is NOT counted as a failure.

A per-ISA failure does not stop the run; the exit code is 0 only if every
render either succeeded or was skipped as divergent. Otherwise exit 1 (and
individual failure lines on stdout describe what went wrong).

```
bd isa-render-all [flags]
```

**Flags:**

```
      --since string   Only re-render ISAs with isa_updated_at >= this RFC3339 timestamp
```
