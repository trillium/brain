# docs/brain/

Brain-specific documentation. **Separate from `docs/` root**, which is bd's documentation home and stays untouched to keep upstream rebases clean.

Anything that's specifically about the brain layer — the `kind` discriminator, the exfiltration hook, the FTS5 cache, the Pulse `/brain/*` module, the v0.2 → Dolt migration — lives here. Anything that's about bd (Dolt backend, federation, multi-remote, the existing CLI verbs) stays in `docs/` root.

## What lives here

- [`WHAT_IS_BRAIN.md`](WHAT_IS_BRAIN.md) — plain-English explainer with ASCII diagrams and Given/When/Then scenarios. **brain IS bd, renamed.** One binary that answers to its installed name, one bag of brain docs (kind ∈ {task, knowledge, both, isa}), the brain-added verb family (`new`, `link`, `related`, `recast`, …), two added edge types (`extends`, `learned-from`), and a markdown-exfiltration hook. Plus the layer the rename framing grew into: a store federation — many named stores, one binary, one search — being consolidated into a single database with namespaces. Start here if you've never used brain before. (Replaces the earlier `BRAIN_VS_BD.md` whose "verb-vocabulary lens over bd" framing was wrong — see `divergence/0006`.)
- [`WHAT_BRAIN_ADDS.md`](WHAT_BRAIN_ADDS.md) — the differential feature inventory: every feature brain has that upstream beads does not, as one scannable list with state and evidence per feature, against the pinned upstream ref (`v1.1.0-rc.1` → `fa4dce454`). Read after `WHAT_IS_BRAIN.md` for the what; this is the what-and-proven-where.
- [`VOICE_IDENTIFIERS.md`](VOICE_IDENTIFIERS.md) — the additive `name` field on every issue-bearing JSON payload: derivation rule, collision behaviour, why `id` stays canonical, and how an agent should present a bead when speaking.
- [`MERGING_DATABASES.md`](MERGING_DATABASES.md) — the workflow for merging several databases into one with `bd brain unify` and keeping the merge current with `unify replay`: when to rebuild instead of replay, what a replay refuses to do, the verifier (`--reference live`) as the acceptance test, and the collision behaviour a replay inherits from the build. See `../../divergence/0027-brain-unify-replay.md`.
- [`PREFIX_RELEASE.md`](PREFIX_RELEASE.md) — `bd store-prefix release` and `history`: how to give a runtime prefix claim up, every refusal path, why a beads-carrying prefix can never be released (absolute, no override), the append-only `brain_store_prefix_events` trail, and the stated pre-adoption gap. The decided design lives in [`../design/brain-prefix-release.md`](../design/brain-prefix-release.md) (see `../divergence/0021-brain-prefix-release-design.md`).

This directory will grow as brain v0.3 is built. Beyond the explainer above, expected residents:

- `exfiltration-hook.md` — how `BrainExfiltrationDecorator` stacks on `HookFiringStore` and writes `entries/{kind}/{slug}.md` on every mutation.
- `fts5-cache.md` — the new `internal/storage/fts/` package, schema, column weights, and rebuild strategy.
- `kind-discriminator.md` — how `kind ∈ {task, knowledge, both}` rides on `issues.issue_type` with no schema migration.
- `reconciler.md` — `brain reconcile` and `brain reconcile --check`, idempotence guarantees, orphan removal.
- `v02-migration.md` — the one-shot `brain migrate-v02` importer that reads brain v0.2's `brain.json` + frontmatter and INSERTs Dolt rows.
- `pulse-brain-module.md` — the `/brain/*` Next.js ISR module, mirrored from the `/plans/*` precedent.

- [`archive/`](archive/) — retired documents kept as frozen historical records. Currently the retired v0.3 ISA ([`archive/ISA-v03.md`](archive/ISA-v03.md)).

Each doc here is paired with a divergence entry in `../divergence/` that records the commit that introduced or changed it.

## Canonical spec

The root-level project ISA document is **retired** (2026-10-07, per the captain-approved [`../../VISION.md`](../../VISION.md): "an ISA is a store type, not a document"). ISA as a record type lives in the store — `kind=isa`, `brain new isa`, the `isa-*` verb family — not as a spec document at the repo root.

- [`archive/ISA-v03.md`](archive/ISA-v03.md) — the retired brain v0.3 ISA, kept as a frozen historical record. Its decision log, constraints, capability audit, ISCs and anti-criteria survive there; nothing further is written into it.
- [`../../VISION.md`](../../VISION.md) — the approved vision; the document the agents doing the work now design against.
- [`WHAT_BRAIN_ADDS.md`](WHAT_BRAIN_ADDS.md) — the differential inventory; the closest thing to a current, evidence-backed feature spec.

## Change history

The divergence trail records every code-changing commit on brain with its rationale:

- [`../../divergence/`](../../divergence/) — divergence trail. Start at [`README.md`](../../divergence/README.md) for the mechanism, then walk the numbered docs.

## bd's docs

For anything bd-related (the upstream parent project):

- [`../adr/`](../adr/) — bd's accepted architecture decision records.
- [`../design/`](../design/) — bd's design docs (Dolt concurrency, KV store, OTel).
- [`../`](../) — bd's root docs directory (CLI reference, FAQ, internals, Dolt backend, federation, integrations).

Keep brain-specific docs out of those locations.
