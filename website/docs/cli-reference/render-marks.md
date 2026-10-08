---
id: render-marks
title: bd render-marks
slug: /cli-reference/render-marks
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc render-marks`

## bd render-marks

Dealing with deletion marks.

When a rendered markdown file (entries/&lt;kind&gt;/&lt;slug&gt;.md) disappears — an
operator intended it, or an accident in the synced folder — 'bd
render-import' marks the bead with the "marked-for-deletion" label instead
of deleting anything. The bead stays; the mark is visible on every read
('bd show', 'bd list'); and, while the mark stands, renders skip the bead
(the file is not silently re-created), so the deletion intent gets a
human's review rather than a background undo.

  bd render-marks list          every bead carrying the mark
  bd render-marks clear &lt;id&gt;…   acknowledge the review, clear the mark

Clearing the mark is an explicit act and the only way a mark leaves a bead:
it removes the label through the normal store surface, after which the
post-write render path re-creates the file. Deleting the SUBSTRATE bead
itself, if the deletion intent is confirmed after all, is deliberately a
separate decision ('bd delete &lt;id&gt;') and never automatic.

```
bd render-marks [flags]
```

### bd render-marks clear

Clear the "marked-for-deletion" label on the named beads. This is the ONLY
way a mark leaves a bead — no render path clears it by itself.

After a clear, the normal render path re-creates each bead's rendered file
(the label change itself triggers the post-write render). To acknowledge
the deletion intent by actually deleting the substrate bead, use
'bd delete &lt;id&gt;' separately; this verb only unmarks, it never deletes.

```
bd render-marks clear <id>... [flags]
```

### bd render-marks list

List the beads whose rendered file was deleted and whose deletion has not
been reviewed yet. Each bead carries the "marked-for-deletion" label.

With --json, stdout is a JSON array of &#123;id, title, status&#125; objects.

```
bd render-marks list [flags]
```
