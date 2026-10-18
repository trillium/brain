---
id: 0036
title: store writes stamp the filing agent, not the human's git identity
isc: []
status: proposed
created: 2026-10-18
updated: 2026-10-18
commits: []
touches:
  - cmd/bd/agent_identity.go
  - cmd/bd/agent_identity_test.go
  - cmd/bd/actor_test.go
  - cmd/bd/main.go
  - docs/CONFIG.md
  - docs/CLI_REFERENCE.md
  - docs/messaging.md
  - website/docs/reference/configuration.md
  - divergence/0036-store-writes-stamp-filing-agent.md
upstream_rebase_notes: |
  `cmd/bd/agent_identity.go` and its test are brain-only — resolve `ours`. The
  hook in `cmd/bd/main.go` is one six-line branch inside `getActorWithGit()`,
  between the `BD_ACTOR` check and the `git config user.name` lookup, plus the
  updated priority comment above it and the `--actor` flag usage string.
  Upstream may have rewritten `getActorWithGit()`; re-apply the branch by hand.
  There is no schema change and no new field, so a rebase cannot conflict on
  storage.
---

# Why

Every store write already carries an actor — `created_by` on the bead, the
actor on its audit events — and `getActorWithGit()` resolved that actor from an
explicit override straight down to the human's git identity. An agent process
knows its own name and nothing passed it on, so every bead an agent filed in
`robots`, `ideas`, `brain` or any other store was stamped with the human whose
git identity happened to be configured on that checkout, and "which agent filed
this bead" was unanswerable (robots-0x8).

The defect is in the resolution chain, not in any one store's wrapper: a
wrapper-level fix would leave `bd` invoked directly, and any writer that does
not run through a wrapper at all (the beads bridge, a `brain stores render-all`
subprocess, a script that sets `BEADS_DIR` itself), still stamping the human.
One fix in the binary reaches every store and every writer.

# What changed

- `cmd/bd/agent_identity.go` resolves this process's fleet agent identity from
  the mechanism the fleet already has: `PARLAY_AGENT_NAME` when it is exported,
  else the `name:` field of `~/.parlay/agents/$PARLAY_AGENT_ID/identity.md`,
  else `PARLAY_AGENT_ID` itself (parlay's id charset is enforced, so a stray
  value cannot name a path outside the agents directory).
- `getActorWithGit()` consults that identity after `BD_ACTOR` and before
  `git config user.name`. Every explicit actor — `--actor`, the `actor` config
  key, `BEADS_ACTOR`, `BD_ACTOR` — still wins.
- No new field, no new environment variable, no schema change, no migration:
  the identity feeds the actor channel that already exists, so `created_by` and
  the events actor both name the agent.
- Docs: the resolution chain in `docs/CONFIG.md` and `docs/messaging.md`, the
  `--actor` usage line in `docs/CLI_REFERENCE.md`, and the `actor` row in the
  website configuration reference.

# Tests

`cmd/bd/agent_identity_test.go`: the identity lookup (env name, identity.md
name, id fallback, no identity, a traversal-shaped id refused) and the
frontmatter reader (quoted, unquoted, nested, absent, unterminated name).
`cmd/bd/actor_test.go`: the new position in the chain — the agent identity
loses to `--actor`/`BEADS_ACTOR`/`BD_ACTOR` and wins over a real
`git config user.name` — and the existing chain tests are pinned against a
suite that itself runs inside an agent process.

Verified end to end on a throwaway embedded store: `bd create` with
`PARLAY_AGENT_NAME` set records `created_by` as that name; with only
`PARLAY_AGENT_ID` set it records the `identity.md` name; with `BEADS_ACTOR` set
that actor still wins; with no agent identity it falls back as before.

# Brain-spec link

[VISION.md](../VISION.md), §"Fix the mechanism, not the symptom" — the fix names
the root cause (a resolution chain that never asked the agent who it was) and
removes it in the one place every store and writer passes through, rather than
patching each store's wrapper. §"One federation, one surface" — a store's
command behaves the same for every store, so the stamp cannot depend on which
wrapper was used. §"The record outlives the tool" — a bead that records what it
is, including who filed it.
