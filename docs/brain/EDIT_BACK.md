# Edit-back and mark-for-deletion

Brain renders every bead to a markdown file (`entries/<kind>/<slug>.md`). By default that is one-way: the database is the record, the file is a view, and nothing you do to the file reaches the bead. This page covers the two opt-in behaviours that change that, and the one rule that never changes: **the database stays the authority.**

Both come from two verdicts in the approved vision ([`VISION.md`](../../VISION.md)): editing a rendered file may pass back into its bead, per store, if the store wants it; and deleting a rendered file never deletes a bead.

## Turning edit-back on

Edit-back is declared **per store** and is **off by default**. The declaration lives in the store registry, `~/.config/brain/stores.yaml`, as `edit_back: true` on the store's entry:

```bash
bd stores edit-back task on      # this store accepts edits from its rendered files
bd stores edit-back task off     # back to one-way
bd stores list --verbose         # the MODE column shows one-way or edit-back per store
```

`bd stores list --json` carries `edit_back` for every store. A store with no entry in the registry has declared nothing and stays one-way.

Edits are imported by an explicit run, never in the background:

```bash
BD_NAME=task bd render-import     # or run it through the store's wrapper: task render-import
```

Run it under a store wrapper (`BD_NAME` pinned): the pass is scoped to one store and refuses to run without one.

## What an edit does

For a store that accepts edit-back, a changed file updates **the bead its `id:` frontmatter names**. Exactly five fields are importable:

| File | Bead field |
|---|---|
| `title:` | title |
| `status:` | status |
| `priority:` | priority (integer 0–4) |
| `labels:` / `tags:` | labels (added and removed to match) |
| the body after the `# {title}` heading | description |

Everything else in the file (`id`, `kind`, `slug`, `created`, `updated`, metadata, and any key you add) is substrate-owned. A difference there is never a change. After an applied edit the normal render runs and the file converges with the row.

Every applied edit prints old → new:

```
applied  …/alpha-note.md  eb-1fy: title: Alpha note → Alpha note EDITED; description: original alpha body → edited alpha body
```

The previous values also stay in Dolt history (`bd history <id>`), so the losing side of an edit is recoverable, not overwritten.

### A store that does not accept edits

The same run in a one-way store writes nothing. It reports the edit it ignored and says why:

```
ignored  …/alpha-note.md  eb-8va: title: Alpha note → Alpha note EDITED
render-import: this store's registry entry does not declare edit-back, so edits are reported here and written nowhere
```

## Two writers: who wins

Once files are an input there are two writers. The rule:

1. **The database wins by default.** Nothing the file says about the five fields reaches the bead unless the store declared edit-back *and* the file passes every check below.
2. **When the store accepts edit-back, the file wins over the row on those five fields, but only if the file was edited from the current record.** The file's `updated:` stamp is the row's updated time at the render it came from. If the row has changed since, the file is **stale**. It is a late copy delivered by the synced folder, and importing it would silently revert newer data. It is refused (`stale-file`) with both stamps named. Re-render (`bd render <id>`) and redo the edit on the fresh file.
3. **The losing side is always visible**: old → new on every applied or ignored edit, a named refusal on every stale or ambiguous one, and Dolt history for the replaced values.

The stale guard compares stamps to the second; a row changed in the same second as the render it is compared against is not detectable.

## Deleting a file marks the bead, never deletes it

If a rendered file disappears, `bd render-import` marks the bead it belonged to. The mark is the label **`marked-for-deletion`**:

- **Who can see it:** anyone, on every read: `bd show <id>`, `bd list --label marked-for-deletion`, and `bd render-marks list`.
- **What it does:** while the mark stands, every render skips the bead. `bd render <id>` and `bd render-all` print `skipped: … marked for deletion` and write nothing. The file is not silently re-created, and a re-render never clears the mark. Edits to a marked bead's file are refused (`edited-while-marked`).
- **How it is cleared:** only by a human, with `bd render-marks clear <id>`. The label is removed and the normal render re-creates the file. No render path and no import clears it.
- **What it never does:** delete the bead. If the deletion was meant, `bd delete <id>` is a separate, deliberate act.

Deletion detection depends on the **render manifest** (`entries/.render-manifest.json`), which the renderer writes as it renders. It is how the run tells "deleted" from "never rendered". If the manifest is absent, deletions are not scanned and the run says so; run `bd render-all` once to populate it. A moved or renamed file reads as a deletion of the old path (the bead is marked) and a refusal of the new one (`not-manifested-path`). Both are visible.

## Refusals

A refusal is loud, named, and writes nothing. The run exits 1 if any file was refused.

| Code | Meaning |
|---|---|
| `cannot-read` / `cannot-parse` | the file is unreadable, or its frontmatter is malformed |
| `no-name` | the frontmatter has no `id`, so the file names nothing |
| `unknown-bead` | the file names a bead that does not exist in this store, and the id is named |
| `outside-namespace` | the id belongs to another store's namespace (the file is misplaced) |
| `slug-mismatch` | the file's slug is not the named bead's render key |
| `not-manifested-path` | the file is a copy at a path the manifest never recorded for that bead |
| `stale-file` | the row changed after the render the file was edited from |
| `unversioned-file` | no usable `updated:` stamp, so the file's version cannot be established |
| `ambiguous-labels` | `tags:` and `labels:` disagree |
| `ambiguous-body` | the `# {title}` heading disagrees with both the file's and the bead's title |
| `priority-unreadable` | priority is not an integer 0–4 |
| `edited-while-marked` | the bead carries the deletion mark |
| `write-failed` / `lookup-failed` | the substrate refused the write, or the lookup failed |

On an embedded-engine store the `outside-namespace` check accepts only the store's own prefix, because the prefix-ownership record is not readable there. That is stricter than the record and errs toward refusing.
