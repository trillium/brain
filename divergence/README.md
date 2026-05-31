# divergence/

This directory holds the **divergence trail** — the running record of how brain departs from upstream bd toward the brain spec (`../ISA.md`).

## Purpose

brain is a fork of [bd](https://github.com/gastownhall/beads). Every code-changing commit on brain must be paired with a divergence doc that answers two questions:

1. **What changed** in this commit.
2. **Why** that change brings the repo closer to what brain is supposed to be.

This is the mechanism that keeps the fork legible. When we rebase against upstream, when we cherry-pick a bd fix in, when we look back six months from now and ask "why does brain do this instead of what bd does," the divergence trail is the answer.

## When a divergence doc is required

Every commit that changes code, schema, build, or runtime behavior on the `main` branch.

## When a divergence doc is NOT required (exception)

Commits with the trailer `divergence:skip` in the commit message are exempt. Intended uses:

- **Upstream pulls** — `git fetch upstream && git merge upstream/main` and the merge commits that result. The upstream side already has its own commit history.
- **Mechanical infrastructure** — pre-commit hook installation, CI plumbing, formatter runs.
- **Doc-only changes** — README typos, comment fixes, formatting in this `divergence/` directory itself.

Use the trailer sparingly. If in doubt, write the doc.

A pre-commit hook will eventually enforce this pairing (tracked in `../ISA.md` — see `Decisions` 2026-05-31 and the deferred ISC-152 follow-up). Until then it's enforced by review.

## Naming

`NNNN-<slug>.md` — four-digit zero-padded, chronological:

- `0001-brain-repo-genesis.md`
- `0002-schema-edges-extends-learned-from.md`
- `0003-cobra-brain-new-aliases.md`
- ...

IDs never get reissued. A superseded doc keeps its number; the superseding doc gets the next free number and links back.

## Frontmatter contract

Every divergence doc starts with this YAML block:

```yaml
---
id: 0001
title: short imperative phrase, lowercase
isc: [ISC-100, ISC-101]           # which ISA criteria this commit moves
status: proposed                   # proposed | landed | superseded | reverted
created: 2026-05-31
updated: 2026-05-31
commits: [<sha>]                   # filled in after the commit lands
touches: [path/one, path/two]      # files/dirs changed in this commit
upstream_rebase_notes: |
  Anything a future rebase against upstream/main needs to know:
  conflict hotspots, files that are brain-only, files where upstream
  may have moved on. Plain English.
---
```

### Status values

- **proposed** — written before the commit; describes intended change. Rare; usually you write the doc as part of the commit.
- **landed** — the commit is in `main`. The default for finished work.
- **superseded** — a later divergence doc replaces this one. Add a `superseded_by: NNNN` field and explain why in the body.
- **reverted** — the commit was reverted out of `main`. Keep the doc; add a `reverted_by: <sha>` field.

## Body sections (required)

Three headings, in this order:

### `# Why`

The fork-spec-driven reason for this change. Cite the ISA section, the ISC range, or the decision record that drives it. Plain English — readable on a phone.

### `# What changed`

The minimal, factual list of code/schema/build/runtime changes. Not a diff dump — a scannable summary.

### `# Brain-spec link`

A relative markdown link back to the ISA section this commit advances. Always include `[ISA.md](../ISA.md)` at minimum.

## How to add a new divergence doc

1. Make your code changes.
2. Pick the next free `NNNN` (last entry + 1).
3. Write the doc with `status: proposed` and `commits: []`.
4. `git add` everything together.
5. Commit with a message that references the doc.
6. After the commit lands, edit the doc: set `status: landed` and fill `commits: [<sha>]`.
7. Amend the commit so the doc and commit reference each other consistently. (For the genesis commit this amend is the only path; for normal divergence docs a follow-up commit is fine if you prefer flat history.)

## Index

| ID | Title | Status |
| --- | --- | --- |
| [0001](0001-brain-repo-genesis.md) | brain repo genesis — fork from bd | landed |
