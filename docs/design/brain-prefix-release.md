# Releasing and transferring a prefix claim

**Status:** design only — nothing is built, no behaviour changes, no verb is
wired up. Production is untouched. This document proposes; it does not land.
One question left open here — whether a beads-carrying prefix may be released
under an override (question b) — has since been **decided (2026-10-07):
absolute refusal, no `--with-beads` override**; see §Question b and refusal 4
in §Question e.
**Companion material:** the namespace-model report
(`/Users/mini0/fm_home/mini0-ops/data/brain-unify-namespace-model/report.md`,
§Requirement 3), which records the runtime claim surface and the conflict rule
verbatim, and the deferred item in
`docs/brain/WHAT_BRAIN_ADDS.md` ("Prefix release/transfer verb — deliberately
absent this round").

## The problem

Runtime prefix claims landed: `bd store-prefix add <prefix> [--store <store>]`
records a claim in `brain_store_prefixes` with its reason, the prefix is usable
to mint the moment it lands, and the conflict rule is deliberately one-way —
**the record is never silently rewritten**. The first claimant owns it, a rival
claim is refused by name, and there is no release verb at all. That last part
was a deliberate refusal, not an oversight: silent re-homing is exactly what
was rejected.

But "the record is never rewritten" has a cost the refusal message already
names: a store that *wants* to give a prefix up has no way to do so. Today the
only paths are raw SQL against the unified database — unrecorded, unaudited,
and outside every invariant the CLI enforces — or leaving the claim in place
forever. Neither is a designed flow. The design question is how to allow
release and transfer **without** reintroducing silent rewriting: every change
to ownership must be a deliberate, recorded, inspectable act, and every state
the flow cannot verify must end in a refusal, not a guess.

## What exists today

The ownership record is one row per prefix
(`internal/brainunify/schema.go`):

```sql
CREATE TABLE brain_store_prefixes (
  prefix varchar(255) NOT NULL,          -- PRIMARY KEY
  store varchar(128) NOT NULL,           -- the owner (or '__unattributed__')
  owner_reason varchar(64) NOT NULL DEFAULT '',
  declared_by text NOT NULL,             -- build-time: who declared it (comma list)
  observed_by text NOT NULL,             -- build-time: who carried beads under it
  bead_count bigint NOT NULL DEFAULT 0,  -- beads observed at claim/decision time
  ambiguous tinyint(1) NOT NULL DEFAULT 0,
  ...
)
```

The `owner_reason` vocabulary so far:

| Reason | Written by | Meaning |
|---|---|---|
| `declared-by-store-config` | build | the store's config.yaml claimed the prefix |
| `store-name-matches-prefix` | build | name equality decided it |
| `no-source-declares-or-matches` | build | unattributed — owner is `__unattributed__` |
| `first-observing-source` | build | |
| `operator-added` | runtime (`bd store-prefix add`) | an operator decision |
| `store-created` | runtime (`brain stores create`) | the store's own-name claim |

The claim path (`issueops.RecordStorePrefix`,
`internal/storage/issueops/unified_namespaces.go`) is transactional, refuses a
rival claim loudly with the owner's name and reason, and extends the owner's
scoped `allowed_prefixes` config row so minting works immediately. The mint
and read paths (`ValidateNamespaceOwnership`,
`resolveNamespacePrefixes` in `internal/storage/dolt/unified_namespace.go`)
resolve everything from this one record: a bead's namespace is the segment of
its id before the first `-`, and a store's narrow view is the prefixes
recorded for it — falling back to the store's own name when it has no rows.

The audit idiom the repo already uses for state changes is the `events` table:
`INSERT INTO events (id, issue_id, event_type, actor, old_value, new_value)`
(`internal/storage/issueops/bulk_ops.go`). The namespace record has no
equivalent today — a runtime claim writes only the `brain_store_prefixes` row.

**Checked against the documented rule:** the implementation
(`RecordStorePrefix`) matches the conflict rule exactly as the namespace-model
report §Requirement 3 records it — unclaimed → recorded, same store → no-op,
different store → loud refusal naming owner and reason. No contradiction
found. Release is also not *impossible* without changing landed code: it needs
one additive table and read/write paths beside the existing ones; nothing
landed must be rewritten for it, though the record's "current ownership"
semantics must be defined precisely enough that release cannot be read as a
contradiction of the never-silently rule (§What is recorded).

## Question a — who may release a prefix

The options, with what each one means, gets, and costs:

**a1. The owning store, through its own wrapper.** `bd store-prefix release
<prefix>` must run under the owner's wrapper: the pinned `BD_NAME` must equal
the row's owner. Authority is proven by namespace identity — the same proof a
claim relies on by default.
*Gets:* symmetric with `add` (whose default target is `BD_NAME`), no new
authority concept, no config to manage.
*Costs:* any process that can run a store wrapper can release — the same trust
level that lets it create and close beads can now give away namespace
capacity. A misdirected command in a cron script or an agent's shell history
becomes a namespace change.

**a2. The operator only, by explicit name.** Release carries `--store <owner>`
and refuses unless `BD_NAME` is *unset or unrelated* — deliberately the mirror
of `add`: the act is attributed to a human decision, not to whatever wrapper
was current.
*Gets:* stronger proof of intent; an agent or script cannot release by
accident while doing ordinary work.
*Costs:* asymmetric with `add` (which happily accepts `--store`), and it makes
the *owning store* unable to shed its own claim without an operator standing
over it — the store is the thing whose view the release changes.

**a3. Deliberate administrative act only: owner's wrapper *plus* an explicit
confirmation flag.** `bd store-prefix release <prefix> --confirm` — refusal
without the flag, and the flag exists precisely so the act cannot happen as a
side effect of anything.
*Gets:* the trust level of a1 with the intent-proof of a2; the confirmation is
recorded in the audit row, so "deliberate" is not just asserted, it is stored.

**Recommendation (not a decision): a3.** Release is the one namespace verb
whose mistake is *subtractive* — claim mistakes are visible and rivalless
(only the owner can hold it), but a release mistake makes beads vanish from
every narrow view at once. The single flag is cheap, and `--confirm` in the
recorded event is what makes the act auditable after the fact. Reject a2's
"operator only" shape: it would forbid a store from legitimately
decommissioning itself, and the operator surface `add` already trusts
(`--store`) shows the home's model is namespace-identity-plus-deliberateness,
not identity-minus-agency.

## Question b — what release means

**What the owner gives up.** After release, the prefix is unclaimed: no store
owns it, `ValidateNamespaceOwnership` refuses minting under it for everyone
(the refusal is the ordinary `ErrPrefixOwnership`, whose message — "claim it
with `bd store-prefix add`" — is now literally true again), and narrow reads
no longer scope to it.

**What happens to the beads.** They keep their ids — nothing about a released
bead's id is ever rewritten, and the design would refuse any flow that
suggested otherwise. The question is ownership of the rows afterwards, and
there are two honest shapes:

**b1. Beads become ownerless (wide-only).** They remain in the merged `issues`
table, visible to `--wide` reads, absent from every narrow view — exactly the
state a build-time unattributed prefix is already in. Whoever claims the
prefix next inherits all of them.
*Gets:* release is simple and total: one act, one meaning, no half-states.
Matches the id-prefix model — ownership is per-prefix, never per-bead.
*Costs:* the releasing store loses read access to its own history in one
command; a `list` that yesterday returned 172 beads returns none of them. That
is the point of releasing, but it should be said plainly, and the refusal
rules in (e) exist to make sure it happens only deliberately.

**b2. Beads keep their former owner until re-claim.** Released prefixes would
need per-bead ownership shadow state, contradicting the design's founding
choice that the prefix record is the *only* thing that is true about
ownership. This is listed only to reject it: it reintroduces exactly the
ambiguity the prefix record exists to remove, and it is what a `store` column
on `issues` would have been (rejected in `brain-single-database.md` §The
mechanism).

**Decision on the beads-carrying-release question (decided 2026-10-07,
close-over of b): absolute refusal.** A prefix that still carries beads
cannot be released — the flow refuses (refusal 4 in §Question e), and there
is no `--with-beads` override. The captain's reasoning, adopted here verbatim
in substance: it matches the captain's own "refuse rather than guess"
posture; releasing a prefix that still owns beads would let a second store
claim it and make ownership of the existing beads ambiguous; and a prefix
frees only when it carries no beads — migrating beads off it first is the
explicit, auditable path. If a genuine operational need for an override
appears later, adding one is a small reversible addition, whereas shipping
the override first and withdrawing it is not. The audited `--with-beads`
override this section originally recommended remains documented as the
**rejected alternative** — see refusal 5 in §Question e — rejected as
overbuilding this round (the one legitimate decomposition case is better
served by the sequenced off-migration than by a single hide-the-beads verb).

**Recommendation (not a decision): b1**, without reservation — it is the only
shape consistent with "the prefix record is the thing that is true about the
data". Under absolute refusal this is no longer a preference but the
mechanism forced by the refusal: a released prefix by definition carries no
beads (the operator migrates them away first), so the beads-become-wide-only
consequence described for b1 applies to no release this decision permits,
and the residual choice is exactly b1's, not b2's.

**What the row becomes.** Two sub-options, and the difference matters for the
transfer window in (c):

**b-row-1. Delete the row.** The prefix returns to the identical state it was
in before anyone claimed it — no row, no owner, re-claimable by anyone. The
fallback logic (`no rows → own name`) and every reader already understand "no
row = unclaimed"; no new state vocabulary is introduced.
*Gets:* one definition of unclaimed across build-time and runtime; re-claim
needs no special path (`add` inserts, as it always has).
*Costs:* while unclaimed, the prefix is visible in `store-prefix list` only by
*absence* — a state you have to know to look for. The audit trail (d) and the
refusal messages must carry the pointer.

**b-row-2. Keep the row, re-point it at `__unattributed__` with reason
`released-by-<owner>`.** The unclaimed state becomes a visible row in
`store-prefix list`, and re-claim *updates* the row.
*Gets:* the in-flight state of a transfer is visible on the ordinary
inspection surface.
*Costs:* it blurs two different meanings of unattributed — the build's
"no source ever declared or matched" versus "an owner deliberately gave this
up" — inside one 64-character reason; and re-claiming an `__unattributed__`
row becomes an UPDATE, a third write path beside insert and no-op, each with
its own concurrency semantics.

**Recommendation (not a decision): b-row-1, delete the row.** The unclaimed
window is short (see c) and is covered by the append-only event record, which
is the designed place to look; the record itself staying build-shaped ("no row
means unclaimed, always, everywhere") is worth more than row-level visibility
of a transient state. If the captain prefers visible-in-list state, b-row-2 is
workable but must then also decide how `add` treats a released-then-reclaimed
row differently from a build-time unattributed row — it currently would treat
both identically (claim over `__unattributed__` is a rival-claim refusal! —
`RecordStorePrefix` matches on `store != target`, and `__unattributed__` is
never the target), which is a latent defect of b-row-2 worth naming: under
b-row-2, releasing a prefix makes it **harder** to re-claim than a fresh
prefix, because the claim collides with the released row. b-row-1 has no such
trap. (This asymmetry is reason enough to prefer deletion on its own.)

## Question c — the transfer path

**c1. Two acts: release, then claim.** `store A: store-prefix release <prefix>
--confirm`, then `store B: store-prefix add <prefix>`.
*Gets:* every state is separately visible and separately audited; no new
verb beyond release; the claim path is the one that already exists and is
already proven.
*Costs:* the unclaimed window is real. Any store that claims in between wins —
the first-claimant rule does not know about transfer intent. A contested
transfer (rival interest in the prefix) is therefore unreliable: the transfer
succeeds only if nobody else claimed first. For uncontested moves (the common
case: a store is renamed or decommissioned and its prefix re-homed) the window
is minutes-to-seconds and the operator performs both acts back to back.

**c2. One atomic move.** `bd store-prefix move <prefix> --to <store> --confirm`
— one transaction appends the transfer event, flips the row's `store` (and the
`allowed_prefixes` of both stores), with no unclaimed instant.
*Gets:* no window; transfer intent is enforced.
*Costs:* it is re-homing in one step — the exact shape the captain refused to
invent silently. To keep it honest it would need its own confirmation
ceremony, and it still *rewrites* the ownership row in one transaction, which
is fine only because the event record makes it loud. It also introduces a new
contention question: a rival claim arriving while the move is in flight either
serialises after it (and is refused, having "lost" the race it could have won
during a release window) or waits — both behaviours need to be specified,
tested, and explained to operators.

**What happens to a claim arriving mid-flight, per option.** Under c1 there is
no "mid-flight": once release lands, the prefix is simply unclaimed, and the
next claim wins, whoever it is — including a store nobody intended. The
intended transferee has no reservation; that is the accepted cost of c1, and
the operator's remedy is to do the two acts promptly and to check
`store-prefix list` between them. Under c2, a claim racing the move is
refused with "a transfer is in progress" (same-transaction serialisation on
the row) — never silently accepted and never silently redirected.

**Recommendation (not a decision): c1 for now, with c2 named as a deliberate
follow-on.** The brief's own framing — the transfer must not reintroduce
silent rewriting — argues for keeping each state transition a separate,
individually refused/confirmed act until a real contested-transfer need
appears. If the unclaimed window proves to be a live problem (two agents
fighting over a prefix mid-transfer), c2 is the designed answer then, and the
event table from (d) already carries the `transfer` event type so the audit
vocabulary does not change when it lands.

## Question d — what is recorded

The ownership row carries a reason but has no history: rewriting it today
would destroy the only audit trail. The design adds an **append-only event
table**, mirroring the repo's existing `events` idiom (id, event_type, actor,
old_value, new_value), keyed to prefix instead of issue:

```sql
CREATE TABLE brain_store_prefix_events (
  id varchar(64) NOT NULL,        -- NewEventID()-style, as `events` uses
  event_type varchar(32) NOT NULL, -- 'claim' | 'release' | 'transfer'
  prefix varchar(255) NOT NULL,
  actor varchar(128) NOT NULL,    -- the namespace that performed the act (BD_NAME)
  old_store varchar(128) NOT NULL,-- owner before ('' when none)
  new_store varchar(128) NOT NULL,-- owner after ('' when unclaimed)
  reason varchar(64) NOT NULL,    -- the human why, free text up to the row's limit
  bead_count bigint NOT NULL DEFAULT 0,  -- live bead count observed at event time
  event_at datetime NOT NULL,
  PRIMARY KEY (id),
  KEY idx_brain_prefix_events (prefix)
)
```

Semantics:

- **Every** change to `brain_store_prefixes` ownership — including claims, so
  the record's history is complete from this table's adoption onward — appends
  an event row **inside the same transaction** that changes the ownership row.
  The two land together or not at all; there is never a changed ownership row
  without its event, and never an event without its change.
- The event table is **append-only and never rewritten**: no UPDATE, no
  DELETE, no compaction. `brain_store_prefixes` remains *current state* —
  which is why rewriting it as part of a *recorded, transactional, deliberate*
  release is not a contradiction of the never-silently rule. The rule the
  captain set is against **silent** rewriting; after this design, no
  ownership change can be silent, because every one has a row that names who
  did it, when, to whom, and why.
- `bead_count` is captured at event time so a later reader can see what the
  release cost without reconstructing it.
- The refusal messages gain a pointer to the trail: a rival claim on a prefix
  released ten minutes ago can be answered with "released by <owner> at <time>
  (event <id>), reason: <why>" instead of just "not yours".
- `bd store-prefix list` grows a `--history` mode (or a `store-prefix history
  <prefix>` subcommand — the implementation pick, left open) that prints the
  event rows for a prefix, newest first.

Alternatives considered:

**d1. No new table — reuse `owner_reason` on the row.** Rewrite
`owner_reason` to `released:...` / `transferred:...`. *Costs:* destroys the
previous reason in place (the audit gap this design exists to close), packs
who/when/why into a 64-character column, and makes history read like
guesswork. Rejected.

**d2. No new table — rely on Dolt's native history.** Every write is already a
Dolt commit; `dolt log`/`dolt diff` on `brain_store_prefixes` gives free
history. *Gets:* zero schema change. *Costs:* history is only reachable
through Dolt-specific tooling outside bd's own SQL surface, is not
self-describing (a commit message would have to carry who/why, and nothing
today writes one), and breaks the moment the store is copied or exported
without history. Rejected as the *primary* record; it remains a free
second copy of the same facts.

**Recommendation (not a decision): the event table**, because it is the only
option that keeps the audit trail inside the record the CLI itself reads, and
because "claims are recorded too" means the day the transfer verb lands the
history needs no backfill.

## Question e — refusal semantics

Every state where the flow must refuse rather than guess. Each refusal names
what is true and what would change it; none is bypassable by `--force` (the
namespace-mint precedent: on the unified database a bypassed check mints a lie
— `unified_namespaces.go` already refuses `--force` bypasses for exactly that
reason).

1. **The prefix is not recorded at all.** Refuse "nothing to release":
   release of a nonexistent claim must be an error, not a success-shaped
   no-op — a no-op would let an operator believe a release happened.
2. **The act is not performed as the owner.** `BD_NAME` must equal the row's
   `store`. Refuse with the owner's name. No `--store` escape exists on the
   releasing side — `add` may claim on another store's behalf (recorded,
   additive, rivalless); release may not (subtractive).
3. **The prefix is build-decided** (`declared-by-store-config`,
   `store-name-matches-prefix`, `first-observing-source`,
   `no-source-declares-or-matches`). Refuse outright: re-homing the build's
   mapping decision is a re-unification question — re-run the build/mapping
   with corrected inputs, or perform it as a deliberate administrative act
   outside this verb. A runtime release flow that could retract the build's
   decisions would make the cutover's provenance record (`declared_by`,
   `observed_by`) mean something different from what the build proved.
4. **The prefix still carries live beads.** Count beads under the prefix from
   the merged `issues` table (not the row's stale `bead_count`) and refuse
   when non-zero. **Absolute refusal — decided 2026-10-07 (question b, above):
   a prefix that still carries beads cannot be released; there is no
   `--with-beads` override.** Ownership of existing beads would become
   ambiguous if a second store could claim the prefix, and a prefix frees
   only when it carries no beads; migrating beads off it first is the
   explicit, auditable path. Release without this check would strand every
   bead in the prefix in wide-only limbo as a surprise rather than as the
   recorded cost of a deliberate migration.
5. **The `--with-beads` override — rejected (decided 2026-10-07).** The
   beads-carrying-release question had a second shape on the table: refuse
   unless the operator repeats the intent (`--with-beads`), record the
   override in the event row (`override_used=1`) with the live bead count,
   and print the bead count and the wide-only consequence in the command's
   output. The original design recommended this audited-override shape, on
   the argument that an unaudited hand-written UPDATE is strictly worse than
   an audited flagged act, and that "refuse rather than guess" is satisfied
   because the flow refuses *until intent is explicit* and never guesses.
   **This recommendation was overruled — the shipped decision is absolute
   refusal (see refusal 4).** The reasoning that carried it: the stricter
   reading of "refuse rather than guess" matches the captain's own posture,
   releasing a beads-carrying prefix would let a second store claim it and
   make ownership of the existing beads ambiguous, and a prefix frees only
   when it carries no beads — migrating beads off it first is the explicit,
   auditable path. The override is rejected for this round as
   **overbuilding**: forcing the one legitimate case (decommissioning a
   store whose prefixes must all go) into a sequenced migration of beads off
   the prefixes first is the honest path for decomposition, not hiding it in
   a single verb. If a genuine operational need for an override appears
   later, adding one is a small reversible addition, whereas shipping the
   override first and withdrawing it is not.
6. **A release that would leave a namespace unmintable.** Checked and stated
   rather than engineered: a store's own-name prefix is *structural* — the
   fallback in `resolveNamespacePrefixes` (no rows → own name) means a store
   can always mint under its own name even with zero recorded rows, so no
   release can make a store unable to mint *anything*. What a release can do
   is make a *prefix* unmintable (unclaimed with beads still in it) — that is
   refusal 4/5's job, not a separate check. The design's claim to the
   captain: the unmintable-namespace state is structurally unreachable, and
   the design would add a defensive assertion in the release path that
   verifies the structural fallback (own-name mint) rather than trusting it.
7. **Two releases racing** (or a release racing a claim): both go through
   `RecordStorePrefix`-style transactions that read the row *inside* the
   transaction; the loser serialises second and finds the row gone or
   changed, and refuses accordingly ("already released at <time>, event
   <id>"). No compare-then-write outside a transaction.
8. **Invalid prefix shape / nonexistent store targets** — the existing shape
   rule (`IsValidAddedPrefix`, no `-` inside a prefix) applies to release and
   transfer identically; a release of a shape-valid prefix the store does not
   own is refusal 2 or 3.

## Question f — interaction with the cutover

**Independent, in mechanism; meaningful only after cutover, in practice.**

- Every runtime namespace path — claim, and this design's release — is gated
  behind the unified probe: on a legacy (per-store) database there is no
  namespace record, and the command refuses instead of pretending. Release
  adds no new gate and removes none.
- Before the cutover, no production store runs on the unified database, so
  there is nothing in production to release. The flow can be built and
  exercised on a scratch build (the same way the claim surface was proven —
  the namespace-model report's transcripts) without touching production.
- The cutover steps 3–6 (pointing stores at the unified database one at a
  time) are unaffected: a release is not required by any cutover step, and no
  cutover step requires the release verb to exist. The one interaction worth
  stating: after step 7 (dropping the per-store databases) release becomes
  the *only* non-raw-SQL way to change ownership, which is the reason to have
  it designed before it is needed — not a reason to sequence it with the
  cutover.
- **Statement for the record: release is independent of the cutover
  ordering.** It may be built before, during, or after; it is exercisable only
  on the unified database; nothing in the cutover runbook depends on it.

## What would be built (if this design is accepted)

1. `brain_store_prefix_events` table added to the unified schema
   (`internal/brainunify/schema.go`, additive — new builds get it; existing
   unified databases get it by the same provisioning path that added the
   namespace tables).
2. `issueops.ReleaseStorePrefix` / `ListStorePrefixEvents` beside
   `RecordStorePrefix` / `ListStorePrefixes` in
   `internal/storage/issueops/unified_namespaces.go`, transactional, with the
   refusal rules above.
3. `bd store-prefix release <prefix> [--confirm]` in `cmd/bd/store_prefix.go`,
   mirroring `add`'s reporting style (outcome, consequences printed, `--json`
   shape).
4. Claim events: `RecordStorePrefix` gains the same-transaction event append,
   so the trail is complete for claims too.
5. `bd store-prefix history <prefix>` (or `list --history`).
6. Tests colocated with the existing namespace tests
   (`unified_namespaces_test.go`, `store_prefix_test.go`): each refusal, the
   override, the race, the event row shape, and the re-claim-after-release
   path.

## What would deliberately NOT be built

- **No atomic `move` verb** in this round (c2) — release-then-claim first.
- **No release of build-decided prefixes**, ever, through this verb — that is
  a re-unification act, not a namespace-maintenance act.
- **No per-bead re-homing.** Beads keep their ids; ownership is per-prefix or
  it does not exist. A "migrate beads A→B" verb is a different, larger design
  and is not sketched here.
- **No release of the store's own-name prefix as a special case** — it is not
  special: if a store's name-prefix row exists (it usually does not — the
  fallback makes it redundant), releasing it is ordinary release of a
  runtime/build-decided row, governed by the same rules. The structural
  own-name fallback means no release can strand a store's minting.
- **No silent path.** No flag combination rewrites ownership without an event
  row in the same transaction. There is no beads-carrying release at all:
  absolute refusal (decided 2026-10-07, question b) forecloses
  `--with-beads`, and nothing else shortcuts the never-silently rule.

## Decided by the captain

1. **The override policy (originally question e5 / b):** decided
   **2026-10-07** — **absolute refusal** (see §Question b, "decided on the
   beads-carrying-release question"). A prefix that still carries beads
   cannot be released; there is no `--with-beads` override. The audited
   override this design originally recommended is now documented as the
   rejected alternative (refusal 5 in §Question e), overruled on the
   captain's own "refuse rather than guess" posture and the ownership-
   ambiguity argument, with the later-need escape hatch (a small reversible
   addition if ever genuinely required) recorded on the record, not carried
   as a live option.

## Left to the captain

1. **The history surface (d):** subcommand vs flag on `list` — presentation,
   not substance.
2. **Whether claim events start being recorded now or only with the first
   release.** Recording claims from adoption onward leaves pre-adoption
   claims (already landed) without event rows; the design accepts that gap
   and documents the boundary, but the captain may prefer to backfill one
   synthetic `claim` event per existing runtime row.
3. **Transfer urgency:** if contested transfers are expected soon (two stores
   legitimately wanting the same prefix with intent on both sides), c2 should
   be prioritised over c1; the design's read of current practice is that they
   are not expected soon.

## Update 2026-10-07: the beads-carrying-release decision

The recommendation on the one genuinely open question in this design —
whether a beads-carrying prefix may be released under an explicit, audited
`--with-beads` override — **changed to absolute refusal** on that date, on
the captain's decision "ABSOLUTE REFUSAL. A prefix that still carries beads
cannot be released; there is no `--with-beads` override." The reasoning
adopted with it: it matches the captain's own "refuse rather than guess"
posture; releasing a prefix that still owns beads would let a second store
claim it and make ownership of the existing beads ambiguous; and a prefix
frees only when it carries no beads — migrating beads off it first is the
explicit, auditable path. The audited-override shape was not silently
dropped: it remains documented above as the rejected alternative (refusal 5
in §Question e) with its original reasoning and the reason it was overruled,
so the record shows the road not taken. If a genuine operational need for an
override appears later, adding one is a small reversible addition, whereas
shipping the override first and withdrawing it is not. The remaining three
items in "Left to the captain" are unchanged and stay open.

---

**Editing history (2026-10-07):** decision recorded. Changes made in this
revision: (a) §Question b — a "Decision on the
beads-carrying-release question" block added, with the captain's rationale;
(b) §Question e — refusal 4 rewritten to absolute refusal, refusal 5
rewritten as the rejected alternative with its original reasoning and the
reason it was overruled; (c) header Status note added; (d) the DDL
`override_used` column removed from the event table; (e) build item 3 and
the test list updated to drop the `--with-beads` surface; (f) "What would
deliberately NOT be built" updated; (g) a new "Decided by the captain"
section, and the former item 1 of "Left to the captain" removed, with the
remaining three items renumbered but otherwise unchanged.
