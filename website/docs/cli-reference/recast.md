---
id: recast
title: bd recast
slug: /cli-reference/recast
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc recast`

## bd recast

brain recast shifts the kind of an existing brain doc. Every edge
survives. Every comment survives. The body survives. The ID survives.
Only the issue_type column changes — and, on certain transitions, the
status column.

--to=&lt;kind&gt; is REQUIRED. Valid values:
  task        — work to be done; participates in ready/blocked queues
  knowledge   — a note or learning; reference-only, never "ready"
  both        — task-shaped work whose body is also the lesson

Status rules:
  knowledge → task / both : status defaults to 'open' unless the row
                            was explicitly 'closed' (then preserved)
  task → knowledge / both : status preserved
  both → task / knowledge : status preserved

If the current kind already equals --to, recast is a no-op (exit 0,
no write, no markdown churn).

Markdown relocation (entries/knowledge/&lt;slug&gt;.md → entries/task/&lt;slug&gt;.md)
is OUT OF SCOPE for this verb — the exfiltrator handles it on its next
idempotent sync.

Examples:
  brain recast B-a7b3c --to=task
  brain recast B-a7b3c --to=knowledge
  brain recast B-a7b3c --to=both
  brain recast B-a7b3c --to=task --json

```
bd recast <id> [flags]
```

**Flags:**

```
      --to string   Target kind: one of task | knowledge | both (required)
```
