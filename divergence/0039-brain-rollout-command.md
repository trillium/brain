---
id: 0039
title: brain rollout, a step engine instead of a written runbook
isc: []
status: proposed
created: 2026-10-22
updated: 2026-10-22
supersedes: 0038
commits: []
touches:
  - cmd/bd/brain_rollout.go
  - cmd/bd/brain_rollout_steps.go
  - cmd/bd/brain_rollout_machine.go
  - cmd/bd/brain_rollout_test.go
  - cmd/bd/brain_host_cmd.go
  - cmd/bd/brain_host_gate.go
  - cmd/bd/brain_registry_cmd.go
  - cmd/bd/brain_registry_ride.go
  - cmd/bd/main.go
  - internal/storeregistry/rollout.go
  - internal/storeregistry/registry_test.go
  - cmd/bd/brain_runbook.go
  - docs/brain/runbooks.go
  - docs/brain/RUNBOOK_STORES_AND_DEPLOY.md
  - docs/CLI_REFERENCE.md
upstream_rebase_notes: |
  Brain-only files; no upstream conflict surface beyond the small additions to
  main.go (store-init classification), brain_host_cmd.go (clients list) and the
  registry-local-verb / ride-along / host-gate lists.
---

# 0039 — `brain rollout`

## What changed

`brain rollout` is the procedure "create a store on the brain host, put a binary
that knows about it on the other machines, prove every machine has it", as a
command. It is a plan by default and changes nothing; `--apply` runs the steps.
Each step has a precondition, an action and a verification; a run stops at the
first one that fails and says what is wrong and what to do. `brain rollout
rollback` puts the kept previous binary back; `brain rollout status` shows the
record.

The step definitions (`cmd/bd/brain_rollout_steps.go`) are the only place the
procedure is written. `--help` and the plan output are generated from them, so
the text cannot drift from the behavior.

Rules the engine enforces (each a step precondition or verification):

- **Pin.** The wrapper pin (`~/.local/bin/.bd-pin.sh`, `BD_WANT`) is owned by
  satellite-config's `converge-stores.sh`, not by this repository. A binary the
  pin would reject is refused with the exact instruction; brain never edits the
  pin. A binary it replaces is kept as `bd.pre-<sha>` and put back at once if
  the pin, sourced the way a wrapper does, refuses the new one.
- **Client machines** are detected from `remote.yaml`; their version is read
  from their own binary with `BRAIN_REMOTE=off`, never through the host.
- **Converged means the machine said so itself**: its own report names it, shows
  the target epoch from the live registry (not a cache) and the candidate build,
  and the newest store reads by name through its wrapper. Unreachable machines
  are reported unreachable; machines left out are listed as not covered.
- Credentials, server users and grants are never touched; `--store` runs the
  existing `brain stores create`.

## The record

A new table `brain_registry_rollouts` (one row per machine: binary version, pin
state, kept previous binary, epoch, outcome, time verified). Existing
`brain_registry_machines` could not hold it: each machine REPLACEs its own row on
every sync, which would erase operator-written columns, and an older binary's
REPLACE would reset any new column. `brain host clients list` and `brain
rollout status` show it.

## Removed

`brain runbook`, `docs/brain/RUNBOOK_STORES_AND_DEPLOY.md` and its embedding
(divergence 0038, now superseded).

## Why

A runbook document enforced nothing, and the traps of 2026-10-22 (the exact
version pin, a client forwarding to an older host build, machines unreachable by
ssh) are rules the code can check.
