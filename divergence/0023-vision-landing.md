---
id: 0023
title: VISION.md landed — the captain-approved vision, verbatim
isc: []
status: proposed
created: 2026-10-07
updated: 2026-10-07
commits: []
touches:
  - VISION.md
  - divergence/0023-vision-landing.md
upstream_rebase_notes: |
  Doc-only entry; no code conflicts expected. VISION.md is brain-only —
  resolve `ours`.
---

# Why

The captain reviewed and approved a vision for brain on an interactive board:
24 hypotheticals across two rounds, verdict recorded as **"In vision"** with
no changes asked for (2026-10-07). This entry records that approval and the
landing of the approved text as `VISION.md` at the repository root, verbatim —
it is the captain's own words, not a draft to improve. The vision states
intent; nothing in it is built or acted on by this landing.

# What changed

- **`VISION.md`** (new) — the approved text, byte-for-byte the approved
  draft (`draft-v4.md`, 98 lines, 11 sections): the identity, the record,
  markdown as a reader's view, the silent-failure class, mechanism over
  symptom, additive change, the federation, events, hooks, the programmatic
  surface, the agent-maintained design record, and scope.

# Acknowledged caveats at approval

Two things were acknowledged at approval and are deliberately NOT acted on
here; they are follow-up work, separately approved:

1. Three sections commit to behaviour that does not exist yet:
   refusal-capable hooks with declared failure policy, durable event
   delivery with retries, and opt-in edit-back from markdown.
2. One answer contradicts what is shipped: the vision says two stores
   holding the same id become a duplication record with new ids for both
   copies, but the built collision rule keeps the winner. The wording stands
   as approved; changing either side is separate work.

# Deliberately not done here

ISA.md not retired or edited, collision rule unchanged, no hooks or event
delivery added, review verdicts not committed (they live in firstmate's
home, not this public repository). Documentation only.
