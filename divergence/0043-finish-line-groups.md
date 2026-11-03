---
id: 0043
title: finish-line groups track a push with non-blocking edges and close themselves
isc: []
status: proposed
created: 2026-11-03
updated: 2026-11-03
commits: []
touches:
  - cmd/bd/group.go
  - cmd/bd/group_proxied_server.go
  - cmd/bd/group_test.go
  - cmd/bd/group_embedded_test.go
  - cmd/bd/close.go
  - cmd/bd/close_proxied_server.go
  - cmd/bd/update.go
  - cmd/bd/update_proxied_server.go
  - cmd/bd/batch.go
  - cmd/bd/todo.go
  - cmd/bd/human.go
  - cmd/bd/duplicate.go
  - cmd/bd/duplicates.go
  - cmd/bd/gate.go
  - cmd/bd/epic.go
  - cmd/bd/mol_squash.go
  - cmd/bd/ado.go
  - docs/CLI_REFERENCE.md
  - website/docs/cli-reference/group.md
  - divergence/0043-finish-line-groups.md
upstream_rebase_notes: |
  `cmd/bd/group.go`, `cmd/bd/group_proxied_server.go` and their tests are
  brain-only — resolve `ours`. The only upstream-owned edits are one added
  call to `autoCloseCompletedGroups` added in each upstream-owned close path
  (`close.go`, `close_proxied_server.go`, `update.go`, `batch.go`, `todo.go`,
  `human.go`, `duplicate.go`, `gate.go`, `epic.go`, `mol_squash.go`, `ado.go`,
  and others); keep them if those close paths move.
---

# Why

A push with a finish line (everything for one launch) needs a to-do list that
ends. A long-lived project bead used as that list mixes two jobs: holding work
and tracking membership. The list drifts and nothing ever closes. Project
beads stay for durable knowledge ([`VISION.md`](../VISION.md)); each finish-line
push gets its own group bead that tracks its members and closes itself.

# What changed

- New `bd group` verbs: `create`, `add`, `remove`, `show`, `sync`.
- A group is an ordinary bead labelled `finish-line-group` that holds
  non-blocking `tracks` edges to its members. `tracks` already existed and does
  not affect ready-work calculation, so members are never blocked.
- Closing a member closes any open group whose tracked members are now all
  closed. This runs on every path that moves a bead to closed (`bd close`,
  `bd update --status closed`, `bd batch`, `bd todo done`, `bd human
  respond`/`dismiss`, duplicate, gate, epic, mol squash, ado), direct and
  proxied. A closed group re-checks any group that tracks it.
- Creating a group, adding members, removing the last open member, or
  `bd group sync` also closes a finished group.
- An empty group never closes, and a member that is not in the store counts as
  not done.
- Only labelled beads self-close, so existing beads that carry `tracks` edges
  keep their current behavior.
- Limit: members must live in the same store as the group. The verbs refuse the
  proxied server for now; closing a member still closes its groups there.

# Brain-spec link

[`VISION.md`](../VISION.md)
