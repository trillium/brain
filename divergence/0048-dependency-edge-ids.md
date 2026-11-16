---
id: 0048
title: give dependency edges their own id in list --include-deps and dep remove
isc: []
status: proposed
created: 2026-11-16
updated: 2026-11-16
commits: []
touches:
  - cmd/bd/list.go
  - cmd/bd/list_test.go
  - cmd/bd/dep.go
  - cmd/bd/dep_proxied_server.go
  - cmd/bd/cli_fast_test.go
  - internal/storage/storage.go
  - internal/storage/dependency_queries.go
  - internal/storage/issueops/dependency_queries.go
  - internal/storage/dolt/dependencies.go
  - internal/storage/dolt/transaction.go
  - internal/storage/dolt/dependency_by_id_test.go
  - internal/storage/embeddeddolt/list_queries.go
  - internal/storage/embeddeddolt/transaction.go
  - internal/storage/embeddeddolt/dependency_by_id_test.go
  - internal/storage/domain/dependency.go
  - internal/storage/domain/db/dependency.go
  - internal/storage/domain/db/dependency_extras_test.go
  - docs/CLI_REFERENCE.md
  - docs/cli-reference/dep.md
  - docs/cli-reference/list.md
  - divergence/0048-dependency-edge-ids.md
upstream_rebase_notes: |
  `bd list --include-deps` is brain-only (see the brain-list-include-deps
  evidence under .pr-evidence; it has no divergence note of its own), so
  `cmd/bd/list.go` resolves `ours` for the listDepEdge block. `dep remove`
  is an upstream command: brain adds a one-argument form in front of the
  existing two-argument body in `cmd/bd/dep.go` and
  `cmd/bd/dep_proxied_server.go`; on conflict keep upstream's body and
  re-apply the `len(args) == 1` prelude and the `depRemoveJSON` output.
  The storage additions are one new read, `GetDependencyRecordByID`, on
  `DependencyQueryStore`, `Transaction`, and the domain repository and use
  case; each is additive and delegates to
  `issueops.GetDependencyRecordByIDInTx`.
---

# Why

A dependency edge has had its own primary id since migration 0050 made it
deterministic (`depid.New(issue_id, target)`), but nothing on the CLI showed
it or accepted it. `bd list --json --include-deps` printed each edge as
`{id, type}` where `id` is the target issue, so a reader could only name an
edge as the pair of issues it joins. Links carry their own id and are
addressed by it; edges should be too.

# What changed

- **`bd list --json --include-deps`** adds `edge_id` to every edge:
  `{id, edge_id, type}`. `id` stays the target issue, so existing readers are
  unchanged. The value is the row's stored primary id; the batched
  source-keyed read does not select that column, so the command derives it
  from the edge's natural key with the same `depid.New` every insert site and
  the 0050 backfill use.
- **`bd dep remove <edge-id>`** is a new one-argument form beside
  `bd dep remove <issue-id> <depends-on-id>`. The edge is looked up by id in
  the store the command runs against (both dependency tables, durable first),
  and its stored endpoints go through the existing removal path with no
  partial-id resolution. An id that names no edge is an error
  (`no dependency edge with id X`); the two-argument form keeps its silent
  success on a missing pair.
- **`bd dep remove --json`** reports `edge_id` next to `issue_id` and
  `depends_on_id` in both forms.
- **Storage:** `GetDependencyRecordByID(ctx, id)` on `DependencyQueryStore`
  and `Transaction` (Dolt and embedded stores and their transactions),
  `DependencySQLRepository.GetDependencyRecordByID` and
  `DependencyUseCase.GetByID` for the proxied-server route, all over one
  shared `issueops.GetDependencyRecordByIDInTx`.

# Not changed

`bd dep list --json` and `bd show --json` still print edges without an edge
id; the export JSONL is untouched. An edge id is not prefix-routed across
stores: the lookup runs against the store the command is invoked on.
