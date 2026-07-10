---
id: 0018
title: OKF v0.1 conformance — emit `type` field in exfiltrated frontmatter
isc: []
status: superseded-by-isa
isa: isa-6zq   # brain exfil → OKF v0.1 + Obsidian-compliant vault (slug: okf-obsidian-vault)
created: 2026-07-09
updated: 2026-07-09
commits: []
touches:
  - internal/brain/exfiltrator/exfiltrator.go   # renderMarkdown: add `type:` line
  - internal/brain/exfiltrator/exfiltrator_test.go
  - ISA.md
upstream_rebase_notes: |
  Brain-only. `renderMarkdown` in internal/brain/exfiltrator/exfiltrator.go
  has no upstream counterpart; resolve `ours` on any bd → brain rebase.
---

# Why

The Open Knowledge Format (OKF) v0.1 spec
(https://github.com/trayburn/presentation-wiki/blob/main/OKF-SPEC.md) is a
lightweight "markdown tree + YAML frontmatter" knowledge-bundle format built
on the same premise brain already commits to: *if you can `cat` a file you
can read it; Dolt is canonical, markdown is the portable view.* An audit
(2026-07-09) found brain's exfiltration output is **already an OKF-shaped
bundle** — one conformance requirement short.

OKF v0.1 has exactly three hard conformance requirements for a bundle:

1. Every non-reserved `.md` has parseable YAML frontmatter — brain **passes**
   (every entry gets a `---`…`---` block, always).
2. Every frontmatter block contains a **non-empty `type` field** — brain
   **fails**: `renderMarkdown` emits `kind:`, never `type:`.
3. Reserved files (`index.md`, `log.md`) follow spec structure *when present*
   — brain **passes vacuously** (it generates neither).

Requirement #2 is the sole blocker. Everything brain emits that OKF does not
name (`id`, `kind`, `status`, `priority`, `labels`) is legal: OKF *mandates*
that consumers preserve unknown keys and accept unrecognized fields. So the
fix is purely additive — emit a `type` line; change nothing else.

# Decision

`renderMarkdown` will emit `type: {IssueType}` alongside the existing
`kind:` line. OKF `type` is producer-chosen and consumer-tolerant
("BigQuery Table", "Playbook", etc.), so brain's kind values
(`task`/`knowledge`/`both`/`isa`/`bug`/`feature`/…) map onto OKF `type`
with zero semantic loss. We keep `kind` too — it is brain's canonical
discriminator that the reconciler, Pulse, and MemoryRetriever read; `type`
is the OKF-facing mirror.

Chosen: **`type` mirrors `kind` verbatim** rather than a descriptive
remap. Rationale — a remap ("knowledge" → "Concept", "task" → "Work Item")
adds a lookup table and a place for drift, buys nothing under OKF's
tolerate-any-value rule, and would make `type ≠ kind` a surprise for anyone
grepping. Verbatim mirror is the boring, correct choice.

# NOT done in this note (deliberate)

The change is **spec'd, not landed.** `renderMarkdown`'s byte output is
load-bearing for the reconciler idempotence guarantee (ISC-123, see
divergence/0012): adding a frontmatter line changes the byte shape of
*every* entry, so the first reconcile after this lands re-renders every
file in every federated store. That is a store-wide churn event, not a
silent patch — it wants its own ISC, a `render-all` dry-run diff, and a
deliberate reconcile, not a drive-by edit. This note reserves the
decision; the implementing ISC sequences the churn.

# Soft gaps (out of scope, not conformance failures)

Under OKF's "must not reject for missing optional fields" rule, none of
these block conformance; listed for completeness:

- `tags:` alias mirroring `labels[]` — trivial; helps OKF consumers that
  key on `tags`.
- `description` frontmatter (1-line summary) — brain has no summary field;
  the body carries the prose today.
- `resource` (URI) — no brain equivalent.
- `index.md` progressive-disclosure listings — not generated; natural
  add-on to `render-all`. Enables `okf_version: "0.1"` bundle-root
  declaration.
- `log.md` per store — derivable from `created`/`updated`; not emitted.
- Cross-links: OKF expresses relationships as in-body markdown links;
  brain expresses them as Dolt graph edges (`brain link`). Brain's edges
  do not render into the markdown, so an OKF-only consumer won't see them.
  Philosophical difference, not a violation (broken/absent links must be
  tolerated).
