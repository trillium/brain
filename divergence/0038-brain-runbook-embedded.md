---
id: 0038
title: operator runbooks embedded in the binary (`brain runbook`)
isc: []
status: superseded
created: 2026-10-22
updated: 2026-10-22
superseded_by: 0039
commits: []
touches:
  - docs/brain/RUNBOOK_STORES_AND_DEPLOY.md
  - docs/brain/runbooks.go
  - docs/brain/README.md
  - cmd/bd/brain_runbook.go
  - cmd/bd/brain_runbook_test.go
  - cmd/bd/main.go
  - cmd/bd/brain_host_gate.go
  - cmd/bd/brain_registry_cmd.go
  - cmd/bd/brain_registry_ride.go
  - docs/CLI_REFERENCE.md
---

# 0038 — operator runbooks embedded in the binary

## What changed

`brain runbook` lists the runbooks compiled into the binary; `brain runbook
<topic>` prints one. The runbooks are `docs/brain/RUNBOOK_*.md`, embedded with
`go:embed` from `docs/brain/runbooks.go`. The first one, `stores-and-deploy`,
covers creating a store, rolling a binary out and proving every machine has it,
including rollback and the traps met on 2026-10-22.

The command needs no database, is exempt from the host gate and the registry
ride-along, and always runs locally on a client machine (a host with an older
binary may lack the topic).

## Why

The procedure lived in one operator's head and in a chat. A machine that has the
binary but not a checkout must still be able to read how it is operated.

## Superseded

Superseded by [0039](0039-brain-rollout-command.md). A written procedure, even
one embedded in the binary, can drift from what the code does and enforces
nothing. `brain runbook`, `docs/brain/RUNBOOK_STORES_AND_DEPLOY.md` and the
embedding were removed; the procedure is now the `brain rollout` command.
