---
id: 0037
title: the store connection converges — one record, generated wrappers, a doctor that repairs drift
isc: []
status: landed
created: 2026-10-18
updated: 2026-10-18
commits: []
touches:
  - internal/configfile/storehost.go
  - internal/configfile/configfile.go
  - internal/configfile/credentials.go
  - cmd/bd/brain_stores_host.go
  - cmd/bd/brain_stores_rewrite.go
  - cmd/bd/brain_stores_doctor.go
  - cmd/bd/brain_stores.go
  - cmd/bd/brain_stores_census.go
  - cmd/bd/main.go
  - divergence/0037-store-connection-convergence.md
upstream_rebase_notes: |
  Upstream bd has no brain federation registry, no `brain stores` verb, and no
  shared store-host record. The only upstream files edited additively are
  `internal/configfile/configfile.go` (GetDoltServerUser gains one resolution
  layer) and `internal/configfile/credentials.go` (LookupCredentialsPassword
  gains one path-resolution layer); both fall through to upstream behaviour when
  the record is absent. `cmd/bd/brain_stores*.go` are brain-only files (resolve
  `ours`). `cmd/bd/main.go`'s `storesCommandCanRunWithoutStore` gains the
  `brain` parent case so 'brain doctor' runs without a database — upstream has
  no such command, so a rebase that takes upstream's version loses only the
  exemption, not correctness.
---

# Why

Task task-lt59w, from the captain (2026-10-17): *brain owns the store
connection*. The previous fix (divergence/0036, mini1-ops) moved where the
federation lives into one file per machine and taught the wrappers to read it at
run time. That closed the "many copies of the host" defect; it did not yet make
the connection *converge* or *self-report*, and three gaps were still live:

- **A machine could not be converged in one step.** `brain stores
  rewrite-wrappers` only rewrote wrappers that already existed, so a store
  registered without a wrapper stayed unreachable under its own name, and a
  wrapper it did rewrite was overwritten with no copy left behind — there was no
  un-converge.
- **Nothing checked the *connection*, only the stores.** `brain stores doctor`
  probed every store, but a wrapper pinning `BEADS_DOLT_SERVER_HOST=127.0.0.1`
  answers *something* while pointing at a stale local daemon, which is exactly
  robots-r1zd5 (mini0: every store failing `Error 1045 Access denied for user
  root on 127.0.0.1:3307`). A read probe cannot see a pin, and the doctor
  reported health.
- **The record carried no credential reference.** The record said where the
  server was but not which user or password file it belongs with, so passing
  `--user`/`--credentials` through wrappers or metadata was the only way to
  state it — one more fact with many copies. For search there is no divergence
  from upstream, so the record simply states the reference and never a password.

Two further defects were found while building this, both in the 0036 code:

1. The pin-stripping regex matched *any* `export BEADS_DOLT_SERVER_HOST=…` line,
   including the canonical run-time block's own `export
   BEADS_DOLT_SERVER_HOST="$_want_host"`. Rewriting an already-canonical wrapper
   therefore deleted the resolved host it had just read, leaving the child with
   no host and silently falling back to per-store metadata — the drift it was
   supposed to end, reintroduced by the repair itself.
2. Nothing distinguished "no wrapper" from "the chord is dropped" because the
   registry records no such intent; the doctor warned forever instead of
   healing.

# What changed

1. **`cmd/bd/brain_stores_host.go` (new)** — `brain stores host` reads the one
   record and says what it is worth: the resolved host/port and which layer
   supplied each value, whether the server answers (a bounded dial), the MySQL
   user and the password file's PATH (never a password, and only whether it is
   present), and any `BEADS_DOLT_PASSWORD`-shaped line in the record — reported
   by key name only, with exit 1. `brain stores host set` is the record's only
   writer: it preserves the lines it was not asked to change, writes 0600
   through a temp file + rename, and refuses a secret-shaped value.
2. **`internal/configfile/storehost.go`** — the record's keys are named, and the
   credential REFERENCE is exposed: `StoreHostValue(key)`,
   `ResolveCredentialsRef()` (user + password-file path + whether that file is
   there), and `ReadStoreHostValues`/`ParseStoreHostLine` for the writers.
   `LOOKUP` layers in `configfile.go` and `credentials.go` read the record
   beneath the process environment and above the store's own metadata, so bd
   itself — not only the generated wrappers — resolves user and password file
   from the one record.
3. **`cmd/bd/brain_stores_rewrite.go`** — convergence, not just repair:
   - a registered store with no wrapper gets the canonical one (server-mode or
     embedded, from the store's own metadata), so every registered store is
     reachable under the name it is registered under;
   - every wrapper that changes is copied to
     `~/.local/state/brain/wrapper-backups/<run>/` first, so a converged machine
     can be un-converged;
   - `--dry-run` reports everything a real run would do, including reading each
     wrapper through, and writes nothing: no wrapper, no registry, no backup.
   - `wrapperPlanFor` is the single plan both a dry run and a real run use, and
     `stripBakedServerPins` is the single place that decides what a pin is — it
     drops a literal host/port and never the block's own `$_want_host`
     resolution. Defect (1) above is fixed there, with a regression test.
4. **`cmd/bd/brain_stores_census.go` (new)** — the registry-vs-wrapper count
   check. `brain doctor` counts the two lists that must describe the same
   federation and reports each side. A *registered store with no wrapper* is a
   store nothing can reach under the name it is registered under (a warning; the
   convergence verb generates it). A *generated wrapper with no registry entry*
   is a CLI in PATH resolving a store the federation does not know about (a
   failure — nothing repairs it automatically, because removing a file is
   destructive and the wrapper may be the only evidence a store still exists).
   Two shapes are deliberately not orphans: a symlink (a hand-made alias,
   `tasks -> task`) and a backup copy in the bin directory
   (`<store>.pre-<date>`, `<store>.bak`), counted separately with "move it out
   of PATH" advice rather than "delete the store's wrapper". `brain stores
   orphans` exposes the same census on its own.

   On a CLIENT machine (remote.yaml names a host) a missing wrapper is NOT
   generated locally: a client reaches its stores through forwarding wrappers
   written by `brain remote install`, and a locally generated store wrapper
   would point the command at a store this machine does not serve. The census
   names the client's own verb instead, and `collectWrapperTargets` skips
   generation there.

5. **`cmd/bd/brain_stores_doctor.go`** — the doctor now checks the *connection*
   the way it checks the stores: the record's existence and liveliness are one
   line, and each server-mode wrapper is checked against the record (baked pin →
   fail; no run-time block → warn; hardcoded home path → drift) using the same
   `wrapperPlanFor` the rewrite verb applies. It is a DRY RUN by default: it
   names every drifted store and the exact repair. `--fix` runs the same
   convergence path (`convergeStoreWrappers`) and, when the record is missing
   while `BEADS_DOLT_SERVER_HOST/PORT` are set, persists it. The verb is
   registered twice — `brain doctor` (the name in the captain's ask) and `brain
   stores doctor` — and both are exempt from store resolution, since a broken
   store connection is precisely when it must run. Three further fixes came out
   of running it against a real host:
   - the doctor reports the *cause* of a failed store read (`failureLine`), not
     the first line: bd prints advisories (store permissions, credentials-file
     mode) before the error that stopped it, and reporting the advisory sent the
     operator to a warning that stopped nothing while `Error 1045 Access denied
     for user 'brain'` sat unread — robots-r1zd5's exact class;
   - a machine that is a *client* says so: `localInspectionNote` and the
     `client_of` JSON field state that the wrapper counts describe THIS machine,
     since a client forwards store commands to its host by default
     (`BRAIN_REMOTE=off` runs them here);
   - `brain stores host`, `host set`, `rewrite-wrappers` and `orphans` join
     `doctor` as commands that run without a beads database: they are the verbs
     for a machine whose stores cannot be opened.

# Verified against a real host

A scratch HOME (`/tmp/bsc-run.sh`) exercises the whole path with a built binary:
a drifted wrapper (baked loopback pin), a registered store with no wrapper, and a
converged store. Dry runs report every finding — "3 repointed, 1 generated",
"Wrappers: 2/3 registered stores have a wrapper" — with a content hash proving
nothing was written; `doctor --fix` drops the pin, adds the run-time block,
generates the missing wrapper, and backs up what it changed byte-for-byte; a
second run reports zero changes (idempotent).

On the macbook host itself (read-only, `BRAIN_REMOTE=off`): **55/55 registered
stores have a wrapper, 0 true orphans, 7 backup copies in PATH** — and, showing
exactly why the follow-up items exist, the same command *without*
`BRAIN_REMOTE=off` reaches a different binary: the client forwards it to the
brain host, whose installed build predates these verbs (0 of the 3 new
subcommands), while the local build has all 3.

# Why this is closer to the brain spec

`VISION.md` §"One federation, one surface" says the federation converges on one
database on one machine, and §"A silent failure is the defect class" says an
unreachable database must be a loud failure. Before this, the connection was
*described* in one place but *unchecked* in every place that used it, and the
only way to converge a machine was a verb that could only patch what it found.
Now the record is the one fact, the wrappers are generated from it, the doctor
reports any wrapper that does not read it (naming the exact repair), and the
repair is the same code path the reporting describes — so "converged" and
"healthy" cannot drift apart again.
