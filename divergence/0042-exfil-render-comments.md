---
id: 0042
title: mirror each bead's comment trail into the rendered markdown
isc: []
status: proposed
created: 2026-11-03
updated: 2026-11-03
commits: []
touches:
  - internal/brain/exfiltrator/comments.go
  - internal/brain/exfiltrator/comments_test.go
  - internal/brain/exfiltrator/exfiltrator.go
  - internal/brain/editback/frontmatter.go
  - internal/brain/editback/engine_test.go
  - internal/storage/brain_exfiltration_decorator_test.go
  - divergence/0042-exfil-render-comments.md
upstream_rebase_notes: |
  All touched paths are brain-only; resolve `ours`. No schema change and no
  new wiring: the comment source is found by type assertion on the
  EdgeSource already passed to `WithLinks`.
---

# Why

Adding a comment re-rendered the parent bead, but the renderer had no comment
section, so the file was byte-identical before and after. Comments are part of
the durable record (`bd show` prints them), yet the greppable copy silently
diverged from the database for them. The mirror now carries the trail.

# What changed

- The render appends a `## Comments` section, oldest first, one
  `### <author> · <RFC3339 UTC>` heading per comment followed by its text. It
  sits between the description and the links block, inside
  `<!-- brain:comments -->` / `<!-- /brain:comments -->` markers.
- The section is emitted only when the bead has comments, so comment-less
  files stay byte-identical.
- `Render` reads comments from `Issue.Comments` when hydrated, else from the
  `EdgeSource` if it also offers `GetIssueComments`. Every path funnels through
  `Render`, so comment-add (decorator, including inside a transaction), `bd
  render`, `bd render-all` and the reconciler all pick the section up. A
  lookup failure renders without comments; the next render converges.
- Edit-back strips the block (`StripCommentsBlock`) so comments never read as a
  description edit. Marker text quoted inside a comment is escaped so it cannot
  forge a block boundary.
- Editing comments in the file is not synced back; the file's comments are a
  read-only mirror.
