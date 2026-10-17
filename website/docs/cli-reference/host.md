---
id: host
title: bd host
slug: /cli-reference/host
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc host`

## bd host

The brain lives on one machine, the brain host: its Dolt server holds the
database, and every other machine is a client (see 'brain remote'). 'brain host'
moves that role.

  brain host promote &lt;ssh-target&gt;   move the host role from this machine to another
  brain host promote &lt;t&gt; --dry-run  show the plan and every reason it would refuse
  brain host status                 what role this machine has, and any promotion in progress
  brain host verify                 check the host (or, on the old host, that the move holds)
  brain host rollback               put both machines back as they were
  brain host clients …              the machines that use this host, registered and seen
  brain host service …              launchd jobs that belong to the host role

A promotion copies the database with Dolt's own backup (proved by hash), moves
the store directories, registry, wrappers and links, starts the Dolt server on
the new host, turns this machine into a client of it, and points the known
clients there. It can be killed at any point and resumed by running it again;
the old host's data is never modified or deleted. See docs/brain/HOST_PROMOTE.md.

```
bd host [flags]
```

### bd host clients

The machines that use this host: registered, and seen connecting

```
bd host clients [flags]
```

#### bd host clients add

Record a machine that follows this host

```
bd host clients add <ssh-target> [flags]
```

#### bd host clients list

List the machines that use this brain host, in two halves that are shown
side by side:

  registered  the ssh targets in host.yaml ('brain host clients add'). A
              promotion points these at the new host when the role moves.
  seen        clients that ran a command here through 'brain remote'. Each
              records itself on its first call — nothing to configure on it —
              and the record carries its version and when it last connected.

A client can be either or both. A registered client that has never connected
shows "never seen"; one that connects without being registered is listed as
not registered (add it if a promotion should reach it). The heartbeats live in
~/.config/brain/clients.yaml and are refreshed at most once a minute per
client; deleting that file is safe, clients re-register themselves.

```
bd host clients list [flags]
```

#### bd host clients remove

Forget a machine, by the name 'brain host clients list' shows it under.

The name may be the registered ssh target, the client's own name for itself
(user@machine), or — when only one client owns it — the machine name they
share. Both halves of that client are removed:

  registration  it is dropped from host.yaml, so a promotion no longer points
                it at the new host.
  heartbeat     its record in clients.yaml goes too, so the row disappears.
                Heartbeats are the client's own report, not a block: a machine
                that is still in use records itself again the next time it
                runs a command here, and reappears as 'seen'.

An unknown name is an error, not a silent success.

```
bd host clients remove <ssh-target|client> [flags]
```

### bd host promote

Move the brain host role from this machine to &lt;ssh-target&gt; (user@host).

Run it on the current host. The target needs ssh key login from here, this brain
build, dolt and rsync. Start with --dry-run: it prints the plan and every reason
the promotion would refuse, and changes nothing.

Phases (each repeatable; a killed promotion resumes by running this again):
  preflight seed files freeze delta restore serve parity fence flip clients verify

--stop-after &lt;phase&gt; runs up to that phase and stops, so the slow copy can be
done ahead and the short pause and switch later.

```
bd host promote <ssh-target> [flags]
```

**Flags:**

```
      --allow-older-dolt           Allow a target whose dolt is older than this machine's
      --bind string                Address the new Dolt server listens on (default 127.0.0.1)
      --client stringArray         A machine that follows this host and should be pointed at the new one (repeatable)
      --database stringArray       Also move this database (repeatable)
      --dry-run                    Show the plan and every reason it would refuse; change nothing
      --force-wrappers             Replace wrappers and links on the target that brain did not generate (originals are journaled)
      --include-legacy             Also move every other database on the old server (the pre-cutover per-store databases)
      --link-dir stringArray       Directory of agent links to recreate on the target (default ~/.pi/agent/bin)
      --move-aside                 Rename store directories that already exist on the target out of the way (never deletes)
      --no-doltcfg                 Do not copy the Dolt user and privilege files
      --no-seed                    Skip the live pre-copy; do the whole copy while writes are paused
      --no-smoke count             Skip the per-store count checks (the database hash comparison still runs)
      --peer-bin string            The brain command on the target (default: brain, then bd, on its PATH)
      --port int                   Port the new Dolt server listens on (default: same as this host's)
      --remotesapi-port int        Also serve Dolt's remotesapi on this port on the new host (off by default)
      --replace-stale              The target is a former host: rename its old copy aside and replace it
      --service string             How the new server is kept running: auto, launchd, process, external (default "auto")
      --service-job stringArray    launchd job that belongs to the host role and moves with it (repeatable; see 'brain host service')
      --service-label string       launchd label for the new server (default com.brain.dolt-server)
      --skip-wrapper stringArray   Leave this wrapper out (repeatable)
      --source-data-dir string     This host's Dolt data directory, when it cannot be discovered
      --source-port int            This host's Dolt port, when it cannot be told from the stores
      --ssh-option stringArray     Extra ssh -o option for reaching the target, Name=value (repeatable)
      --stop-after string          Run up to and including this phase, then stop (preflight, seed, files, freeze, delta, restore, serve, parity, fence, flip, clients, verify)
      --stop-source-server         Stop this machine's Dolt server after the switch (default: leave it running, refusing brain writes)
  -y, --yes                        Do not ask for confirmation
```

### bd host rollback

Undo a promotion. Everything either machine changed was journaled first, so
this restores it: this machine serves the brain again, the new host stops.

If the new host has been written to since the switch, those writes would be
lost, so rollback refuses; move the host role back with 'brain host promote'
from the new host instead (it carries the writes), or pass --discard-new-writes
to drop them (the database is kept aside on the new host, not deleted).

```
bd host rollback [promotion-id] [flags]
```

**Flags:**

```
      --discard-new-writes   Roll back even though the new host has been written to since the switch
      --force                Undo this machine even if the target cannot be reached (run rollback again later to finish the target)
```

### bd host service

launchd jobs that belong to the host role and move with it

```
bd host service [flags]
```

#### bd host service add

Declare a job that moves with the host role

```
bd host service add <launchd-label> [flags]
```

#### bd host service remove

Stop treating a job as part of the host role

```
bd host service remove <launchd-label> [flags]
```

### bd host status

Show this machine's role and any promotion in progress

```
bd host status [flags]
```

**Flags:**

```
      --check   Also contact the server and hash its databases
```

### bd host verify

Check that the host role is where it should be and working

```
bd host verify [flags]
```
