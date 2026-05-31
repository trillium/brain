---
id: 0003
title: modularity-first architecture
isc: [ISC-100, ISC-104, ISC-105, ISC-106, ISC-107, ISC-108, ISC-109, ISC-111, ISC-117, ISC-118, ISC-119, ISC-120, ISC-121, ISC-122, ISC-123, ISC-124, ISC-125, ISC-126, ISC-127, ISC-128, ISC-129, ISC-200, ISC-201, ISC-202, ISC-203, ISC-204]
status: landed
created: 2026-05-31
updated: 2026-05-31
commits: []
touches: [ISA.md, divergence/0003-modularity-first.md]
upstream_rebase_notes: |
  Decision content is brain-only — lives in `ISA.md` under
  `## Decisions` → `### First-Tranche Decisions (2026-05-31)` as the
  5th bullet, and in this divergence doc. The five named interface
  packages (`internal/brain/<thing>/`) and the per-verb Cobra file
  convention (`cmd/bd/brain_*.go`) are brain-only naming and have no
  upstream equivalent in bd. On any bd → brain sync that touches
  ISA.md or `cmd/bd/brain_*.go`, resolve conflicts with `ours`.
---

# Why

Trillium's direction on 2026-05-31, verbatim:

> "It is OK if we pick the wrong thing, it is better to pick the wrong thing and build out portions of the code then wait for my answers. As long as I know the choices that have been made and we can address them, it is OK. Make sure to design the code so it is modular, that is the most important part. We would like to be able to swap out parts and plug in other parts in case any of the parts we have built are not correct."

The point: build forward, but engineer modularity so wrong picks are cheap to swap. v0.3 will pick defaults eagerly and ship working code rather than wait on ratification. Every "wrong pick" must be quarantined behind a swappable interface so reversal is hours, not days. This decision is what gives the other four First-Tranche decisions their escape hatches — Go-only stays cheap because each Go subsystem is one interface and one default impl; the soft sunset stays cheap because the importer is one interface, not a re-shaped storage layer.

Plain English version: pick now, but pick behind a door. If we open the door later and find we picked wrong, we change one file, not the whole room.

# What changed

Five interface boundaries are named as the load-bearing seams of brain v0.3. Each one is the place where a future Forge run touches the system to swap a default impl for a different impl. Each lives in its own Go package so it has its own test surface.

1. **`BrainVerb`** — interface every brain CLI verb implements.
   - **Location:** `internal/brain/verb/` (interface) + `cmd/bd/brain_<verb>.go` (thin Cobra wrappers, one file per verb).
   - **Purpose:** Decouple Cobra command wiring from verb behavior. Each verb is a thin Cobra file that delegates to its `BrainVerb` implementation. Files: `brain_new.go`, `brain_show.go`, `brain_list.go`, `brain_link.go`, `brain_related.go`, `brain_jot.go`, `brain_legacy.go`, etc.
   - **Swap example:** Replace `brain new` behavior by writing a new `BrainVerb` impl and changing one line in `brain_new.go`. The Cobra wiring stays untouched.

2. **`Exfiltrator`** — interface `Render(node) error`.
   - **Location:** `internal/brain/exfiltrator/` (interface + default impl).
   - **Purpose:** Render a brain node out of Dolt to some external surface. Default impl writes markdown to `~/data/knowledge/entries/{kind}/{id}.md`. Wired via a `HookFiringStore`-style decorator on the storage layer so every commit fires the renderer.
   - **Swap example:** Drop in a different `Render()` to emit HTML, JSON, or a Pulse-shaped manifest. The storage layer, the verbs, and the schema do not move.

3. **`Reconciler`** — interface `Reconcile(ctx, scope) (Report, error)`.
   - **Location:** `internal/brain/reconciler/` (interface + default impl).
   - **Purpose:** Read filesystem ↔ DB, return a diff report, and apply (or refuse to apply) idempotent fixes. Default impl is markdown-vs-Dolt.
   - **Swap example:** Replace the conflict-resolution policy (e.g., DB-wins instead of report-then-apply) or point `Reconcile` at a different reconciliation target without touching the verb that calls it.

4. **`SearchBackend`** — interface `Index(node); Search(query) []Result`.
   - **Location:** `internal/brain/search/` (interface) + `internal/storage/fts/` (default sqlite FTS5 impl).
   - **Purpose:** Index brain nodes for search and serve queries. Default impl is sqlite FTS5 to satisfy ISC-126-129.
   - **Swap example:** Swap in tantivy, bleve, postgres-fts, or an external service without touching CLI verbs. `brain search` keeps its surface; the backend changes underneath.

5. **`LegacyImporter`** — interface `Import(source) (Report, error)`.
   - **Location:** `internal/brain/legacy/` (interface + default impl).
   - **Purpose:** One-shot migration from a prior brain format into Dolt via the storage interface. Default impl reads brain v0.2 JSON + markdown from `~/data/knowledge/` (203 entries) and writes nodes + edges into Dolt.
   - **Swap example:** Add a second importer for Obsidian, Logseq, or a raw markdown tree. The verb `brain legacy import` accepts a `--source` flag that selects the `LegacyImporter` impl; storage doesn't move.

**Per-verb file convention.** Every brain CLI verb gets its own file under `cmd/bd/` named `brain_<verb>.go`. The file is a thin Cobra wrapper that delegates to a `BrainVerb` impl. This mirrors bd's existing one-file-per-subcommand pattern. Swap = replace one file.

**Per-package engine convention.** Every interface lives in `internal/brain/<thing>/` as its own Go package. Default impls live in the same package. Tests live next to the package. This means each seam has its own focused test surface and the bd codebase (`internal/storage/`, `cmd/bd/`) is not polluted with brain-only types.

**Coexistence link.** First-Tranche Decision #3 already says brain ships alongside bd as a separate binary with a separate config dir. This decision is what makes the eventual `.brain/` config dir + `brain` binary rename a one-file swap, not a refactor: the seams are already named, the verb files already isolated, the engines already in their own packages.

No code changes land in this commit. This is documentation only — the decision and its naming. Code follows in subsequent commits that implement each seam against ISCs 100, 104-109, 117-129, and 200-204.

# Brain-spec link

[ISA.md](../ISA.md) — see `## Decisions` → `### First-Tranche Decisions (2026-05-31)` → Decision #5 (modularity-first architecture).

Sections most relevant to this decision:

- `## Decisions` → `### First-Tranche Decisions (2026-05-31)` → Decision #5 — the canonical bullet with What/Why/How-to-apply.
- `## Constraints` — already says brain binary is Go; this decision adds the package-shape and per-verb-file conventions on top.
- `## Criteria` → CLI alias ISCs (104-109) — each verb covered by Decision #5's BrainVerb interface and `cmd/bd/brain_<verb>.go` convention.
- `## Criteria` → FTS5 ISCs (126-129) — covered by Decision #5's SearchBackend interface.
- `## Criteria` → reconcile/exfiltrate ISCs (117-125) — covered by Decision #5's Exfiltrator + Reconciler interfaces.
- `## Criteria` → schema and storage ISCs (100, 200-204) — covered by Decision #5's "engines in their own packages" convention so the storage layer stays unpolluted.

# Modularity test

Concrete escape hatches future-Trillium can audit:

1. **Replace `brain search`'s sqlite FTS5 with bleve.** Write a new `SearchBackend` impl in `internal/brain/search/bleve.go`, change the construction call in the search verb, ship. Storage schema, CLI surface, and verb files do not move. If this is more than a one-day swap, the interface failed.
2. **Replace markdown exfiltration with HTML.** Write a new `Exfiltrator` impl in `internal/brain/exfiltrator/html.go`, change one line in the storage decorator wiring, ship. Verbs, schema, and reconcile do not move. If reconcile breaks because it assumed markdown, the seam was leaky.
3. **Add an Obsidian importer alongside the v0.2 importer.** Write a new `LegacyImporter` impl in `internal/brain/legacy/obsidian.go`, register it behind a `--source=obsidian` flag in `brain_legacy.go`, ship. v0.2 importer keeps working. If the new importer requires changes to nodes or edges schema, the interface was too narrow.
