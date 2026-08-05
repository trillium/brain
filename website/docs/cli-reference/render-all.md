---
id: render-all
title: bd render-all
slug: /cli-reference/render-all
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc render-all`

## bd render-all

Walk every issue in the substrate and render its markdown to the
configured exfil root.

For each issue, one tab-separated line is printed to stdout:
  &lt;id&gt;\t&lt;path&gt;\t&lt;status&gt;

where status is "rendered" or "failed: &lt;reason&gt;". A per-issue failure does not
stop the run; the exit code is 0 only if every render succeeded. Otherwise
exit 1 (and individual failure lines on stdout describe what went wrong).

A summary line is emitted on stderr at the end:
  Exfiltrated &lt;ok&gt; / &lt;total&gt; beads to &lt;root&gt;/entries/ (&lt;failed&gt; failed)

With --json, stdout is a single JSON object instead of per-line text:
  &#123; "rendered": N, "failed": N, "total": N, "root": "&lt;path&gt;",
    "results": [ &#123; "id", "path", "status", "error" &#125;, ... ] &#125;

```
bd render-all [flags]
```
