---
id: 0037
title: one shared store-host file for the brain federation, read at run time
isc: []
status: landed
created: 2026-10-18
updated: 2026-10-18
commits: []
touches:
  - internal/configfile/storehost.go
  - internal/configfile/storehost_test.go
  - cmd/bd/brain_stores.go
  - cmd/bd/brain_stores_doctor.go
  - cmd/bd/brain_stores_rewrite.go
  - cmd/bd/brain_stores_test.go
  - cmd/bd/brain_stores_create_test.go
  - cmd/bd/brain_stores_doctor_test.go
  - cmd/bd/brain_stores_rewrite_test.go
  - divergence/0037-store-host-single-source.md
  - divergence/README.md
upstream_rebase_notes: |
  Every path here is brain-only (resolve `ours`): `cmd/bd/brain_stores*.go` and
  the new `internal/configfile/storehost.go`. No upstream file is edited, and
  core bd resolution is untouched: `GetDoltServerHost`/`GetDoltServerPort`,
  `doltserver.DefaultConfig`, and `bd`'s pre-run all behave exactly as
  upstream. The wrappers this change generates export BEADS_DOLT_SERVER_HOST
  and BEADS_DOLT_SERVER_PORT, which upstream bd already honours.

  On an upstream rebase nothing needs re-applying to upstream files. If the
  shared-file layer is dropped entirely, the brain wrappers fall back to the
  host/port each store's metadata.json pins (the pre-0037 behaviour).
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
run time by the generated store wrappers, with the process environment
overriding it. Missing file = a clear error naming the file, never a silent
`127.0.0.1`.

The change is deliberately **brain-scoped**: only the brain verbs read the
file, and only the wrappers this federation generates use it. Core bd keeps
its upstream precedence (port file, then config.yaml, then metadata.json), so
no unrelated project can be repointed by a stale brain file. A wrapper pins
nothing: it reads the file at run time, so repointing the federation is one
file edit, and a store carrying a stale `dolt_server_host` cannot quietly drag
reads back to the old host *through its wrapper*.

# What changed

1. **`internal/configfile/storehost.go`** — the reader the brain verbs use.
   `Resolve()` gives the host/port for this process (process env wins per key,
   then the file), reporting per-key provenance so a host from the environment
   and a port from the file is labelled `mixed` rather than claimed as the
   file's. A missing file is `ErrStoreHostMissing`; a file that exists but does
   not define both keys, or defines an unusable port, is an error naming the
   file. `ValidateStoreHostFile()` is the same check without a fallback, for
   verbs that must refuse before they touch anything.

2. **`cmd/bd/brain_stores.go`** — `brain stores create` requires the shared
   file up front (a process env value alone does not satisfy it), resolves the
   server from it, and refuses loudly when the server does not answer. The
   generated shared-server wrapper carries a run-time block that reads the file
   and exports both variables, and stops with a clear error naming the file
   (and the unusable key) when the file is missing or unusable — including a
   non-numeric port.

3. **`cmd/bd/brain_stores_rewrite.go`** — `brain stores rewrite-wrappers` also
   requires the file up front, so it can never repoint the fleet into wrappers
   that refuse. It classifies a wrapper from the wrapper's own `BEADS_DIR` and
   its store's metadata (an unclassified wrapper is reported, never `ok`),
   strips whole-line baked host/port pins, detects pins in shapes it cannot
   safely rewrite (a conditional default, a command list) and fails the wrapper
   while one survives, and installs the run-time block *before the first
   statement that can reach a bd invocation* so every path in the wrapper — not
   just the final `exec` — resolves the shared server. Every rewritten wrapper
   is first copied to `<wrapper>.bak`.

4. **`cmd/bd/brain_stores_doctor.go`** — the doctor reports any store whose
   `metadata.json` pins its own `dolt_server_host`/`dolt_server_port`: the
   wrapper reads the shared file, but a bare bd run in the store directory
   would use the pin and silently diverge from the federation's one server.

## Rollback

The repoint is reversible; do not simply delete the file, because every
repointed wrapper exits 78 while the file is absent.

1. **Restore the file** (preferred): put `BEADS_DOLT_SERVER_HOST=<host>` and
   `BEADS_DOLT_SERVER_PORT=<port>` back in
   `~/.config/brain/store-host.env`. Every wrapper resolves again with no
   wrapper change.
2. **Or export the two variables** in the environment that invokes the tools.
   A process env value of either key wins over the file, so exports alone make
   the wrappers work even with the file gone.
3. **Or restore a wrapper** from the `<wrapper>.bak` copy the repoint pass
   wrote next to it (`cp ~/.local/bin/<store>.bak ~/.local/bin/<store>`); the
   backup is the exact pre-repoint file. A wrapper is never rewritten without
   first writing that copy.

Deleting the file and restarting every store's `dolt sql-server` is not a
rollback: the wrappers would still refuse, and a store's own server is not what
the federation reads.

# Why this is closer to the brain spec

`VISION.md` §"One federation, one surface" says the federation converges on one
database on one machine, and §"A silent failure is the defect class" says a
database that cannot be reached is a loud failure. Both are served by moving
the server's location into one readable, overridable fact per machine that the
federation's own wrappers read: there is nothing left to disagree with,
repointing is one file edit, a missing or unusable host produces a named error
instead of an empty store, and the doctor surfaces any store whose own metadata
would still drag a bare run to the old host.
