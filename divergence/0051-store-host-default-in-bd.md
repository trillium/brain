---
id: 0051
title: store commands read the machine's store-host file themselves, under a flag and the environment
isc: []
status: proposed
created: 2026-11-24
updated: 2026-11-24
commits: []
touches:
  - cmd/bd/brain_store_host_default.go
  - cmd/bd/brain_store_host_default_test.go
  - cmd/bd/main.go
  - internal/configfile/storehost.go
  - internal/configfile/storehost_test.go
  - docs/brain/REMOTE.md
  - docs/brain/FLEET_DOCTOR.md
  - docs/CLI_REFERENCE.md
  - docs/cli-reference/index.md
  - divergence/0051-store-host-default-in-bd.md
  - divergence/README.md
upstream_rebase_notes: |
  `cmd/bd/main.go` is an upstream file. The brain edit is two additions: the
  `--store-host` persistent flag next to `--global`, and one call to
  `applyStoreHostDefault(cmd)` in the root `PersistentPreRunE`, straight after
  `hostRoleGate`. Re-apply both by hand if upstream reshuffles the pre-run; the
  call must stay ahead of everything that reads `BEADS_DOLT_SERVER_HOST`,
  `BEADS_DOLT_SERVER_PORT` or `BEADS_DOLT_SERVER_USER`.

  Everything else is brain-only (resolve `ours`). No upstream resolver is
  edited: `GetDoltServerHost`, `IsDoltServerMode`, `doltserver.DefaultConfig`
  and the credentials lookup are untouched and still read the environment, which
  is where this change puts the winning value. If upstream grows its own
  user-level server address file, keep this layer above it or fold the two
  together; do not let both write the same variables.
---

# Why

The captain's ask (task-xrw1q): every machine uses the same command-line tool,
and the work executes on one machine. For the brain that machine is the Dolt
host, and the tool is `bd` behind the store commands (`brain`, `task`, `robots`,
`glossary` and the rest).

[0037](0037-store-host-single-source.md) already made the server's address one
fact per machine, `~/.config/brain/store-host.env`. But only a shell read it:
each store wrapper sourced the file before exec'ing `bd`. That left the address
depending on the wrapper's text, and that text differs by machine: mini0's
`task` wrapper sources the file behind a `BRIDGE_BIND_HOST` guard, mini1's on a
single unguarded line, and a wrapper `brain stores create` generates uses a
run-time block. A wrapper without the line falls back to the host its store's
`metadata.json` pins. That is `127.0.0.1` on mini0 and on mini1, and it is right
only on the host.

A machine's configuration can own a file. It cannot own the text of fifty
wrappers. So the binary reads the file.

# What changed

1. **`cmd/bd/brain_store_host_default.go`** settles the server address first in
   the root pre-run, in this order:

   | | source |
   |---|---|
   | 1 | `--store-host host[:port]` (new global flag) |
   | 2 | `BEADS_DOLT_SERVER_HOST` / `BEADS_DOLT_SERVER_PORT` in the environment |
   | 3 | `~/.config/brain/store-host.env` |
   | 4 | what `bd` resolved before: `metadata.json`, `config.yaml`, `127.0.0.1` |

   It applies the winner by exporting it into the process environment. Every
   reader of the address already honors those variables: the storage mode, the
   dial host and port, the credentials lookup, the connect-error diagnosis, a
   child `bd`. They therefore cannot disagree about which layer won, and the
   file behaves exactly as it does when a wrapper sources it. Each key resolves
   on its own, as in the generated wrapper's run-time block: an environment
   host with no environment port takes the file's port.

2. **The file is a default for store commands only.** It is read when the
   wrapper exported `BD_NAME` and the store is server-backed (`dolt_mode:
   server` in its `metadata.json`, or `BEADS_DOLT_SERVER_MODE=1` /
   `BEADS_DOLT_SHARED_SERVER=1` from the wrapper). A bare `bd` in a project
   directory and a store with its own embedded Dolt never read it. This keeps
   0037's guarantee that a brain file cannot repoint an unrelated database: a
   non-local host in the environment forces server mode, so reading the file
   for every `bd` would have turned every embedded project on the machine into
   a client of the brain's server.

3. **No file, no change.** Without the file nothing is exported and the order
   is 1, 2, 4.

4. **`internal/configfile/storehost.go`** gains `ReadStoreHostDefaults`, which
   reads the file the way a shell that sources it sees it: key by key, with
   `BEADS_DOLT_SERVER_USER` as a third optional key and no requirement that
   host and port both be present. The strict reader the brain write verbs use
   (`StoreHostFromFile`, `ValidateStoreHostFile`) is unchanged. A value it
   cannot take literally (a port that is not a number, or shell syntax such as
   `${VAR:-x}`) is dropped and named on stderr with its file and line; the
   other keys still apply.

## What it does not change

- The wrappers on the three machines keep working as they are. They source the
  file, so the environment is already set when `bd` starts and layer 3 has
  nothing left to fill.
- `brain stores create`, `rewrite-wrappers`, `sync` and the doctors read the
  file through the strict reader, as before.
- A `bd` run with `BEADS_DIR` and no wrapper (hooks, launchd jobs) still
  follows `metadata.json`. The doctor's `metadata-host` item still reports it.

## One behavior to know before deploying the binary

A wrapper that deliberately skips sourcing the file now gets the file's address
from the binary instead. mini0's hand-edited wrappers do this: they skip the
file when `BRIDGE_BIND_HOST` is set. On mini0 the file names mini0 itself, so
the server is the same one. To keep such a process on another
server, export `BEADS_DOLT_SERVER_HOST` and `BEADS_DOLT_SERVER_PORT` for it:
the environment wins.

# Why this is closer to the brain spec

`VISION.md` §"One federation, one surface": the federation is one database on
one machine, reached the same way from everywhere. With the address in a file
the binary reads, a machine joins by getting the binary and that one file from
its configuration, and a store wrapper shrinks to the two lines that say which
store it is. The flag and the environment stay as the explicit, visible
overrides for one call.
