---
id: 0040
title: store writes stamp the filing agent, not the human's git identity
isc: []
status: proposed
created: 2026-10-18
updated: 2026-10-30
commits: []
touches:
  - cmd/bd/agent_identity.go
  - cmd/bd/agent_identity_test.go
  - cmd/bd/agent_identity_store_test.go
  - cmd/bd/create.go
  - cmd/bd/create_form.go
  - cmd/bd/create_input.go
  - cmd/bd/gate.go
  - cmd/bd/memory.go
  - cmd/bd/state.go
  - cmd/bd/todo.go
  - docs/CONFIG.md
  - divergence/0040-store-writes-stamp-filing-agent.md
upstream_rebase_notes: |
  `cmd/bd/agent_identity.go` and its tests are brain-only — resolve `ours`.
  Upstream files only change at their `CreatedBy:` fields, which read
  `getFilingActor()` instead of `getActorWithGit()` (create.go, create_form.go,
  create_input.go's `in.createdBy`, gate.go, memory.go, state.go, todo.go).
  `getActorWithGit()` itself is untouched. Upstream may add new create sites;
  point their `CreatedBy` at `getFilingActor()` by hand. There is no schema
  change and no new field, so a rebase cannot conflict on storage.
---

# Why

A new bead records who filed it in `created_by`, and that value came from
`getActorWithGit()`, which resolves an explicit override straight down to the
human's git identity. An agent process knows its own name and nothing passed it
on, so every bead an agent filed in `robots`, `ideas`, `brain` or any other
store was stamped with the human whose git identity happened to be configured
on that checkout, and "which agent filed this bead" was unanswerable
(robots-0x8).

The defect is in the binary, not in any one store's wrapper: a wrapper-level
fix would leave `bd` invoked directly, and any writer that does not run through
a wrapper at all (the beads bridge, a `brain stores render-all` subprocess, a
script that sets `BEADS_DIR` itself), still stamping the human. One fix in the
binary reaches every store and every writer.

# Scope

Only the filing attribution changes. The global actor — the `--claim`
assignee, the comment author, the current-issue lookup (in-progress issues by
assignee), the `bd todo` assignee, and the actor on every event — still
resolves through `getActorWithGit()` exactly as before. Moving those to the
agent would have changed claim ownership under running agents: an issue claimed
under the human's git name would refuse a re-claim with `ErrAlreadyClaimed`
and drop out of the current-issue lookup.

# What changed

- `cmd/bd/agent_identity.go` resolves this process's fleet agent identity from
  the mechanism the fleet already has: `PARLAY_AGENT_NAME` when it is exported,
  else the `name:` field of `~/.parlay/agents/$PARLAY_AGENT_ID/identity.md`,
  else `PARLAY_AGENT_ID` itself (parlay's id charset is enforced, so a stray
  value cannot name a path outside the agents directory).
- `getFilingActor()` returns an explicit actor (`--actor`, the `actor` config
  key, `BEADS_ACTOR`, `BD_ACTOR`) when one is set, else that agent identity,
  else `getActorWithGit()`'s fallback. Every `CreatedBy` stamp on issue
  creation (`bd create` and its form, gate, memory, state event, todo) reads
  it.
- No new field, no new environment variable, no schema change, no migration.
- Docs: `docs/CONFIG.md` describes the `created_by` rule beside the actor
  resolution chain, which is unchanged.

# Tests

`cmd/bd/agent_identity_test.go`: the identity lookup (env name, identity.md
name, id fallback, no identity, a traversal-shaped id refused), the frontmatter
reader, and `getFilingActor()`'s precedence — the agent identity loses to
`--actor`/`BEADS_ACTOR`/`BD_ACTOR` and wins over a real `git config user.name`,
while `getActorWithGit()` keeps answering the git identity.
`cmd/bd/agent_identity_store_test.go`: on a real store, an agent-run
`bd todo add` stamps the agent in `created_by` and the human in `assignee`, a
claim with the global actor succeeds, and a claim the human identity already
holds stays idempotent.

# Brain-spec link

[VISION.md](../VISION.md), §"Fix the mechanism, not the symptom" — the fix names
the root cause (a filing stamp that never asked the agent who it was) and
removes it in the one place every store and writer passes through, rather than
patching each store's wrapper. §"One federation, one surface" — a store's
command behaves the same for every store, so the stamp cannot depend on which
wrapper was used. §"The record outlives the tool" — a bead that records what it
is, including who filed it.
