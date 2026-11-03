---
id: 0041
title: new issues record whether a human, an agent or a procedure wrote them
isc: []
status: proposed
created: 2026-11-03
updated: 2026-11-03
commits: []
touches:
  - cmd/bd/author_kind.go
  - cmd/bd/author_kind_test.go
  - cmd/bd/agent_identity_store_test.go
  - cmd/bd/brain_new.go
  - cmd/bd/create.go
  - cmd/bd/create_form.go
  - cmd/bd/create_input.go
  - cmd/bd/create_proxied_server.go
  - cmd/bd/create_proxied_server_test.go
  - cmd/bd/gate.go
  - cmd/bd/graph_apply.go
  - cmd/bd/markdown.go
  - cmd/bd/memory.go
  - cmd/bd/state.go
  - cmd/bd/swarm.go
  - cmd/bd/todo.go
  - docs/CONFIG.md
  - internal/brain/verb/new/new.go
  - internal/brain/verb/new/new_test.go
  - divergence/0041-author-kind-stamp.md
upstream_rebase_notes: |
  `cmd/bd/author_kind.go` and its test are brain-only — resolve `ours`.
  Upstream files change only at the places that already read
  `getFilingActor()` (see 0040): each gains a `Metadata:` field (or wraps the
  existing one) with `withAuthorKind(...)`. `newverb.Args` gains an optional
  `Metadata` field. Upstream may add new create sites; stamp their metadata
  by hand. There is no schema change: the stamp is two keys in the existing
  metadata object.
---

# Why

`created_by` says who filed an issue but not what kind of filer it was, so the
share of issues a person wrote could only be bounded from above: everything
not provably an agent counted as possibly human. Stamping the kind at write
time, when the answer is known, makes that share exact.

The filing-agent stamp from 0040 already settles the agent half: the process
knows whether it runs as a fleet agent. This change reuses that detection and
adds the missing distinction between a person and a script.

# What changed

- `cmd/bd/author_kind.go` adds `withAuthorKind`, which merges `author_kind`
  (`human`, `agent` or `procedural`) and, for agents, `author_agent` into an
  issue's metadata object.
- The kind is `BEADS_AUTHOR_KIND` when it holds one of the three values, else
  `agent` when `agentIdentityActor()` finds a fleet agent, else `human`.
  Scripts and scheduled jobs set the variable to `procedural`; an
  unrecognised value is ignored rather than stamped.
- Every create site that stamps `created_by` from `getFilingActor()` also
  stamps the metadata. A key the caller put in `--metadata` is never
  overwritten, and metadata that is not a JSON object is left alone.
- No new issue type, column or migration. Issues created earlier simply lack
  the keys.
- Docs: `docs/CONFIG.md` describes the keys and the resolution order.

# Tests

`cmd/bd/author_kind_test.go`: the kind resolution table (no signal, fleet
agent, declared procedural/human/agent, case and whitespace, unknown value)
and the metadata merge (fresh object, agent name, procedural carries no
agent, existing keys kept, caller-set kind kept, non-object untouched).
`cmd/bd/agent_identity_store_test.go`: on a real store, issues filed through
`bd todo add` read back as human, agent with a name, and procedural, and an
unstamped issue stays valid. `internal/brain/verb/new/new_test.go`: the
`brain new` verb stores the metadata it is handed.

# Brain-spec link

[VISION.md](../VISION.md), §"The record outlives the tool" — an issue records
what it is, including what kind of author wrote it, at the moment that is
known. §"Fix the mechanism, not the symptom" — the count is made exact at the
write, not estimated afterwards.
