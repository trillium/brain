---
id: 0037
title: shared store registry with an epoch, sync, ride-along, fleet doctor and a poller
isc: []
status: proposed
created: 2026-10-21
updated: 2026-10-21
commits: []
touches:
  - internal/storeregistry/registry.go
  - internal/storeregistry/sql.go
  - internal/storeregistry/helpers.go
  - internal/storeregistry/registry_test.go
  - internal/storeregistry/regtest/regtest.go
  - cmd/bd/brain_registry_shims.go
  - cmd/bd/brain_registry_sync.go
  - cmd/bd/brain_registry_ride.go
  - cmd/bd/brain_registry_cmd.go
  - cmd/bd/brain_registry_fleet.go
  - cmd/bd/brain_registry_reconcile.go
  - cmd/bd/brain_registry_poller.go
  - cmd/bd/brain_registry_e2e_test.go
  - cmd/bd/brain_registry_more_test.go
  - cmd/bd/brain_stores.go
  - cmd/bd/brain_stores_doctor.go
  - cmd/bd/brain_remote.go
  - cmd/bd/main.go
  - docs/brain/STORE_REGISTRY.md
  - docs/brain/STORE_REGISTRY_DEPLOY.md
  - docs/brain/store-registry-bridge.patch
  - docs/brain/README.md
  - docs/CLI_REFERENCE.md
  - divergence/0037-store-registry-epoch-sync.md
upstream_rebase_notes: |
  All `brain_registry_*.go` files and `internal/storeregistry/` are brain-only —
  resolve `ours`. Hooks in shared files: `main.go` gets `registryCheckEarly`
  before `maybeForwardRemote`, `registryRideAlong(rootCtx, cmd)` just before
  `syncCommandContext()` in the root `PersistentPreRunE`, and
  `storesCommandCanRunWithoutStore` is widened (sync/verify on a client,
  doctor --fleet on a client, poller/shell-hook always). `brain_remote.go`:
  `remoteForwardingActive` runs `registryLocalVerb` args locally. `brain_stores.go`:
  `create/remove/rename/alias/add/set-about/edit-back` call the registry first and
  fall back to their old local behaviour; `seedNamespaceRows` is split into
  `seedNamespaceTx`. Re-apply by hand if upstream rewrites those verbs.
---

# Why

A store created on one machine was usable only there. Nothing told the other
machines: the three registries matched only because a file was copied by hand,
`remove` and `rename` were half-broken, the beads bridge read `stores.yaml` once
at start, and the agent link folder had already drifted on all three machines.
The data and the namespace record are in the one shared database; what a machine
lacks is only the local shim that points at it. The captain wanted every machine
to learn of a create, rename or remove with no human step, a recovery process
for an out-of-sync fleet, and proof of convergence.

# What

- `internal/storeregistry`: four tables (`brain_registry_state` with the epoch,
  `_entries` with tombstones, an append-only `_log`, and `_machines`, what each
  machine reports it applied), written by `Mutate`, which commits the entries,
  the log rows, any caller hook (the namespace seed) and the epoch bump in one
  transaction with a compare-and-swap on the epoch.
- `create`, `rename`, `remove`, `alias`, `add`, `set-about`, `edit-back` write the
  registry when it is adopted; `rename` and `remove` now do the full job (wrapper,
  `stores.yaml`, `stores.env`, link, database alias) and `remove` leaves a tombstone
  that each machine applies to its own generated files only.
- `brain stores sync [--dry-run] [--machine]`: the one idempotent reconcile.
- Ride-along: every command reads the epoch (one row, ~0.1 ms) and syncs inline
  when behind; never blocks; locked; loop-protected. Launchd poller as backstop.
- `brain stores doctor --fleet`, `verify`, `reconcile`, `epoch`, `registry`,
  `poller`, `shell-hook`, and the runtime-resolution verb `brain store <name> …`.

# Decisions

- Registry in the shared database with an epoch (option A of the scout report);
  an outbox event is at most an accelerator, never the source of truth.
- Runtime resolution and generated shims are both kept, one registry feeding
  both: existing callers run stores by command name and need the file.
- Remove never deletes data, database rows or non-generated files.
- `create` does not wait for the fleet; it reports `epoch N; k of n converged`.
- The 8 database-only names are asked about (`reconcile`), never guessed.
