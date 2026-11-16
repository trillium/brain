---
title: "bd recall"
description: "BM25-ranked search over full bead bodies and comments"
---

{/* AUTO-GENERATED: do not edit manually */}

Generated from `bd help --doc recall`.

brain recall ranks every bead in this wrapper's namespace against a
free-text query and prints the best matches with their scores.

brain search is a substring filter: every word of the query must appear
somewhere in the bead, and the survivors are ordered by where the words sit.
recall is a ranking instead. It weighs the whole bead — title, description,
design, notes, acceptance criteria, close reason, labels and the full text
of every comment — with BM25 (term frequency against document length, rare
words counting more), so a bead that mentions the topic only in a comment,
or many times in a long note, surfaces with a score that says how well it
fits. Words need not all match; the ones that do decide the rank. All
statuses are searched, closed beads included.

Each hit prints its score, status icon, id, kind, title, and which fields
matched (id, title, body, comment, label). --json emits the same as an object
with query, tokens, corpus size and hits. No hit at all exits non-zero.

--kind filters on the bead's issue_type exactly (knowledge, task, isa,
decision, …). --wide ranks every store's beads instead of this wrapper's.

The memory plane's read-by-key verb moved to 'bd memories recall &lt;key&gt;'
('bd remember &lt;key&gt;' reads it too).

Examples:
  brain recall "kokoro speak daemon"
  brain recall --top 10 "tailnet phone"
  brain recall --kind knowledge --json "talon vocabulary"

```
bd recall <query> [flags]
```

**Flags:**

```
      --kind string   Only beads of this kind (issue_type): knowledge, task, isa, decision, …
      --top int       Number of hits to print (negative for all) (default 5)
```
