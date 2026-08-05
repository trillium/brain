---
id: render
title: bd render
slug: /cli-reference/render
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc render`

## bd render

Render the markdown for a single issue and write it to disk under
&lt;exfil-root&gt;/entries/&lt;kind&gt;/&lt;slug&gt;.md.

Exfil root resolves in this order: BRAIN_KNOWLEDGE_ROOT env, dirname($BEADS_DIR),
~/data/brain. Every kind renders — task, knowledge, both, bug, feature, epic,
etc. — there is no kind gate.

Use this when the on-disk markdown has been deleted, corrupted, or written by
an older version of bd. The substrate row is authoritative; the markdown is a
derived view.

The path of the rendered file is printed to stdout. Exit 0 on success.

Examples:
  bd render brain-k00042
  BRAIN_KNOWLEDGE_ROOT=/tmp/work bd render brain-k00042

```
bd render <id> [flags]
```
