---
id: 0035
title: brain host promote — move the brain host role between machines, and clients that correct themselves
isc: []
status: proposed
created: 2026-10-12
updated: 2026-10-12
commits: []
touches:
  - cmd/bd/brain_host_state.go
  - cmd/bd/brain_host_dolt.go
  - cmd/bd/brain_host_service.go
  - cmd/bd/brain_host_layout.go
  - cmd/bd/brain_host_plan.go
  - cmd/bd/brain_host_peer.go
  - cmd/bd/brain_host_promote.go
  - cmd/bd/brain_host_cmd.go
  - cmd/bd/brain_host_gate.go
  - cmd/bd/brain_host_follow.go
  - cmd/bd/brain_host_unix.go
  - cmd/bd/brain_host_windows.go
  - cmd/bd/brain_host_test.go
  - cmd/bd/brain_host_follow_test.go
  - cmd/bd/brain_host_e2e_test.go
  - cmd/bd/brain_remote.go
  - cmd/bd/main.go
  - docs/brain/HOST_PROMOTE.md
  - docs/brain/REMOTE.md
  - docs/brain/README.md
  - docs/CLI_REFERENCE.md
  - website/docs/cli-reference/host.md
  - divergence/0035-brain-host-promote.md
upstream_rebase_notes: |
  All `brain_host_*.go` files are brain-only — resolve `ours`. Three hooks in
  `cmd/bd/main.go`: `hostGateEarly(os.Args[1:])` before `maybeForwardRemote` at
  the top of `main()`, `hostRoleGate(cmd)` first in the root `PersistentPreRunE`,
  and an `isBrainHostVerb(cmd)` store-skip next to `isBrainRemoteVerb`. In
  `brain_remote.go`: the forwarded script exports `BRAIN_REMOTE_FORWARDED=1`,
  `args[0] == "host"` runs locally like `remote`, and `forwardRemote` now
  verifies and follows a moved notice (the old loop-free single forward is
  `forwardOnce`). Re-apply by hand if upstream rewrites `main()` or the pre-run
  classification.
---

# Why

The brain host is one machine. When it has to change (a laptop retiring, a
server taking over), moving it by hand means a database copy, a dozen config
trees, wrappers, links, jobs and every client's config — with no way to know the
copy is exact or to put it back. The captain wanted the role movable with one
correct command, and clients that are behind should fix themselves.

# What

- `brain host promote <ssh-target>` with `--dry-run` (plan and refusals),
  `--stop-after <phase>`, resume by re-running, idempotent; `status`, `verify`,
  `rollback`, `clients`, `service`; a hidden `peer` op the target runs over ssh.
- The copy is Dolt's own: `DOLT_BACKUP('sync-url')` → rsync over ssh → `dolt backup
  restore`, proved by `dolt_hashof_db()` equal on both ends; a seed pass while
  live, then an incremental copy under a freeze.
- The freeze is a gate in the brain binary (`host.yaml` role `frozen`), because
  Dolt 2.2.3 does not enforce `read_only` for root; a hash **fence** just before
  the switch proves nothing wrote behind it.
- Every change on either machine is journaled first (previous bytes, renames,
  service actions), which is what makes resume and rollback exact. The old host's
  data is never modified or deleted.
- It refuses when the target already holds a different live brain (a host from
  another promotion, a stranger on the port, a database or store directory it did
  not place, wrappers it did not generate).
- Self-correcting clients: the old host keeps a durable forwarding record
  (`forward.yaml`) and a `lineage` of promotions in `host.yaml`; a client whose
  host is stale gets a moved notice, verifies that the named machine is a brain
  host whose lineage contains the promotion from that sender, walks chains,
  refuses loops (and chains over five hops), rewrites `remote.yaml`, retries
  once and prints one line. Unverifiable → nothing changes, exit 76.
- Rolling back a finished move is itself recorded as a move back, so clients that
  followed the move follow it back.

# Decisions

- Application-level freeze plus a hash fence rather than a server-level freeze
  that Dolt does not offer; the fence turns "something wrote behind the gate"
  from silent loss into a refused switch.
- The source orchestrates and the target runs `brain host peer <op>` (JSON over
  ssh) so each step has one implementation and is testable; only ssh crosses the
  network.
- The old host's server is left running by default (it may hold unrelated
  databases); `--stop-source-server` is explicit.
- Trust for self-correction is the host the client was configured with, over ssh,
  plus the lineage check; ssh host-key checking stays on (opt-in
  `BRAIN_FOLLOW_ACCEPT_NEW=1`).

# Tests

`cmd/bd/brain_host_test.go`, `brain_host_follow_test.go`, `brain_host_e2e_test.go`
(see `docs/brain/HOST_PROMOTE.md` § Evidence), plus a recorded run on scratch
hosts holding a copy of the production unified database.
