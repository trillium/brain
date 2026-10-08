---
id: render-import
title: bd render-import
slug: /cli-reference/render-import
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc render-import`

## bd render-import

Import edits made to this store's rendered markdown files back into the
beads they name, and mark beads whose rendered file has been deleted.

The pass is opt-in per store and refuses to guess:

  - A store accepts edit-back only when its registry entry declares it:
      bd stores edit-back &lt;name&gt; on
    Where the declaration lives: ~/.config/brain/stores.yaml, per store.
    'bd stores list' shows which stores accept edits (one-way is the
    default). A store that does not accept edit-back keeps today's one-way
    rendering: this run reports the file-vs-row differences and writes
    nothing to the substrate.
  - WHAT AN EDIT MEANS: the fields the import defines are title, status,
    priority, labels (the frontmatter's labels/tags), and description (the
    body after the "# &#123;title&#125;" H1). Everything else on the bead — id, kind,
    render-key slug, created/updated timestamps, metadata — stays
    substrate-owned; the file's disagreement there is never a change. The
    substrate stays the authority: the database keeps the durable record,
    and the render-import run is the one place the field is allowed to win,
    with every change printed as old → new.
  - DELETION MARKS, NEVER DELETES: a rendered file that has disappeared
    (recorded by the render manifest, entries/.render-manifest.json) marks
    its bead with the "marked-for-deletion" label. The bead is never
    removed and its label is visible on every read. A bead wearing the
    mark is skipped by every render — a re-render never silently clears
    the mark, and an unmarked render never resurrects the deleted file —
    so an accident in the synced folder cannot destroy the record, and
    nothing resurrects it without a human. The mark is cleared only by
      bd render-marks clear &lt;id&gt;
    after which the normal render path re-creates the file.

Refusals are loud and named — each is a different refusal with its own
code, spelled out by the run: a file that cannot be read; frontmatter
that names no bead (no id); a file naming a bead whose id prefix belongs
to another store's namespace; a file naming a bead that does not exist in
this store (refused by name); a file whose slug disagrees with the bead it
names; a file at a path the render manifest never recorded for that bead;
tags and labels disagreeing; a body whose H1 disagrees with both the
file's and the bead's title; a priority that is not an integer 0-4; an
edit to a bead that is already marked for deletion.

Behavioral contract (see divergence/0029): each refused file names its
reason; each applied edit names old → new; each mark names the missing
file. Exit 0 only when nothing was refused.

Examples:
  bd render-import
  bd render-import --json

```
bd render-import [flags]
```
