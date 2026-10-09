---
id: 0029
title: opt-in edit-back from rendered markdown, and mark-for-deletion
isc: []
status: landed
created: 2026-10-09
updated: 2026-10-09
commits: [c75f94f01, c90645a02, b5a8d7d8c, d5b461f00]
touches:
  - internal/brain/editback/
  - internal/brain/exfiltrator/manifest.go
  - internal/brain/exfiltrator/manifest_test.go
  - internal/brain/exfiltrator/exfiltrator.go
  - cmd/bd/render_import.go
  - cmd/bd/render_marks.go
  - cmd/bd/render.go
  - cmd/bd/brain_stores.go
  - docs/brain/EDIT_BACK.md
  - docs/brain/README.md
  - docs/CLI_REFERENCE.md
  - divergence/0029-brain-markdown-editback.md
upstream_rebase_notes: |
  Brain-only feature; upstream bd has no rendered-markdown view to edit.
  Conflict hotspots: cmd/bd/render.go (two `IsSkipMarked` branches added
  around `exf.Render`), cmd/bd/brain_stores.go (`storeEntry.EditBack`, the
  `edit-back` verb, `stores list` output), and
  internal/brain/exfiltrator/exfiltrator.go (a deletion-mark check at the top
  of `Render`, a manifest write at the end of `Render` and `Remove`). The
  rest is new files. The generated docs/CLI_REFERENCE.md and
  website/docs/cli-reference/ must be regenerated after a rebase
  (scripts/generate-cli-docs.sh).
---

# Why

Two verdicts from the captain's approved vision (`VISION.md`):

- *Editing a rendered file* — **Conditional**: "yes - if the user wants this,
  edits to data/{store}/{beadfile}.md should pass back into the bead".
- *Deleting a rendered file* — **Off mission** as the symmetric version:
  "add label marked for deletion maybe? I don't want users accidentally
  deleting data". The vision carries it as: it marks the bead for deletion,
  so an accident in a synced folder cannot destroy the record.

Before this change rendering was one-way and a bridge daemon syncs the
markdown directory between machines, so a stray delete or a stale copy in the
synced folder was invisible to the database. This change makes the file an
optional, per-store *input* without letting it become a second authority.

# What changed

- **Per-store declaration.** `~/.config/brain/stores.yaml` gains
  `edit_back: true` on a store entry (`storeEntry.EditBack`, off by default).
  `bd stores edit-back <name> on|off` sets it; `bd stores list` shows
  `one-way` / `edit-back` per store and `--json` carries `edit_back`.
- **`bd render-import`** (new, `internal/brain/editback`). One explicit pass
  over the store's rendered files, scoped to one store (refuses without
  `BD_NAME`). For each file:
  - an edit updates the bead the file's `id:` names — five fields only:
    title, status, priority, labels, description. Everything else in the
    file is substrate-owned and never a change;
  - a store that does not declare edit-back reports the same edits as
    `ignored` and writes nothing;
  - every applied or ignored change prints old → new; replaced values stay in
    Dolt history.
- **Conflict rule (two writers).** The row wins by default. A store that
  accepts edit-back lets the file win on the five fields, but only when the
  file was edited from the *current* record: the file's `updated:` stamp must
  not be older than the row's. An older stamp is a stale copy from the synced
  folder and is refused (`stale-file`), because importing it would revert
  newer data. The losing side is always visible (old → new, a named refusal,
  Dolt history).
- **Deletion marks, never deletes.** A render manifest
  (`entries/.render-manifest.json`, written by the renderer on every render
  and dropped on a deliberate `Remove`) records which files exist. When a
  recorded file is gone, `render-import` adds the label
  `marked-for-deletion` to the bead. The mark is visible on every read
  (`bd show`, `bd list --label`, `bd render-marks list`); while it stands
  `Render` returns a typed skip (`SkipMarkedError`) so `bd render` and
  `bd render-all` skip the bead with a named status and nothing resurrects
  the file; a re-render never clears it. `bd render-marks clear <id>` is the
  only way it leaves, after which the normal render re-creates the file.
  Nothing in this feature deletes a bead.
- **Named refusals** (never a silent skip): `cannot-read`, `cannot-parse`,
  `no-name`, `unknown-bead`, `outside-namespace`, `slug-mismatch`,
  `not-manifested-path`, `stale-file`, `unversioned-file`, `ambiguous-labels`,
  `ambiguous-body`, `priority-unreadable`, `edited-while-marked`,
  `write-failed`, `lookup-failed`, `manifest-escapes-root`. Exit 1 if any file
  was refused. A missing manifest is reported as "deletions NOT scanned".
- **Docs.** `docs/brain/EDIT_BACK.md` (how to turn it on, what an edit does,
  what deletion means), the CLI reference entries for `render-import`,
  `render-marks` and `stores edit-back`.

# Known limits

- The stale guard compares stamps to the second with 2 seconds of slack (a
  bead's first render persists its slug after writing the file, bumping the
  row's stamp); a real change within that window is not detectable.
- On the embedded engine the namespace check cannot read the prefix-ownership
  record and falls back to the store's own prefix only (stricter, so it refuses
  more, never less).
- Deletion detection only covers files the manifest recorded; run
  `bd render-all` once after upgrading.
- Not wired to run automatically: `render-import` is an explicit command.
  Scheduling it is a decision for the operator (and for the sync bridge).
- No store was cut over, re-pointed or migrated; behaviour was proven on a
  scratch copy only (see the task report).
