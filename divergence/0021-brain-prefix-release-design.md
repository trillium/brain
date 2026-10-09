---
id: 0021
title: prefix release and history verbs, with the design they implement
isc: []
status: landed
created: 2026-10-07
updated: 2026-10-08
commits: [a6040bfe9, e266467a5]
touches:
  - docs/design/brain-prefix-release.md
  - divergence/0021-brain-prefix-release-design.md
  - cmd/bd/store_prefix.go
  - cmd/bd/store_prefix_test.go
  - internal/storage/issueops/unified_namespaces.go
  - internal/storage/issueops/unified_namespaces_test.go
  - internal/storage/issueops/store_prefix_events_test.go
  - internal/brainunify/schema.go
  - docs/brain/PREFIX_RELEASE.md
  - docs/brain/README.md
  - docs/brain/WHAT_BRAIN_ADDS.md
  - docs/CLI_REFERENCE.md
upstream_rebase_notes: |
  The design doc, `docs/brain/PREFIX_RELEASE.md` and the `store-prefix`
  section of `docs/CLI_REFERENCE.md` are brain-only — resolve `ours`.
  `cmd/bd/store_prefix.go`, `internal/storage/issueops/unified_namespaces.go`
  and `internal/brainunify/schema.go` are brain-only files (upstream has no
  unified database or namespace record). The change is additive: a new table
  (`brain_store_prefix_events`), two new subcommands, and a same-transaction
  event append inside `RecordStorePrefix`.
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
left open (history surface, claim-event backfill, transfer urgency) were
unchanged and stayed open at this point in the day — all three were decided
later the same day; see the update below, and the design doc is now free of
open questions.

## Update 2026-10-07: the three remaining open items decided

The design doc left three items open to the captain after the
beads-carrying-release decision; all three are now **decided 2026-10-07**
and recorded in `docs/design/brain-prefix-release.md` (§Decided by the
captain items 2–4, §Decided items (formerly "Left to the captain"), and in
the body of §Question c and §Question d), each with its rejected or
deferred alternative kept visible:

- **History surface:** the `bd store-prefix history <prefix>`
  **subcommand**, not a `list --history` flag — clearer and discoverable,
  and it sits with the other `store-prefix` verbs; the flag shape is
  recorded in the design as the rejected alternative.
- **Claim events:** recorded **from adoption onward**; existing runtime
  rows are **not** backfilled with a synthetic `claim` event per row — an
  honest boundary beats fabricated history. The resulting gap is stated
  plainly in the design: pre-adoption claims have no event row.
- **Transfer urgency:** keep the two-act release-then-claim (c1) now; the
  atomic move (c2) is named as a deliberate **fast-follow** to be built if
  contested transfers actually appear, not an indefinitely deferred
  nice-to-have.

With these, the design has no open questions left. This update is
documentation-only: no code, no verb, no behaviour change, nothing
implemented, and the absolute-refusal decision already recorded above is
untouched and nowhere contradicted. One leftover repaired while editing:
the design's test-list item still named "the override" after the previous
revision had removed that surface; the stray mention now reads as the
claim-event append test (and the absence of backfill rows) instead.

## Update 2026-10-08: implemented

The decided design is built. `bd store-prefix release <prefix> --confirm
[--reason]` frees a runtime-claimed prefix that carries no beads and
`bd store-prefix history <prefix>` shows the trail. Every claim and release
appends a row to the new append-only `brain_store_prefix_events` table in the
same transaction as the ownership change, so no flag combination changes
ownership silently. Refusals: no `--confirm`; prefix not recorded; caller not
the owner's namespace (no `--store` escape); build-decided prefix; any live
bead under the prefix (absolute — no `--with-beads`). Claims are recorded from
adoption onward; pre-adoption rows have no event and `history` says so rather
than inventing one. The atomic `move` stays the named fast-follow.

Proof: behavioural transcripts on a scratch copy of the merged database
(claim → release → re-claim by another store; refusal with a live bead;
refusal on a build-decided prefix; history with reasons; attempted silent
rewrites refused), recorded in
`/Users/mini0/fm_home/mini0-ops/data/brain-prefix-release-build/report.md`.
Not deployed; no store re-pointed. See the design's §Implementation notes for
the shapes chosen where it left a mechanism open, and
[`docs/brain/PREFIX_RELEASE.md`](../docs/brain/PREFIX_RELEASE.md) for the
user-facing page.

