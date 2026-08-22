---
id: 0019
title: compose bead title and body in $EDITOR (bd create --edit, bd edit --append)
isc: []
status: proposed
created: 2026-08-06
updated: 2026-08-06
commits: []
touches:
  - cmd/bd/create_edit.go
  - cmd/bd/create_edit_test.go
  - cmd/bd/create.go
  - cmd/bd/create_input.go
  - cmd/bd/create_proxied_server.go
  - cmd/bd/edit.go
upstream_rebase_notes: |
  `cmd/bd/create_edit.go` and its test are brain-only — resolve `ours`.
  The four edits in upstream files are small and anchored:
    * create.go — the `maybeComposeCreate` block at the top of the
      createCmd RunE (right before the `usesProxiedServer()` branch), the
      `titleLengthError(title)` call after the inline title resolution,
      the `discardComposeDraft()` call after `commandDidWrite.Store(true)`,
      plus the `--edit` flag registration and the `Long` help text.
    * create_input.go — `resolveTitle` now assigns to a local and runs
      `titleLengthError` before returning.
    * create_proxied_server.go — one `discardComposeDraft()` call before
      the output switch.
    * edit.go — the editor lookup and exec were replaced by the shared
      `resolveEditorCommand` / `runEditorOnFile` helpers now living in
      create_edit.go, and `--append` was added.
  If upstream reworks either create path, re-anchor these calls rather
  than taking `ours` wholesale — the surrounding create logic is upstream's.
---

# Why

Reported as robots-fy5m. Trillium pasted a long paragraph of prose into
`brain create` as the title and got:

```
Error: validation failed for issue : title must be 500 characters or less (got 1111)
```

Nothing was created and the text was gone — the CLI rejected the input
without saying where the long half was supposed to go. The request that
came with the bug was the actual gap: *"a way of creating a new bead,
giving it a title and stuff, and then being able to append to the body
with one tool that opens a code editor for me to edit the body."*

Every piece existed separately — `create` took `--description`, `edit`
opened `$EDITOR`, `note` appended — but there was no single command that
takes a title and a real body in one sitting. The result is that anything
longer than a shell one-liner got jammed into the title, which is exactly
the failure above. `create-form` is a TUI with a single-line description
field, so it does not close the gap either.

Federated stores make this worse, not better: a `brain` entry is a
document. Composing a document through `-d "…"` in a shell quote is the
wrong shape for the content the store is for.

# What changed

- **`bd create --edit` / `-E`** opens `$EDITOR` on a git-commit-shaped
  buffer: first line is the title, everything after the first blank line
  is the body. Any title/`--description` already passed prefills it.
- **Bare `bd create` at a terminal** composes instead of erroring. Off a
  terminal (agents, scripts, CI) the old `title required` error still
  fires — now naming `--edit` — so nothing non-interactive changes.
- **Scissors, not `#` comments.** Bead bodies are markdown, so the
  instructions sit below a `# --- >8 ---` cut line and everything at or
  below it is discarded. Markdown headings in the body survive verbatim.
- **An over-long title re-prompts instead of failing.** The buffer comes
  back with the text untouched and a note above the help saying how long
  the title was and that the rest belongs in the body.
- **The draft file outlives a failed create.** The buffer is deleted only
  once the bead exists; if anything downstream fails, the path is printed.
- **`titleLengthError`** replaces the bare validation failure on both
  create paths (inline and proxied) with the length, the limit, and the
  two ways out.
- **`bd edit <id> --append`** opens an empty buffer and appends what you
  write to the field (description by default, or `--notes` / `--design` /
  `--acceptance`), separated by a blank line. This is the "append to the
  body later" half of the request. It is rejected with `--title`.
- `resolveEditorCommand` / `runEditorOnFile` are now shared by `create`
  and `edit` instead of `edit` carrying its own copy.

# Brain-spec link

[ISA.md](../ISA.md) — the brain fork exists so that stores hold documents,
not one-line issue titles. A create path that can only accept a shell
string pushes document-shaped content into the title field, which is what
this entry fixes.
