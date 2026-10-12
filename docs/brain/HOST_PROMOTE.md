# Moving the brain host: `brain host promote`

The brain lives on one machine, the **brain host**: its Dolt server holds the
database and every other machine is a client ([`REMOTE.md`](REMOTE.md)). This
page is about moving that role from one machine to another with one command,
safely, and about what happens to the machines that still point at the old one.

```sh
brain host promote me@new-host --dry-run   # the plan, and every reason it would refuse; changes nothing
brain host promote me@new-host             # move the role (asks first; --yes to skip)
brain host verify                          # check the move holds
brain host rollback                        # put both machines back
```

Run it on the **current** host. The target needs ssh key login from here
(`ssh -o BatchMode=yes me@new-host true`), this brain build, `dolt` and `rsync`.

## What it does

```
 old host (this machine)                                   new host (target)
 ───────────────────────                                   ─────────────────
 1 preflight  ── asks the target what it already holds ──▶ refuses on a different live brain
 2 seed       dolt backup ──── rsync over ssh ───────────▶ dolt backup restore (proves dolt opens it)
 3 files      store dirs, markdown mirrors ──────────────▶ staging area
 4 freeze     writes paused here; drain; hash the database
 5 delta      incremental backup ─── rsync (only changes) ▶ restore, hash == the frozen hash
 6 serve                                                    dolt sql-server started on the copy
 7 parity     per-store rows + checksums + `count` equal on both copies
 8 fence      re-hash here: nothing wrote since the freeze
 9 flip       demote this machine ◀──────────────────────▶ activate the target (wrappers, registry, links, host.yaml)
10 clients    each known client is pointed at the new host
11 verify     end-to-end checks through the new host (reads only)
```

The phases, as `--stop-after` names them: `preflight seed files freeze delta
restore serve parity fence flip clients verify`.

Everything moves with Dolt's own tools: `CALL DOLT_BACKUP('sync-url', …)` on the
old host (safe against a live server, incremental on repeat), `dolt backup
restore` on the new one, and `dolt_hashof_db()` — a hash of the whole database,
working set included — to prove the copy is exact. Only ssh crosses the network;
the Dolt port is never exposed. The seed pass copies the bulk while writes
continue, so the pause is the incremental copy plus the checks, not the whole
transfer: **about 30 seconds for the 384 MiB, 51-namespace database it was
proved on** (the freeze itself took one second; the per-store checks are most of
the rest).

### What moves

| | |
|---|---|
| the databases the stores live in | `brain_unified`; `--database NAME` adds one; `--include-legacy` moves every other database on the old server too (the pre-cutover per-store databases stay behind by default) |
| the Dolt data directory's user files | `.doltcfg` (users, privileges), unless `--no-doltcfg` |
| store directories | each store's `.beads` config (without its old per-store `backup/` and server logs) and its markdown mirror, rsynced to a staging area and renamed into place at the flip |
| `~/.config/brain/stores.yaml`, `stores.env` | rebased to the target's home and written by the same code `brain stores` uses |
| store wrappers and the scripts they source, agent links (`~/.pi/agent/bin`) | text rewritten for the target: paths under the old home → the new home, `BEADS_DOLT_SERVER_PORT` pins → the new port, and `exec bd` → the brain build when the target's `bd` is something else |
| launchd jobs that belong to the role | the ones you declare (`brain host service add LABEL`, or `--service-job LABEL`): plist and the scripts it runs are rebased and installed; a job that was disabled stays disabled. Other candidates are listed, not moved |
| the Dolt server | started on the target as a launchd job (macOS default), a detached process, or "external" (you start it); loopback only unless `--bind` |

Paths are rebased from the old home to the target's home, so the same
`~/data/<store>` layout appears under a different account name. A store outside
the home directory is refused.

### What does not move, on purpose

- The old host's data. It is never modified or deleted — not the database, not
  the store directories, not the old server's files. `rollback` depends on that.
- Databases on the old server that are not the brain's (listed in the plan).
- Dolt's `remotesapi` port and `DOLT_REMOTE_PASSWORD`: the new server does not
  serve remotesapi unless you pass `--remotesapi-port` (and set its password
  yourself). The plan warns when the old server had it.

## How consistency is kept

**The freeze is a gate in the brain binary, not a Dolt setting.** Dolt 2.2.3 does
not enforce `read_only` / `super_read_only` for `root` (tried: both set, writes
still succeeded), and every store connects as root, so a server-level freeze is
not available. Instead `host.yaml` on the old host says `role: frozen`, and
every brain command checks it before touching anything: read-only commands run,
everything else is refused with a message. That covers `bd`, every store
wrapper, forwarded client calls, hooks. It does not cover something that writes
to the database by raw SQL, so the promotion also **proves** nothing did:

- at the freeze it waits until no other statement is running and the database
  hash stops moving, then records that hash;
- the backup is bracketed by two hashes; the copy is only used if they match the
  frozen hash;
- the restored copy on the new host must hash to the frozen hash (checked
  offline, then again on the running server);
- the **fence** re-hashes the old database immediately before the switch. If it
  changed, nothing is switched: the promotion stops, the old host is still the
  host, and `rollback` unfreezes it.

Equal `dolt_hashof_db()` means equal content, so *store-for-store parity* is not
a separate hope: the parity phase additionally compares per-table hashes and,
per store, the row count and an order-independent checksum of its issues,
comments, labels, dependencies and events, and runs the store's own `count`
through its own config against both copies.

## Failure and rollback semantics

State is written after every phase (`~/.config/brain/promote/<id>/state.json`),
and **every change to either machine is journaled before it is made**
(`journal.jsonl` beside it on the old host, `~/.config/brain/promote-incoming/<id>/`
on the new one): the previous bytes of a file, a rename, a service started or
stopped. The journal is what makes both resume and rollback exact.

| If it dies… | State | Do |
|---|---|---|
| in preflight / seed / files | nothing has changed on either machine except staging copies | run the same command again, or `rollback` |
| after the freeze, before the flip | this machine is **frozen** (reads work, writes refused with a message naming the promotion); the new host holds a staged copy; nobody else is the host | run the same command again (every phase is safe to repeat; the freeze is re-measured and the copy redone if the hash moved), or `brain host rollback` to unfreeze |
| during the flip | both ends' steps are individually journaled and idempotent; the order is target first, then this machine's `host.yaml` last | run the same command again |
| after the flip (clients / verify) | the move is done | run the same command again to finish, or `brain host verify` |

`brain host promote` is idempotent: run on a machine that already handed the
role to that target it says so and exits 0; a different target while one is in
progress is refused. `kill -9` at the worst moment (between the restore and the
server start, with this machine frozen) was tried: the next run resumed from the
saved phase and finished.

`brain host rollback` undoes a promotion, finished or not, newest step first,
target first so there is never a moment two machines both serve the brain:

- **before the flip**: unfreezes this machine (host.yaml restored), leaves the
  staged copy on the target (named in the output) for you to remove;
- **after the flip, nothing written to the new host**: the target's server is
  stopped and its files restored (its client config comes back), this machine's
  wrappers, client config and host.yaml are restored, its stopped server is
  started again, and the database is re-hashed to the frozen value;
- **after the flip, the new host has been written to**: refused, because those
  writes would be lost. Promote back from the new host instead (it carries them:
  `brain host promote <old host> --replace-stale`), or `--discard-new-writes`
  (the database is kept aside on the new host, not deleted);
- **the target is unreachable**: refused unless `--force`, which undoes this
  machine only; run it again when the target is back.

Rolling back a finished move is itself recorded as a move back (generation + 1),
so clients that already followed the move find their way back — see below.

### What it refuses

`--dry-run` lists these; a real run stops on them before changing anything.

- the target **already holds a different live brain**: it is a host from another
  promotion, something else listens on the Dolt port, it has a database of the
  same name that this promotion did not place, or a store directory is in the
  way (`--move-aside` renames it; `--replace-stale` is for a *former* host's old
  copy, whose old server must be stopped first). Nothing is ever deleted.
- the target is this machine; this machine is itself a client, or is no longer
  the host, or is frozen by another promotion;
- the target's `dolt` is older than this machine's (`--allow-older-dolt`), or
  `dolt` / `rsync` is missing, or there is not enough free space on either end;
- wrappers or links on the target that brain did not generate (`--force-wrappers`
  replaces them with the originals journaled; `--skip-wrapper NAME` leaves one);
- the staging area and a destination are on different filesystems (the switch
  needs an atomic rename).

## The old host afterwards

At the flip this machine becomes a client of the new one: its store wrappers
become client wrappers, `remote.yaml` names the new host, `host.yaml` says
`role: moved`, and it writes the **forwarding record**
(`~/.config/brain/forward.yaml`). Local commands forward to the new host
transparently; a local escape hatch to the stale copy needs
`BRAIN_HOST_GATE=off` (it refuses by default — a read would quietly show stale
data).

Its Dolt server is left running unless you pass `--stop-source-server`. Stopping
a server that also holds other databases is your call, which is why it is not
the default. Leaving it running is safe for everything that goes through a brain
command (the gate refuses); a raw SQL writer would diverge from the new host,
and only stopping the server prevents that.

## Self-correcting clients

A client whose configured host is stale — it was offline during the promotion,
or never registered with the host — fixes itself on its next brain command.

1. The client forwards the command as usual. The old host (which holds the
   forwarding record, or says it moved) refuses before running anything and
   answers with one line: the new host's ssh target and the facts needed to
   check it (promotion id, generation, the machine that handed the role over).
2. The client connects to the **named** machine and requires it to be a brain
   host (`role: host`, not older than the move) whose **lineage** — the chain of
   promotions that made it the host — contains that promotion, from that
   sender. A third machine, one that was merely named, or one that claims the
   role without having received it, fails this.
3. A chain of moves (A → B → C) is walked hop by hop, each verified against the
   final host's lineage. Loops (a machine visited twice) and chains longer than
   five hops are refused.
4. Only then does it rewrite its own `remote.yaml`, run the command **once**
   more on the new host, and print one line:
   `brain: re-pointed this client from a to b (the brain host moved 2026-10-12; verified); …/remote.yaml updated`.
5. If anything cannot be verified, **nothing is changed and the command does not
   run anywhere**; it exits 76 and says what it could not verify and how to
   point the client by hand (`brain remote set …`).

What the client trusts: the host it was configured with, reached over ssh. The
verification connection is an ordinary ssh connection, so ssh's own host-key
check applies: a new host whose key this client has never seen stops the follow
with an instruction to `ssh <host> true` once (or set `BRAIN_FOLLOW_ACCEPT_NEW=1`
to accept first-seen keys automatically). A command whose input was a pipe that
has already been read is not replayed: the client re-points, says so, and asks
you to run the command again. `BRAIN_REMOTE_HOST` pins a host for one command and
is never rewritten into the file.

The promotion also pushes the change to the clients it knows (`--client`,
`brain host clients add`); the self-correction is what covers the rest.

## Services

The Dolt server on the new host is one of: **launchd** (a LaunchAgent,
`com.brain.dolt-server`, started with `launchctl bootstrap`; survives reboot),
**process** (a detached `dolt sql-server` with a pid file; does not survive a
reboot — the plan says so), or **external** (you start it; brain only checks it
answers). On the old host `--stop-source-server` stops whichever of these it
recognises (a launchd job is also *disabled*, so it does not return at login;
rollback enables and loads it again); a server started by hand is not stopped.

## Moving the real MacBook brain — checklist (not run)

Nothing here was run against the live brain. Before the real move:

1. Install this build on both machines (the promotion protocol version must match;
   the brain version may differ and is only a warning).
2. `dolt` on the target must be at least the old host's version (the MacBook runs
   2.1.10; moving to a newer dolt is fine, back to an older one is refused).
3. `ssh -o BatchMode=yes <target> true` from the old host must work without a prompt.
4. The target's `bd` on `PATH` may be upstream beads (it is on mini0): the moved
   wrappers are pointed at the brain binary directly, and the plan says so.
5. A file on the target that is *not* a brain wrapper but has a store's name
   (mini0's `~/.local/bin/brain` is the binary itself) blocks the move; leave
   that name out with `--skip-wrapper brain`.
6. Declare the jobs that belong to the role (`com.brain.unified-backup`,
   `com.beads-bridge.server`; `com.pai.brain-sync` is disabled and would stay
   disabled): `brain host service add LABEL`. The dry run lists the candidates it
   found and does not move any it was not told about.
7. Decide `--stop-source-server` (the MacBook's server also holds ~70 databases
   that are not the brain's).
8. `--dry-run` first. If you want the slow copy done before the pause, run
   `brain host promote <target> --stop-after files` ahead of time (nothing is
   switched and writes continue), then run the same command again to finish.

## Limits

- macOS launchd and detached-process servers are implemented; systemd is not
  (use `--service process` or `external` on Linux). Windows is not supported.
- Only launchd mode's `launchctl` calls and plist are unit-tested; the end-to-end
  proof used the process mode.
- Stores and the data directory must be under the home directory.
- A raw-SQL writer is not stopped by the freeze; it is detected by the hash fence.
- The pause includes the per-store checks (`--no-smoke` skips the `count` runs;
  the hash comparison always runs).
- Clients that followed a move and then see it rolled back are sent back only if
  the new host had been a client of the old one (its restored `remote.yaml` names
  it). Otherwise `brain remote set <old host>` on those clients.
- Host keys: see "Self-correcting clients".

## Evidence

On scratch hosts on one machine (HOME directories standing in for machines, an
ssh stand-in that runs each command under the other host's HOME, real Dolt
servers on separate ports): host *a* held a copy of the production unified
database (384 MiB, 43 tables, 51 namespaces, 22,704 issues) with 37 store
commands and agent links; *b* was a client of *a*; *c*, *d*, *e* were clients.

- full promote *a → b*: per-store `count` identical before and after (37 stores,
  5,879 issues), database hash identical, `.doltcfg` and mirrors moved;
- a refused promotion for each of: a different brain already on the target;
  something listening on the target's port;
- a client offline during the promote (*c*) and one never registered (*d*, *e*)
  followed the move on their next command; *c* followed a two-move chain
  (*a → b → d*); tampered forwarding records (naming a non-host, a promotion the
  target never received, an unreachable machine, a wrong sender) were each
  refused with nothing changed;
- a write made through a client after the move landed on the new host and not on
  the old one, whose database hashed (offline) to its frozen value;
- a promotion killed with `kill -9` while the old host was frozen: reads worked,
  writes were refused, the next run resumed and finished (and, resumed again
  after an error in a later phase, finished);
- clean rollbacks (including restarting the stopped old server), a rollback
  refused after the new host was written to, and a ping-pong back to a former
  host with `--replace-stale`;
- `cmd/bd/brain_host_test.go`, `brain_host_follow_test.go` and
  `brain_host_e2e_test.go`: rebasing, the journal, the gate, notice parsing and
  filtering, chain/loop/lineage verification, the client follow through a fake
  ssh, the refusal rules, launchd calls, and a two-host promote → resume →
  rollback with real Dolt servers (skipped under `-short`).

See `../../divergence/0035-brain-host-promote.md`.
