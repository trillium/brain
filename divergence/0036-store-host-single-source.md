---
id: 0036
title: one shared store-host config — the Dolt server is one fact per machine, read at run time
isc: []
status: landed
created: 2026-10-18
updated: 2026-10-18
commits: []
touches:
  - internal/configfile/storehost.go
  - internal/configfile/storehost_test.go
  - internal/configfile/configfile.go
  - cmd/bd/brain_stores.go
  - cmd/bd/brain_stores_doctor.go
  - cmd/bd/brain_stores_rewrite.go
  - cmd/bd/brain_stores_test.go
  - cmd/bd/brain_stores_create_test.go
  - cmd/bd/brain_stores_doctor_test.go
  - divergence/0036-store-host-single-source.md
upstream_rebase_notes: |
  Upstream bd has no brain federation registry, no `brain stores` verb, and no
  shared store-host config. Two upstream files are edited, both additively:

  - `internal/configfile/configfile.go`: `GetDoltServerHost` and
    `GetDoltServerPort` gain one extra resolution layer (the shared store-host
    file) between the process environment and the per-store metadata pins.
    Upstream's chain is otherwise untouched; upstream has no such file, so
    `StoreHostFromFile()` returns ok=false and the functions behave exactly as
    upstream does.
  - `cmd/bd/brain_stores*.go` are brain-only files (resolve `ours`).

  On an upstream rebase, take upstream's `configfile.go` and re-apply the two
  inserted `if h, _, ok := StoreHostFromFile(); ok { … }` /
  `if _, p, ok := StoreHostFromFile(); ok { … }` blocks, or drop them: without
  the file layer every store falls back to its own baked `dolt_server_host`,
  which is the pre-0036 behaviour.
---

# Why

The captain's ask (2026-10-17, task-s0icm) was to fix the *cause* of
improperly configured brain stores, not each store. Three of the reported
defects were the same cause seen from three angles:

- `robots-1nri` / `robots-1o77` / `robots-1ypiy` — "store registered but does
  not answer a read": every store probed through its wrapper, 48 of 54
  timing out, then 48 of 54 reporting "no beads database found (registry path
  missing: /Users/trilliumsmith/…)".
- `task-d3cz` — `brain stores create chores` left a store that no command
  could read (missing `config.yaml`; a Dolt auto-start that always timed out).
- `task-8m5x` — the fleet's Dolt host config was stale on two machines.

The cause is that *where the federation's Dolt server lives* was recorded in
many places at once: baked into each generated store wrapper, baked into each
store's `metadata.json`, hardcoded in a routing shim's default, and exported
by a shell rc. Every one of them can go stale on its own, and each stale copy
is invisible until a store silently answers from the wrong place or times out.

The captain's scope update (2026-10-18, firstmate inbox 001) named the fix:
one shared file per machine, `~/.config/brain/store-host.env`, holding
`BEADS_DOLT_SERVER_HOST` and `BEADS_DOLT_SERVER_PORT` as `KEY=VALUE`, read at
run time by bd and by every generated store wrapper, with the process
environment overriding it. Missing file = a clear error naming the file, never
a silent `127.0.0.1`.

# What changed

1. **`internal/configfile/storehost.go`** — the reader. `Resolve()` gives the
   host/port for this process (process env wins per key, then the file), names
   the file when it is missing, and refuses a half-written or unparseable file
   rather than falling through to a default. `StoreHostFromFile()` is the
   best-effort form used by host resolution.

2. **`internal/configfile/configfile.go`** — `GetDoltServerHost` and
   `GetDoltServerPort` consult the shared file above the per-store pins, so bd
   itself reads it at run time and a store carrying a stale
   `dolt_server_host` cannot drag reads back to the old host. Removing the file
   restores per-store resolution exactly.

3. **`cmd/bd/brain_stores.go`** — `brain stores create` resolves the server
   from the environment, then the shared file, and refuses loudly when neither
   yields a server that answers; the embedded (self-starting) shape is only
   reachable with `--embedded`, and `config.yaml` is written in both shapes
   (task-d3cz's missing file). Generated shared-server wrappers no longer bake
   a host: they read the shared config at run time and exit 78 naming it when
   it is missing.

4. **`cmd/bd/brain_stores_rewrite.go`** — `brain stores rewrite-wrappers` is
   now the one step that repoints a machine's stores: it adds the run-time
   host block to server-mode wrappers, drops baked host pins, heals registry
   paths recorded under another home and persists them.

5. **`cmd/bd/brain_stores_doctor.go`** — the doctor heals registry paths before
   probing (a registry written on another machine no longer reads as "every
   store is broken"), reports which entries it healed, and checks the wrapper's
   own `BEADS_DIR` against the registry so a wrapper that resolves a *different*
   store fails loudly instead of answering from the wrong one (the
   `robots-bfenw` / `robots-3pfky` misroute class, which a read probe cannot
   see).

# Why this is closer to the brain spec

`VISION.md` §"One federation, one surface" says the federation converges on one
database on one machine, and §"A silent failure is the defect class" says a
database that cannot be reached is a loud failure. Both are served by moving
the server's location into one readable, overridable fact per machine: there is
nothing left to disagree with, repointing is one file edit, and a missing or
stale host produces a named error instead of an empty store.
