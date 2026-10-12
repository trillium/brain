---
id: 0034
title: client mode — `brain remote` forwards brain and every store command to a brain host over ssh
isc: []
status: proposed
created: 2026-10-12
updated: 2026-10-12
commits: []
touches:
  - cmd/bd/brain_remote.go
  - cmd/bd/brain_remote_test.go
  - cmd/bd/main.go
  - docs/brain/REMOTE.md
  - docs/brain/README.md
  - docs/CLI_REFERENCE.md
  - website/docs/cli-reference/remote.md
  - divergence/0034-brain-remote-client-mode.md
upstream_rebase_notes: |
  `brain_remote.go` and its test are brain-only — resolve `ours`. Two small
  hooks in `cmd/bd/main.go`: a `maybeForwardRemote(os.Args[1:])` call at the top
  of `main()`, and an `isBrainRemoteVerb(cmd)` store-skip next to
  `storesCommandCanRunWithoutStore`. Re-apply both by hand if upstream rewrites
  `main()` or the pre-run store-init classification. Note upstream's
  `bd dolt remote …` also has a command named `remote`; the store-skip keys on
  an annotation, not the name, so the two do not collide.
---

# Why

The stores live in one Dolt database on one machine. Other machines need to
use them without a local database, a second server, or the Dolt port exposed
on the network (the host's server accepts passwordless root logins on all
interfaces; the design must not depend on that or worsen it).

# What

- `~/.config/brain/remote.yaml` names the brain host (an ssh target).
- When it is set, `main()` forwards the whole command line to the host over
  ssh before any command or database machinery starts. The host runs the same
  command through its own wrapper (`$bin_dir/<store>` or `$bin_dir/brain`) under
  a fixed `/bin/sh -c` script, with each argument passed as a quoted positional
  parameter. stdin, stdout, stderr, TTY (only when stdin and stdout are both
  terminals) and exit code are relayed.
- One multiplexed ssh connection (`ControlMaster=auto`, `ControlPersist=10m`).
- `brain remote set|unset|status|install|disconnect`. `install` reads the host's
  registry (`brain stores list --json`), checks the host has a wrapper for each
  store, and writes a marker-tagged client wrapper per store; it never replaces
  an unmarked file without `--force`, never writes `bd`, and reports PATH
  shadowing.
- Escape hatches: `BRAIN_REMOTE=off`, `BRAIN_REMOTE_HOST`. The host side of every
  forward runs with `BRAIN_REMOTE=off`, so forwarding cannot loop.

# Decisions

- ssh forwarding rather than a network Dolt connection: no new credential,
  port or server change; the host's wrappers (create-emit hooks, pins) apply
  unchanged. Cost: one process on the host per call.
- The host is addressed only through commands it already has (`brain stores list
  --json`, its wrappers), so the host's install needs no change to be a host.
- Client wrappers carry no logic: they set `BRAIN_REMOTE_STORE=<name>` and exec
  the brain binary, so `brain` and every store behave identically.

# Tests

`cmd/bd/brain_remote_test.go`: quoting round-trips through a real shell;
argument/stdin/exit-code preservation through a fake ssh that joins words and
re-parses them like the real one; unreachable-host message and status;
validation of host, bin_dir and ssh options; wrapper install, idempotence,
`--force`, `--prune`, never `bd`; wrapper → binary store handoff; registry parse
and host wrapper check; PATH shadow notes; `brain remote` does not open a store
while `dolt remote add` still does.
