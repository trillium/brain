---
id: 0002
title: first-tranche decisions — Go-only, jot alias, Pulse deferred, soft sunset
isc: [ISC-130, ISC-131, ISC-132, ISC-133, ISC-134, ISC-135, ISC-136, ISC-137, ISC-138, ISC-139, ISC-140, ISC-141, ISC-251]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: [<TBD>]
touches: [ISA.md, divergence/0002-first-tranche-decisions.md]
upstream_rebase_notes: |
  Decision content lives entirely in ISA.md `## Decisions` (the new
  "First-Tranche Decisions (2026-05-31)" subsection) and in this divergence
  doc. Upstream bd has no equivalent surface — both files are brain-only
  paths. On any bd → brain sync that touches ISA.md, resolve conflicts
  with `ours`. This commit also surgically rewrites one line in `## Goal`
  (line 86 region) so the goal statement stops contradicting Decision #3
  (Pulse deferred). That rewrite is brain-only too.
---

# Why

Trillium asked on 2026-05-31 for "sensible defaults that center around the tool being dependable, written in one programming language, with all choices documented." These four decisions are the answer. Together they define what v0.3 ships and what slides to v0.3.1.

The decisions cascade from a single constitutional gate:

- **Decision 1 (Go-only)** is the gate. v0.3 is one binary, one toolchain, one test runner, one set of failure modes. Dependability comes from shrinking the verifiable surface to something a single person can hold in their head end to end.
- **Decision 3 (Pulse deferred)** falls directly out of Decision 1. Pulse is Next.js, which is TypeScript. Either Pulse waits, or the Go-only gate breaks. Pulse waits. v0.3 ships CLI-only and v0.3.1 picks up the read-only `/brain/*` module as its own focused tranche.
- **Decision 4 (soft sunset)** is shaped by Decision 1 in a different direction. The v0.2 → Dolt importer has to exist for migration, and that importer has to be Go (the throwaway TS brain-v0.2 codebase is being retired). The safety net is to ship the importer plus a read-only `brain legacy *` namespace in v0.3.0 so both surfaces stay live during the proving window, then remove legacy in v0.3.1 once parity is verified. Hard cutover at v0.3.0 risked data loss if the importer had edge-case bugs; the soft sunset removes that risk class.
- **Decision 2 (jot alias)** is the one decision that is not a cascade — it is a UX continuity pick. The Capability Audit (above, 2026-05-31) showed that bd already ships every primitive the brain v0.2 `jot` verbs needed: `bd mol wisp create/list/show` + `bd promote` + `bd mol wisp gc`. brain v0.2 muscle memory is `brain jot save`, not `brain mol wisp create`. Wrapping the bd verbs in a ~50 LOC Cobra alias namespace (`cmd/bd/brain_jot.go`) gives Trillium the verb he reaches for, without duplicating mechanism. It fits the Go-only gate because the wrappers are Go.

Plain English version: pick one language, ship the smaller thing first, keep a safety net for the old data while we prove the new substrate, and don't make the user re-learn the verbs they already type.

# What changed

Four decisions land in `ISA.md` under a new dated subsection `### First-Tranche Decisions (2026-05-31)`, inside `## Decisions`, positioned after the Capability Audit and before "Preserved from v0.2."

Per-decision summary:

1. **v0.3 is Go-only (constitutional).** Reject any ISC that introduces non-Go code into the v0.3 critical path. The gate from which #3 and #4 follow.
2. **`brain jot` is a Cobra alias namespace.** New file `cmd/bd/brain_jot.go`, ~50 LOC. Wraps `bd mol wisp create/list/show` + `bd promote` + `bd mol wisp gc`. Added as ISC-251 in Criteria + Test Strategy + Features.
3. **Pulse `/brain/*` module deferred to v0.3.1.** ISC-130 through ISC-136 tagged `(DEFERRED v0.3.1)` in the Criteria section header and per-ISC rows; pulse-brain-module row in the Features table reflects the same.
4. **v0.2 → v0.3 soft sunset.** Section retitled "v0.2 to v0.3 migration (soft-sunset model)." ISC-140 ships in v0.3.0 (legacy reads stay live); ISC-141 deferred to v0.3.1 (legacy removed after parity).

Surgical rewrites required for consistency with the new decisions:

- `## Goal` (line 86 region): rewrote the one-paragraph goal so it stops promising "one Pulse module at `/brain/*`" as part of v0.3 done-state. New text states v0.3 ships Go CLI + Dolt + soft-sunset legacy, with Pulse explicitly listed as v0.3.1 scope.

One Changelog entry added under `## Changelog` summarizing the conjecture/refutation/learning/criterion_now for the Go-only + scope cut.

This divergence doc itself (`divergence/0002-first-tranche-decisions.md`).

No code changes. No file under `cmd/`, `internal/`, `pkg/`, or `docs/` is touched.

# Brain-spec link

[ISA.md](../ISA.md) — see `## Decisions` → `### First-Tranche Decisions (2026-05-31)` for the canonical record.

Sections most relevant to these decisions:

- `## Decisions` → `### First-Tranche Decisions (2026-05-31)` — the four ratified decisions with What/Why/How-to-apply each.
- `## Constraints` — already says brain binary is Go (aligned with Decision #1).
- `## Goal` — rewritten to reflect Decision #3 (Pulse deferred) and Decision #4 (soft sunset).
- `## Criteria` → `### Jot alias namespace` — new ISC-251 from Decision #2.
- `## Criteria` → `### Pulse /brain/* module (DEFERRED v0.3.1)` — Decision #3 applied.
- `## Criteria` → `### v0.2 to v0.3 migration (soft-sunset model)` — Decision #4 applied.
- `## Features` — `jot-alias` promoted from optional to landed; `pulse-brain-module` tagged DEFERRED v0.3.1.
- `## Changelog` — one-line summary entry of the decision set.
