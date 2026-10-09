---
id: 0020
title: brain feature inventory — the differential list over upstream beads
isc: []
status: landed
created: 2026-10-07
updated: 2026-10-07
commits: [a7ed20bdcf8856c5f00e4e1530d69952a2d2ebe5]
touches: [docs/brain/WHAT_BRAIN_ADDS.md, docs/brain/README.md, divergence/0020-brain-feature-inventory.md]
upstream_rebase_notes: |
  Doc-only entry; no code conflicts expected. The inventory pins upstream at
  tag v1.1.0-rc.1 / commit fa4dce454 and asserts absences by name — when a
  rebase brings upstream landings matching any row (the known near-miss is
  `bd patch`, which upstream later added independently), re-verify that row
  against the then-current upstream ref and amend docs/brain/WHAT_BRAIN_ADDS.md.
  The declared-but-unbuilt section (FTS5, reconciler, Pulse, legacy namespace)
  references ISA.md ISC ranges; the ISA is authoritative for their status.
---

# Why

The captain's ask: one authoritative, evidence-backed list of the features brain supports that upstream beads does not. Nothing in the repo recorded that: `ISA.md` describes intent and criteria, `divergence/` records individual commits against upstream, and `docs/brain/` explains the model — but "what does brain have that bd lacks" existed only as scattered knowledge plus four reporters' home-local reports. `divergence/0005` also warns about the rejected `BRAIN_VS_BD.md` verb-vocabulary framing; this entry delivers the *differential feature inventory* in its place, marked as its replacement companion.

# What changed

- **`docs/brain/WHAT_BRAIN_ADDS.md`** (new) — the inventory. Every feature row carries name, one-line description, location, state (shipped / built+proven / code / declared / deferred-unbuilt), evidence, and the upstream check with its ref. The upstream reference is pinned: `gastownhall/beads` tag `v1.1.0-rc.1` → `fa4dce4548d8d15d5478b9ad7e4f6ee7cbfabaa1`, verified against brain's `cmd/bd/version.go` (`Version = "1.1.0-rc.1"`) and README; absences re-checked at later upstream `a4509deb2` where noted (one near-miss recorded: `bd patch`). The verb-level diff was established mechanically by building both binaries and diffing help: 14 fork-only verbs, 1 fork-only persistent flag (`--wide`), zero upstream-only verbs at the pinned ref. The unification tranche (`brain unify`, namespace model, `--wide`, storeless-mint refusal, `store-prefix`, unified provisioning) is represented as built+proven-not-deployed and cites the four reporters' reports by path. A declared-but-unbuilt section keeps the list honest (FTS5, reconciler, Pulse module, legacy namespace, prefix release verb, cutover itself).
- **`docs/brain/README.md`** — index pointer line for the new doc.

# Brain-spec link

Complements [ISA.md](../ISA.md) — `# What brain adds to beads` in the fork README and the ISA Features table state intent; this inventory states the landed-and-pinned reality with evidence, upstream ref, and the unbuilt remainder (`sections 8/9`).
