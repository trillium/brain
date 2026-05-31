# docs/brain/

Brain-specific documentation. **Separate from `docs/` root**, which is bd's documentation home and stays untouched to keep upstream rebases clean.

Anything that's specifically about the brain layer — the `kind` discriminator, the exfiltration hook, the FTS5 cache, the Pulse `/brain/*` module, the v0.2 → Dolt migration — lives here. Anything that's about bd (Dolt backend, federation, multi-remote, the existing CLI verbs) stays in `docs/` root.

## What lives here

This directory will grow as brain v0.3 is built. At genesis it's empty except for this README.

Expected residents:

- `exfiltration-hook.md` — how `BrainExfiltrationDecorator` stacks on `HookFiringStore` and writes `entries/{kind}/{slug}.md` on every mutation.
- `fts5-cache.md` — the new `internal/storage/fts/` package, schema, column weights, and rebuild strategy.
- `kind-discriminator.md` — how `kind ∈ {task, knowledge, both}` rides on `issues.issue_type` with no schema migration.
- `reconciler.md` — `brain reconcile` and `brain reconcile --check`, idempotence guarantees, orphan removal.
- `v02-migration.md` — the one-shot `brain migrate-v02` importer that reads brain v0.2's `brain.json` + frontmatter and INSERTs Dolt rows.
- `pulse-brain-module.md` — the `/brain/*` Next.js ISR module, mirrored from the `/plans/*` precedent.

Each doc here is paired with a divergence entry in `../divergence/` that records the commit that introduced or changed it.

## Canonical spec

The single source of truth for what brain is and what done looks like:

- [`../../ISA.md`](../../ISA.md) — the brain v0.3 ISA. Problem, vision, constraints, ISC table, decision log, capability audit, changelog.

## Change history

The divergence trail records every code-changing commit on brain with its rationale:

- [`../../divergence/`](../../divergence/) — divergence trail. Start at [`README.md`](../../divergence/README.md) for the mechanism, then walk the numbered docs.

## bd's docs

For anything bd-related (the upstream parent project):

- [`../adr/`](../adr/) — bd's accepted architecture decision records.
- [`../design/`](../design/) — bd's design docs (Dolt concurrency, KV store, OTel).
- [`../`](../) — bd's root docs directory (CLI reference, FAQ, internals, Dolt backend, federation, integrations).

Keep brain-specific docs out of those locations.
