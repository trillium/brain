---
id: 0021
title: design for prefix release and transfer (no release verb exists yet)
isc: []
status: proposed
created: 2026-10-07
updated: 2026-10-07
commits: [a6040bfe9]
touches:
  - docs/design/brain-prefix-release.md
  - divergence/0021-brain-prefix-release-design.md
upstream_rebase_notes: |
  Doc-only entry; no code conflicts expected. `docs/design/brain-prefix-release.md`
  is brain-only — resolve `ours`. It proposes a release/transfer design for the
  runtime prefix record (brain_store_prefixes, the R3 surface in
  divergence/0020's deferred list); nothing is implemented. When the design is
  accepted and built, the implementing commit gets its own divergence entry and
  this one flips to `superseded` (or `landed`, if only the doc lands).
---

# Why

The namespace-model work (R3, `bd store-prefix add/list`) deliberately shipped
the claim side only: the record is never rewritten silently, so a store that
wants to give a prefix up has no verb, and the only paths left are raw SQL —
unrecorded and unaudited. The captain deferred the release/transfer flow
rather than inventing it silently ("taking a prefix back needs a designed
flow"). This entry lands the design, not the flow.

# What changed

- **`docs/design/brain-prefix-release.md`** (new) — the design, in the
  `brain-single-database.md` register. It answers, with options and
  consequences for each: who may release (recommendation: the owning store's
  own wrapper plus an explicit `--confirm` — namespace identity proves
  authority, the flag proves intent); what release means (the row is deleted,
  the prefix returns to exactly its pre-claim state, beads keep their ids and
  become wide-only until someone claims the prefix again); the transfer path
  (recommendation: release-then-claim as two acts now, atomic `move` as a
  designed follow-on only if the unclaimed window becomes a live problem);
  what is recorded (recommendation: an append-only `brain_store_prefix_events`
  audit table written in the same transaction as every ownership change —
  claims included — which is what makes a recorded, transactional rewrite of
  the ownership row consistent with the never-silently rule); the refusal
  semantics (build-decided prefixes refused outright; beads-carrying prefixes
  refused outright — absolute refusal, no `--with-beads` override, decided
  2026-10-07, see below; no owner impersonation via `--store`; racing acts
  serialise and refuse loudly); and
  the cutover interaction (independent in mechanism, meaningful only after
  cutover in practice).
- **`docs/brain/README.md`** — index pointer to the design doc.

# Brain-spec link

Serves the fork's own namespace model — upstream bd has no unified database,
no `brain_store_prefixes`, and no runtime prefix surface, so there is no
upstream precedent for releasing one. The design's founding constraint is the
one recorded in divergence 0020's inventory (B-10): the record is never
rewritten *silently*. This design keeps every change loud, deliberate and
event-recorded; it builds nothing.

# Deliberately not built here

No verb, no code path, no schema change, no behaviour change. The design doc
is the deliverable; the implementing commit (if accepted) writes its own
divergence entry and cites this one.

## Update 2026-10-07: beads-carrying-release order overruled to absolute refusal

The design's recommendation on the one genuinely open question — whether a
prefix that still carries beads may be released under an explicit, audited
`--with-beads` override — **changed to absolute refusal** on 2026-10-07, by
captain's decision: "ABSOLUTE REFUSAL. A prefix that still carries beads
cannot be released; there is no `--with-beads` override." The reasoning
recorded with the decision: it matches the captain's own "refuse rather than
guess" posture; releasing a prefix that still owns beads would let a second
store claim it and make ownership of the existing beads ambiguous; and a
prefix frees only when it carries no beads — migrating beads off it first is
the explicit, auditable path. The audited-override shape is not silently
dropped: `docs/design/brain-prefix-release.md` now documents it as the
rejected alternative (refusal 5 there) with its original reasoning and the
reason it was overruled, so the record shows the road not taken. If a
genuine operational need for an override appears later, adding one is a
small reversible addition, whereas shipping the override first and
withdrawing it is not. This update is documentation-only: no code, no verb,
no behaviour change, nothing implemented. The other three items the design
left open (history surface, claim-event backfill, transfer urgency) are
unchanged and stay open.
