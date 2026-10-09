---
id: 0026
title: external-store model — a store's command runs where its database lives (design only)
isc: []
status: landed
created: 2026-10-09
updated: 2026-10-09
commits: [854dc6894]
touches:
  - docs/design/brain-external-stores.md
  - divergence/0026-external-stores-design.md
upstream_rebase_notes: |
  Doc-only entry; no code conflicts expected. This design document is
  brain-only — on a rebase, resolve `ours`. Its facts about the serving
  machine's Dolt server (bind, accounts, remotesapi, shim state) were read
  on 2026-10-09 and will drift; the ground-truth table in
  docs/design/brain-external-stores.md is the record of what was verified
  and when, not a live reference.
---

# Why

The captain's requirement: **a store's command on one machine runs the actual
command on the machine that hosts it** — machine A holds the dolt store,
machine B holds only the config, and `task create` / `task list` come from
machine A. His approved vision fixed the direction that this design obeys on
the review board for H-8 ("One database, on one machine, everywhere" → "Yes -
this is the intent. There should be a way to make a subordinate copy too, but
that can be a future feature"): one host serves, other machines are clients;
a replica is a future feature, not part of this. The vision's silent-failure
clause — a database that cannot be reached is a loud failure, never an empty
result — rules out the one behaviour the standing shim performs on
unreachable remote (`BEADS_ROUTE=local… using local (reconcile later)`): it
is a fallback to whatever is lying on the local disk, and that is exactly the
quiet, softer wrong answer the vision refuses.

The existing shim defers live SQL routing "until rebind + auth exist". This
design verified (read-only, over ssh and by probe from a second machine) that
the rebind already happened on or around 2026-10-05, so the remaining
prerequisite is only auth.

# What changed

- **`docs/design/brain-external-stores.md`** (new) — the design of the
  external-store model, answering the brief's six questions:
  - three transport shapes priced with meaning / gets / costs / host
    requirements: ssh command forwarding (the wrapper runs the real command
    on the host), ssh tunnel (the client speaks MySQL to a forwarded local
    port), direct SQL over the tailnet (server rebound + authenticated);
  - a marked recommendation — **direct SQL over the tailnet, authenticated
    per-consumer** — chosen on failure semantics, client diff size, and
    attribution, with the other two shapes kept in named roles (ssh as the
    bootstrap/admin channel, tunnel as the off-tailnet exception);
  - where the database lives and how a client wrapper learns it: per-store
    `location` (`local` default / `remote` + `host`) in
    `~/.config/brain/stores.yaml`, the registry that already records store
    identity, learned by wrappers through the existing regen path — never
    by probing;
  - the exact loud refusal a command prints when its host is unreachable,
    and an explicit never-list (never an empty result, never a local
    fallback, never `BEADS_ROUTE=local`, never a warning instead of a
    refusal, never exit 0);
  - wide and narrow reads from a client (unchanged semantics — same
    database, different wire), and all-sight during transition: unreachable
    is refusal, deliberately-not-yet-merged is *declared* rather than
    silently omitted;
  - the transition path: reconcile is the pre-cutover data mover, the shim
    stays as the transition mechanism and is retired per store as each
    store's cut-over lands and its `macbook` remote dies with it;
  - what must change before a network client is allowed (per-consumer
    least-privilege SQL accounts, a client credentials story, a stated
    position on plaintext MySQL inside the WireGuard tailnet), and what
    the design refuses to expose (root to the network, SQL off the
    tailnet, a shared account, anything that broadens cross-store
    writes).
- Nothing else changed. **No code, no schema, no wrapper, no shim, no
  registry, no server state on any machine was touched.** Design only,
  landed as a document, including for facts discovered about the serving
  machine (stale shim comment, existing `brainsync@'%'` overgrant) — those
  are recorded in the design's ground-truth table and its open-questions
  list, not acted on.

# Brain-spec link

[VISION.md](../VISION.md), §"One federation, one surface" (the federation
converges on one database on one machine; a subordinate copy is a future
capability) and §"A silent failure is the defect class" (a database that
cannot be reached is a loud failure, never an empty result). Design partner
docs: [docs/design/brain-single-database.md](../docs/design/brain-single-database.md)
(the unified database this transport points at) and
[docs/design/brain-cutover-runbook.md](../docs/design/brain-cutover-runbook.md)
(the per-store repoint act this transition generalizes across machines).
