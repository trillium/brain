---
id: rollout
title: bd rollout
slug: /cli-reference/rollout
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc rollout`

## bd rollout

The one procedure for: create a store on the brain host, put a brain binary that
knows about it on the other machines, and prove every machine has it. It
replaces a written runbook: it enforces the rules and records what it found.

  brain rollout                      PLAN: every step and check against every
                                     machine; changes nothing (the default)
  brain rollout --apply --binary P   run the steps; stops at the first failure
  brain rollout --store NAME ...     also create the store first (on the host)
  brain rollout rollback [--apply]   put the kept previous binary back
  brain rollout status               what the last runs recorded per machine

A plan evaluates every machine and says where --apply would stop. --apply stops
at the first precondition or verification that fails, says what is wrong and
what to do, and records the machine as refused, failed or unreachable.
A machine is only called converged when the machine ITSELF reports the target
registry epoch and the candidate build and its newest store reads by name; a
machine that cannot be reached is reported unreachable, never converged.

It never edits credentials, server users, grants or a wrapper pin. The pin
(~/.local/bin/.bd-pin.sh, BD_WANT) is owned by satellite-config; a binary the
pin would reject is refused with the exact instruction. A binary it replaces is
kept as bd.pre-&lt;sha&gt; beside it. Machines come from the fleet roster
(~/.config/brain/fleet.yaml, as for 'brain stores doctor --fleet'); a client
machine is checked through its own binary with BRAIN_REMOTE=off.

The steps:

Once per run:
  candidate the binary being rolled out
      needs:  the --binary file (default: the running binary) is an executable that prints its own version with --version
      does:   nothing; read-only
      proves: the version string and sha256 are recorded for every later step
      undo:   none
  registry  the store registry the fleet must converge on
      needs:  the registry is adopted and readable from this machine (directly, or through its relay host on a client)
      does:   nothing; read-only
      proves: the target epoch and digest are read once and every machine is proved against that epoch
      undo:   none
  store     create the store (only with --store NAME)
      needs:  this machine reads the database directly (it is the brain host, not a client) and the name is a valid store name
      does:   run this binary's 'brain stores create NAME' with the configured database login; credentials, server users and grants are never touched
      proves: the registry has an active entry NAME and its epoch moved; that epoch becomes the target for every machine
      undo:   'brain stores remove NAME' tombstones it (data directory, rows and prefix stay by design)
  records   the shared rollout record
      needs:  this machine can read (and with --apply, write) the brain_registry_rollouts table in the shared database
      does:   nothing; read-only
      proves: earlier records are loaded so a rollback knows which binary each machine kept
      undo:   none

For each machine, in roster order:
  reach     reach the machine and read its facts
      needs:  the machine answers ssh non-interactively (BatchMode, 10s) as the deploy user and runs a POSIX sh
      does:   nothing; read-only probe of the pin, the installed binary, the client config and the build's verbs
      proves: the probe answered with its facts
      undo:   none
  mode      direct machine or client
      needs:  none
      does:   nothing; read-only
      proves: a client (~/.config/brain/remote.yaml names a host) forwards commands to its host and so would run the host's build; every version check on a client therefore runs its own binary with BRAIN_REMOTE=off, and the registry is read through the relay as the client itself reads it
      undo:   none
  pin       the wrapper pin accepts the binary
      needs:  ~/.local/bin/.bd-pin.sh is absent, or its BD_WANT equals the candidate's version string exactly; otherwise every pinned wrapper would exit 78 as soon as the binary is swapped. The pin is owned by satellite-config, so a mismatch is refused with the instruction, never edited
      does:   nothing; read-only
      proves: the pin state (ok, absent) is recorded for the machine
      undo:   none
  binary    install the binary, keeping the previous one
      needs:  the pin accepts the candidate; the candidate's platform is the machine's; the file to replace is known (BD_BIN, or the bd.real behind a BD_BIN shim)
      does:   upload the candidate beside the target, run it there and check it reports the candidate's version, copy the current binary to bd.pre-&lt;sha&gt; (an existing, different backup is never overwritten), then rename the new file into place
      proves: the machine's own binary (BRAIN_REMOTE=off) reports the candidate's version and the pin, sourced the way a wrapper does, accepts it; if not, the kept binary is put back at once
      undo:   'brain rollout rollback --apply' puts the kept binary back (keeping the replaced one as bd.post-&lt;sha&gt;)
  converge  make the machine apply the registry
      needs:  the machine's brain has 'stores sync' (the build contains the registry)
      does:   run the machine's own 'brain stores sync' (idempotent; backs up what it replaces; never touches a wrapper brain did not generate)
      proves: the machine's own 'brain stores verify' reports the target epoch and nothing missing
      undo:   sh ~/.local/share/brain/registry-backup/&lt;time&gt;/restore.sh on the machine; BRAIN_REGISTRY=off switches the registry off
  smoke     the wrappers the sync wrote cannot loop, and one real read answers
      needs:  the converge step passed
      does:   nothing; read-only
      proves: every wrapper brain generated in ~/.local/bin on the machine execs a regular executable file that is neither the wrapper itself nor another generated wrapper (read from the files, nothing is run for this); then one real read ('&lt;store&gt; list --limit 1') through the newest store's wrapper answers within a few seconds, under a watchdog that kills it
      undo:   none; on failure the exact restore command of the sync's backup is printed and recorded
  prove     the machine itself proves it converged
      needs:  all earlier steps of the machine passed
      does:   nothing; read-only
      proves: the report came from this machine (its own name, not its host's), the machine's own binary and its brain command run the candidate's exact version, at the target epoch, from the live registry (not a cache), converged, and the newest store reads by name through its wrapper
      undo:   none
  record    write what was established to the shared record
      needs:  the shared database accepts the write (checked at the 'records' step)
      does:   upsert the machine's row in brain_registry_rollouts: binary version, pin state, kept previous binary, epoch, outcome, time verified. A refused or failed machine is recorded too, as refused/failed/unreachable, and keeps its earlier verified time
      proves: the row is read back
      undo:   none (the record is history of what was found)


```
bd rollout [flags]
```

**Flags:**

```
      --apply              Run the steps (default: plan only, nothing is changed)
      --binary string      The bd binary to roll out (default for a plan: the running binary; required with --apply)
      --machines strings   Only these roster machines (leaving one out is reported, never hidden)
      --roster string      Roster file (default ~/.config/brain/fleet.yaml, else mini0, mini1, macbook)
      --store string       Create this store first (run on the brain host) and prove it by name on every machine
```

### bd rollout rollback

Restore the binary a rollout kept as bd.pre-&lt;sha&gt;, keeping the one it replaces as
bd.post-&lt;sha&gt;. The kept binary comes from the rollout record, or --previous PATH
(a file on the machine). Refuses when the machine's pin does not accept the kept
binary: restore the old pin and binary together with the converge backup's
restore.sh first. The store registry is additive and is left as it is.

The steps:

Once per run:
  registry  the store registry the fleet must converge on
      needs:  the registry is adopted and readable from this machine (directly, or through its relay host on a client)
      does:   nothing; read-only
      proves: the target epoch and digest are read once and every machine is proved against that epoch
      undo:   none
  records   the shared rollout record
      needs:  this machine can read (and with --apply, write) the brain_registry_rollouts table in the shared database
      does:   nothing; read-only
      proves: earlier records are loaded so a rollback knows which binary each machine kept
      undo:   none

For each machine, in roster order:
  reach     reach the machine and read its facts
      needs:  the machine answers ssh non-interactively (BatchMode, 10s) as the deploy user and runs a POSIX sh
      does:   nothing; read-only probe of the pin, the installed binary, the client config and the build's verbs
      proves: the probe answered with its facts
      undo:   none
  mode      direct machine or client
      needs:  none
      does:   nothing; read-only
      proves: a client (~/.config/brain/remote.yaml names a host) forwards commands to its host and so would run the host's build; every version check on a client therefore runs its own binary with BRAIN_REMOTE=off, and the registry is read through the relay as the client itself reads it
      undo:   none
  pin       the wrapper pin accepts the kept binary
      needs:  a kept binary is known (the rollout record, or --previous PATH on the machine) and the pin is absent or wants that binary's version; restoring behind a pin that wants the new version would make every wrapper refuse
      does:   nothing; read-only
      proves: the kept binary's version is read on the machine
      undo:   none
  restore   put the kept binary back
      needs:  the pin accepts the kept binary
      does:   copy the kept binary over the installed one; the binary it replaces is kept as bd.post-&lt;sha&gt;
      proves: the machine's own binary reports the kept binary's version and the pin accepts it
      undo:   run 'brain rollout --apply' again with the candidate
  record    write what was established to the shared record
      needs:  the shared database accepts the write (checked at the 'records' step)
      does:   upsert the machine's row in brain_registry_rollouts: binary version, pin state, kept previous binary, epoch, outcome, time verified. A refused or failed machine is recorded too, as refused/failed/unreachable, and keeps its earlier verified time
      proves: the row is read back
      undo:   none (the record is history of what was found)


```
bd rollout rollback [flags]
```

**Flags:**

```
      --previous string   A kept binary on the machine to restore (default: the one in the rollout record)
```

### bd rollout status

Show the recorded binary, pin state and verification time of each machine

```
bd rollout status [flags]
```
