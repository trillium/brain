---
id: 0001
title: brain repo genesis — fork from bd
isc: [ISC-100, ISC-101, ISC-150, ISC-155, ISC-156]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: [9de1765a533ef24f761e484d244a262dde87b7aa]
touches: [ISA.md, divergence/README.md, divergence/0001-brain-repo-genesis.md, docs/brain/README.md]
upstream_rebase_notes: |
  This commit establishes the brain fork. Future bd→brain syncs via
  `git fetch upstream` + cherry-pick should preserve this genesis intact;
  do not rebase across it. The files this commit adds (ISA.md at repo root,
  the divergence/ directory, docs/brain/) are brain-only and will never
  exist upstream — they should always merge cleanly with `ours` strategy
  on those paths if a conflict ever surfaces.
---

# Why

brain v0.3 is being built as an in-place Go fork of bd rather than a separate TypeScript harness around it. The decision was ratified on 2026-05-31 against the prior expectation that brain v0.3 would extend the brain v0.2 TS codebase at `~/data/knowledge/`.

The reframe that drove the fork: language preference at the executor side is a phantom axis. The assistant writes the code, so "TS vs Go" stops being a cost to the human. Once that axis collapses, the only remaining axis is dependability, and fork-direct wins on every sub-axis:

- **Single writer to Dolt** — one process owns mutations. No second-process race, no `dolt sql-server` lifecycle to babysit.
- **Transaction atomicity** — task writes, knowledge writes, and edge writes all land inside one Dolt transaction. No "task committed but edge failed" half-states.
- **Minimal integration seams** — fewer process boundaries means fewer places state can disagree with itself. bd's existing Dolt connection, migration runner, and hook decorator are reused, not reimplemented across a process boundary.
- **Derived markdown rebuildable from canonical store** — `brain reconcile` walks Dolt and rewrites every markdown file from scratch. If the markdown drifts, the canonical store is the answer, not a manual edit.

Pulse stays a read-only markdown consumer at `/brain/*`, mirroring the proven `/plans/*`, `/wiki/*`, and `/status/*` pattern. Pulse never opens a Dolt connection. That separation is the safety net: the database can be replaced, the renderer can be replaced, the words survive in plain markdown.

Full rationale lives in `../ISA.md` under `## Decisions` (2026-05-31 entries on Dolt-source substrate and fork-direct implementation) and in the project memory note `project-brain-v03-lift`.

# What changed

Only the fork itself. No code changes in this commit. Specifically:

- **`ISA.md` at the repo root** — the canonical brain v0.3 ISA, imported verbatim from `~/data/knowledge/ISA.md` (committed there at `0651374`). This is the source of truth for what brain is and what done looks like. Includes the full ISC table (ISC-100 through ISC-150), the kind-discriminator model, the exfiltration-hook design, and the capability-inheritance audit that revised lift posture to MEDIUM-LOW.
- **`divergence/`** — the divergence trail directory plus its `README.md` describing the mechanism (frontmatter contract, status values, body sections, `divergence:skip` trailer exception, naming convention).
- **`divergence/0001-brain-repo-genesis.md`** — this file. Establishes the trail with the genesis entry.
- **`docs/brain/README.md`** — brain-specific docs index, kept separate from `docs/` root (which is bd's documentation home — touching it would invite rebase pain).

No file under `cmd/`, `internal/`, `pkg/`, or `docs/` (outside `docs/brain/`) is modified. bd's behavior is unchanged. The brain binary does not exist yet.

# Brain-spec link

[ISA.md](../ISA.md) — the canonical spec this fork is converging on.

Sections most relevant to this genesis:

- `## Constraints` — brain binary is Go, fork of bd; CLI is the only writer; Pulse never opens a Dolt connection.
- `## Decisions` (2026-05-31) — Dolt-source substrate ratified; fork-direct implementation ratified on dependability grounds.
- `## Decisions` (2026-05-31) — capability-inheritance audit; lift posture revised MEDIUM → MEDIUM-LOW.
- `### Capability Audit (2026-05-31)` — per-ISC inheritance map from bd.
