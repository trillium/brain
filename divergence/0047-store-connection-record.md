---
id: 0047
title: the store-host record carries a credential reference, one verb reads and writes it, and the doctor counts registry against wrappers
isc: []
status: landed
created: 2026-10-18
updated: 2026-11-10
commits: []
touches:
  - internal/configfile/storehost.go
  - internal/configfile/configfile.go
  - internal/configfile/credentials.go
  - cmd/bd/brain_stores_host.go
  - cmd/bd/brain_stores_census.go
  - cmd/bd/brain_stores_doctor.go
  - cmd/bd/main.go
  - divergence/0047-store-connection-record.md
upstream_rebase_notes: |
  Upstream bd has no brain federation registry, no `brain stores` verb, and no
  shared store-host record. The only upstream files edited additively are
  `internal/configfile/configfile.go` (GetDoltServerUser gains one resolution
  layer) and `internal/configfile/credentials.go` (LookupCredentialsPassword
  gains one path-resolution layer); both fall through to upstream behaviour when
  the record is absent. `cmd/bd/brain_stores*.go` are brain-only files (resolve
  `ours`). `cmd/bd/main.go`'s `storesCommandCanRunWithoutStore` gains the
  `host` verbs and the `brain` parent case so 'brain doctor' runs without a
  database — upstream has no such command, so a rebase that takes upstream's
  version loses only the exemption, not correctness.
---

# Why

Task task-lt59w, from the captain (2026-10-17): *brain owns the store
connection*. Divergence 0037 (store-host single source) moved where the
federation lives into one file per machine and taught the wrappers to read it at
run time. Later work added the pin and credential checks to `brain stores
doctor` and the fleet audit. Three gaps stayed open:

- **The record carried no credential reference.** It said where the server was
  but not which user or password file belongs with it, so passing
  `--user`/`--credentials` through wrappers or metadata was the only way to
  state it — one more fact with many copies.
- **Nothing could read or write the record as a unit.** Editing it by hand is
  how a password ends up in a file that is meant to hold none.
- **Registry and wrappers were never counted against each other.** A read probe
  cannot see a registered store with no wrapper, or a generated wrapper whose
  store the federation no longer knows.

# What changed

1. **`internal/configfile/storehost.go`** — the record's keys are named and the
   credential REFERENCE is exposed: `BEADS_DOLT_SERVER_USER` and
   `BEADS_CREDENTIALS_FILE` join host and port. `StoreHostValue(key)`,
   `ResolveCredentialsRef()` (user + password-file path + whether that file is
   there) and `ReadStoreHostValues`/`ParseStoreHostLine` serve the readers and
   the writer. `BRAIN_STORE_HOST_FILE` overrides bd's record path; generated
   wrappers deliberately source the fixed `$HOME/.config/brain/store-host.env`
   path. The record never holds a password.
2. **`internal/configfile/configfile.go`, `credentials.go`** — bd itself, not
   only the generated wrappers, resolves the user and the password file from the
   record: it is read beneath the process environment and above the store's own
   metadata.
3. **`cmd/bd/brain_stores_host.go` (new)** — `brain stores host` reads the one
   record and says what it is worth: host/port and which layer supplied each
   value, whether the server answers (a bounded dial), the MySQL user and the
   password file's PATH (never a password), and any `BEADS_DOLT_PASSWORD`-shaped
   line in the record — reported by key name only, with exit 1. `brain stores
   host set` is the record's only writer: it preserves lines it was not asked to
   change, writes 0600 through a temp file + rename, and refuses a secret-shaped
   value.
4. **`cmd/bd/brain_stores_census.go` (new)** — the registry-vs-wrapper count.
   `brain stores doctor` (and its alias `brain doctor`) counts the two lists that
   must describe the same federation. A *registered store with no wrapper* is a
   warning (`brain stores sync` regenerates it). A *generated wrapper with no
   registry entry* is a failure — nothing repairs it automatically, because
   removing a file is destructive and the wrapper may be the only evidence a
   store still exists. A symlink (a hand-made alias) and a backup copy in the
   bin directory are deliberately not orphans. `brain stores orphans` exposes the
   same census on its own. On a CLIENT machine a missing wrapper is not
   regenerated locally: the census names `brain remote install` instead.
5. **`cmd/bd/main.go`** — `brain stores host`, `host set`, `rewrite-wrappers`,
   `orphans` and `brain doctor` run without a beads database: they are the verbs
   for a machine whose stores cannot be opened.

# Relationship to main

This work was rebased over main's independent landing of the same area (0037
store-host single source, the pin/credential checks in `stores doctor`, the
shared registry, `rewrite-wrappers` with `.bak-repoint-` backups). Where the two
overlapped, main's form wins: registry paths are returned exactly as recorded
(no healing), `brain stores create` refuses an environment host that differs
from the record, and `rewrite-wrappers` only repoints wrappers that exist. The
earlier branch's `doctor --fix`, wrapper generation inside `rewrite-wrappers`
and the `--dry-run` plan were dropped in favour of those.
