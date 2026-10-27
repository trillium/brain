---
id: brain
title: bd brain
slug: /cli-reference/brain
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc brain`

## bd brain

brain v0.3 absorbs bd into a single tool that unifies tasks and
knowledge under one substrate (Dolt) with markdown as the exfiltrated
render artifact.

The 'brain' verbs (new, show, list, link, related) speak the knowledge-graph
vocabulary documented in ISA.md. They are thin aliases over bd's storage
layer, gated by the kind discriminator (task | knowledge | both).

For the bd vocabulary (create, dep, list, show, etc.) see 'bd --help'.

See ISA.md §"Decisions" → "First-Tranche Decisions" → "Decision 5
(modularity-first architecture)" for the seam this command tree implements.

```
bd brain [flags]
```

### bd brain link

brain link writes one typed edge (a row in the dependencies table)
between two existing brain docs.

Exactly one edge-type flag must be set:
  --extends         the from-doc extends/revises the to-doc
  --learned-from    the from-doc captures a lesson learned from the to-doc
  --related         the two docs are related (the catch-all edge)
  --type &lt;name&gt;     any well-known bd dependency type (escape hatch)

The flags are mutually exclusive — setting zero or more than one is a
usage error. The wrapper resolves the chosen flag to a single edge-type
string and hands it to the verb; the verb does all validation, existence
probing, and storage I/O.

Examples:
  bd brain link B-a7b3c B-217 --learned-from
  bd brain link B-a7b3c B-552a --extends
  bd brain link B-100 B-101 --related
  bd brain link B-100 B-101 --type=blocks

```
bd brain link <from> <to> [flags]
```

**Flags:**

```
      --extends        Edge type: from-doc extends/revises to-doc
      --learned-from   Edge type: from-doc captures a lesson learned from to-doc
      --related        Edge type: the two docs are related
      --type string    Edge type: any well-known bd dependency type (escape hatch)
```

### bd brain new

brain new creates a brain doc of the given kind.

&lt;kind&gt; must be one of:
  task       — work to be done; participates in ready/blocked queues
  knowledge  — a note or learning; reference-only, never "ready"
  both       — task-shaped work whose body is also the lesson (defaults open)
  isa        — an Ideal State Artifact; allocates IDs of shape &lt;prefix&gt;-isa-XXXXX
               and REQUIRES a slug (auto-generated from &lt;title&gt; when --slug is
               omitted; supply --slug explicitly when the title yields no
               alphanumerics).

The kind value rides on the existing issues.issue_type column. For kind=isa
the verb additionally sets issue.IDPrefix="isa" so the storage layer allocates
"&lt;config-prefix&gt;-isa-XXXXX" IDs (see migrations 0050/0051/0052 for the
substrate).

--slug is optional for non-isa kinds: when non-empty it is validated against
the slug regex (^[a-z0-9][a-z0-9-]&#123;0,63&#125;$) and written to the issues.slug
column; when empty the column stays NULL. Slug values must be globally unique
across all kinds — collisions exit with code 2.

Examples:
  bd brain new task "ship the FTS5 indexer"
  bd brain new knowledge "Dolt FK constraints are lazy until commit"
  bd brain new both "Friday cache bug + postmortem" --body "details..."
  bd brain new isa "Brain as ISA Substrate"
  bd brain new isa "Custom ISA" --slug=my-custom-slug

Valid kinds: task | knowledge | both | isa

```
bd brain new <kind> <title> [flags]
```

**Flags:**

```
      --body string   Optional markdown body (maps to the existing description column)
      --slug string   Optional slug (required for kind=isa; auto-generated from title when omitted)
```

### bd brain promote

brain promote is NOT a brain verb. It is a redirector for the natural-
language verb that often comes to mind when someone wants to shift a
brain doc's kind (knowledge → task, for example).

The brain verb for kind-shift is "recast":

  brain recast &lt;id&gt; --to=&lt;kind&gt;

The name "promote" was avoided because bd already uses it for wisp →
bead graduation (a different, narrower operation). Namespacing matters
more than reading-naturalness for a verb that appears hundreds of
times in shell history.

```
bd brain promote [flags]
```

### bd brain recast

brain recast shifts the kind of an existing brain doc. Every edge
survives. Every comment survives. The body survives. The ID survives.
Only the issue_type column changes — and, on certain transitions, the
status column.

--to=&lt;kind&gt; is REQUIRED. Valid values:
  task        — work to be done; participates in ready/blocked queues
  knowledge   — a note or learning; reference-only, never "ready"
  both        — task-shaped work whose body is also the lesson

Status rules:
  knowledge → task / both : status defaults to 'open' unless the row
                            was explicitly 'closed' (then preserved)
  task → knowledge / both : status preserved
  both → task / knowledge : status preserved

If the current kind already equals --to, recast is a no-op (exit 0,
no write, no markdown churn).

Markdown relocation (entries/knowledge/&lt;slug&gt;.md → entries/task/&lt;slug&gt;.md)
is OUT OF SCOPE for this verb — the exfiltrator handles it on its next
idempotent sync.

Examples:
  brain recast B-a7b3c --to=task
  brain recast B-a7b3c --to=knowledge
  brain recast B-a7b3c --to=both
  brain recast B-a7b3c --to=task --json

```
bd recast <id> [flags]
```

**Flags:**

```
      --to string   Target kind: one of task | knowledge | both (required)
```

### bd brain related

brain related performs a breadth-first walk of outgoing edges from
the given center brain doc and prints the reachable subgraph as an
indented tree.

The walk is bounded by --depth (default 2). --depth=0 prints just the
center; --depth=1 prints the center and direct neighbours; higher
values BFS further out. Each printed node carries its kind tag
([kind=task], [kind=knowledge], or [kind=both]) and — for closed tasks
— a ", closed" annotation. On a cycle, the second appearance of a node
is annotated "(already visited)" and the BFS does not recurse through
it again.

Edges are followed in the outgoing (from → to) direction only — the
same direction "bd dep list" prints by default and the same direction
"brain link &lt;a&gt; &lt;b&gt;" creates. This keeps the rendered tree directional
and prevents an explosion at common hub nodes; a future
"--bidirectional" flag is conceivable but not in scope.

Examples:
  brain related B-a7b3c
  brain related B-a7b3c --depth=3
  brain related B-a7b3c --depth=0      # print the center alone
  brain related B-a7b3c --json         # machine-readable tree

```
bd related <id> [flags]
```

**Flags:**

```
      --depth int   BFS depth cap (0 prints the center alone; higher walks further) (default 2)
```

### bd brain rollout

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
      proves: a client (~/.config/brain/remote.yaml names a host) forwards commands to its host and so would run the host's build; every version check on a client therefore runs its own binary with BRAIN_REMOTE=off. A client has no database: its sync and verify run through its own bd binary (not the forwarding 'brain' wrapper) and read the registry through the relay
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
      does:   run the machine's own 'brain stores sync' (idempotent; backs up what it replaces; never touches a wrapper brain did not generate). On a client this is its own bd binary's sync, which reads the registry through the relay and writes the client's wrappers; the host's sync is never taken for it
      proves: the machine's own 'brain stores verify' reports the target epoch and nothing missing; on a client the report must name the client itself and say it was read through the relay
      undo:   sh ~/.local/share/brain/registry-backup/&lt;time&gt;/restore.sh on the machine; BRAIN_REGISTRY=off switches the registry off
  smoke     the wrappers the sync wrote cannot loop, and one real read answers
      needs:  the converge step passed
      does:   nothing; read-only
      proves: every live wrapper in ~/.local/bin on the machine (a file named for a registered store or alias; backups like &lt;store&gt;.pre-&lt;date&gt; are skipped and listed) execs a regular executable file that is neither the wrapper itself nor another generated wrapper (read from the files, nothing is run for this); then one real read ('&lt;store&gt; list --limit 1') through the newest store's wrapper answers within a few seconds, under a watchdog that kills it
      undo:   none; on failure the exact restore command of the sync's backup is printed and recorded
  prove     the machine itself proves it converged
      needs:  all earlier steps of the machine passed
      does:   nothing; read-only
      proves: the report came from this machine (its own name, not its host's), the machine's own binary and its brain command run the candidate's exact version, at the target epoch, from the live registry (not a cache), converged, and the newest store reads by name through its wrapper. A client has no database, so it proves what it can with its own pieces and never accepts the host's answer: its binary (BRAIN_REMOTE=off) is the candidate's exact version; its own sync/verify name it and found every registered store's wrapper; the newest store's wrapper exists there and answers 'list --limit 1' through the host; the registry epoch read through the relay equals the target. Its record says it is a client and lists this
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

#### bd brain rollout rollback

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
      proves: a client (~/.config/brain/remote.yaml names a host) forwards commands to its host and so would run the host's build; every version check on a client therefore runs its own binary with BRAIN_REMOTE=off. A client has no database: its sync and verify run through its own bd binary (not the forwarding 'brain' wrapper) and read the registry through the relay
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

#### bd brain rollout status

Show the recorded binary, pin state and verification time of each machine

```
bd rollout status [flags]
```

### bd brain store

Resolve &lt;name&gt; from the shared registry at call time and run the command
against that store, exactly as its wrapper would:

  brain store recipes list
  brain store recipes create "Pasta" --type task

The registry is read through the epoch ride-along (and cached under
~/.config/brain/registry.cache.json), so a store created on another machine a
moment ago works here immediately, before 'brain stores sync' has generated its
wrapper. An unknown name triggers one sync and one retry.

  brain store --check &lt;name&gt;   exit 0 when the cached registry has the name
                               (no database access; used by the shell hook)

'brain stores shell-hook zsh|bash' prints a command-not-found handler that
sends an unknown command name here when the registry has it.

```
bd store <name> [command…] [flags]
```

### bd brain stores

Manage the registry of bd stores federated under brain.

The registry lives at ~/.config/brain/stores.yaml. Each registered store
can be searched via 'brain search', transferred to via 'brain transfer',
and synced via 'brain repo sync'.

Run 'brain stores env' to regenerate ~/.config/brain/stores.env for
shell wrapper scripts that need BRAIN_STORE_* variables.

```
bd stores [flags]
```

#### bd brain stores add

Register a store in the brain federation registry

```
bd stores add <name> <beads-dir> [flags]
```

**Flags:**

```
      --local-only   Act on this machine only; do not write the shared store registry (other machines are not told)
```

#### bd brain stores alias

Create a second CLI variant ('alias') that points at an already-
registered store. Useful for singular/plural pairs (idea/ideas,
person/people) or short forms of long names.

Writes a wrapper at ~/.local/bin/&lt;new-name&gt; pinning the existing
store's BEADS_DIR and BD_NAME. Does NOT add a separate registry
entry — both names route to the same data because the wrapper
sets BEADS_DIR identically.

Examples:
  brain stores alias robots robot       # 'robot' command → robots store
  brain stores alias person people      # 'people' command → person store
  brain stores alias ideas idea         # 'idea' command → ideas store

To remove an alias, delete the wrapper:  rm ~/.local/bin/&lt;new-name&gt;

```
bd stores alias <existing-store> <new-name> [flags]
```

**Flags:**

```
      --bd-binary string   Path to bd that the wrapper should exec (default: "bd")
      --no-wrapper         Skip writing the wrapper at ~/.local/bin/<alias>
```

#### bd brain stores create

Provision a new connected store end-to-end. Does, in order:

  1. Provision the Dolt store. In shared-server mode (BEADS_DOLT_SERVER_MODE=1,
     as exported by the brain wrapper) the store is created as a database in the
     running shared dolt sql-server: metadata.json (dolt_mode=server) and
     config.yaml are written first, then bd CREATEs the database and initializes
     the schema. Without the server pins it falls back to an embedded 'dolt init'
     in &lt;path&gt;/.beads/.
  2. Create &lt;path&gt;/entries/ for exfiltrated markdown.
  3. Write a CLI wrapper at ~/.local/bin/&lt;name&gt;. In shared-server mode the
     wrapper exports the four server pins (mirroring the review/brain wrappers);
     otherwise it pins only BEADS_DIR and BD_NAME. Either way it exec's bd.
  4. Register the store in ~/.config/brain/stores.yaml.
  5. Regenerate ~/.config/brain/stores.env.

Default path is $HOME/data/&lt;name&gt;. Override with --path. Skip the wrapper
with --no-wrapper if you manage shell shims another way.

If any step fails after files have been written, the verb leaves the
partial state in place and exits non-zero — re-running with the same
arguments resumes idempotently.

Examples:
  brain stores create recipes
  brain stores create recipes --path /Volumes/extra/recipes
  brain stores create recipes --bd-binary /opt/homebrew/bin/bd

```
bd stores create <name> [flags]
```

**Flags:**

```
      --bd-binary string    Path to bd that the wrapper should exec (default: "bd", resolved by PATH)
      --dolt-email string   --email arg to pass to 'dolt init' (skipped if empty)
      --dolt-name string    --name arg to pass to 'dolt init' (skipped if empty)
      --local-only          Act on this machine only; do not write the shared store registry (other machines are not told)
      --no-wrapper          Skip writing the CLI wrapper at ~/.local/bin/<name>
      --path string         Filesystem root for the new store (default: $HOME/data/<name>)
```

#### bd brain stores doctor

Walk every store registered in ~/.config/brain/stores.yaml and assert it
answers a read. The registry is the source of truth for which stores must
work; anything registered but unreadable is a provisioning bug.

Each store is probed the way an agent would reach it: its CLI wrapper at
~/.local/bin/&lt;name&gt; is invoked with 'list --limit 1'. Stores registered
with --no-wrapper are probed directly with BEADS_DIR pinned, and reported
as a warning so the missing wrapper stays visible.

A store fails when the probe exits non-zero, times out, or prints
'no beads database found' — the signature of a half-provisioned store that
was registered but never initialized. Such a store is silently non-functional
for as long as nobody happens to use it; for queue-shaped stores (staleness,
review, inbox) 'silently empty' is indistinguishable from 'nothing to do',
so the failure is invisible by construction until something asserts it.

Run it from a scheduler (launchd/cron) or a session-start probe so a
half-provisioned store surfaces within a day rather than a week.

With --fleet it instead proves the whole fleet converged on the shared store
registry: it reads the registry epoch and the generated artefacts (wrappers,
agent links, stores.yaml/env) on every machine of the roster over ssh,
read-only, and reports converged / lagging / drifted with the exact fix
command (exit 0 converged, 1 not converged, 2 registry unreadable).

Exit codes:
  0 — every store answered a read
  1 — at least one store failed (or, with --strict, produced a warning)

Every exit 1 is accompanied by a 'FAILING STORES: &lt;names&gt;' line so a
scheduler can attribute the failure. When the registry itself is unreadable
no store names are known, so the line carries the __registry__ sentinel.

```
bd stores doctor [flags]
```

**Flags:**

```
      --fleet                    Prove the whole fleet converged: read the registry epoch and generated artefacts on every roster machine (read-only, over ssh)
      --fleet-timeout duration   Per-machine time limit for --fleet (default 30s)
      --jobs int                 Probe this many stores concurrently (default 8)
      --json                     Emit a structured JSON object instead of per-store text lines
      --machines strings         Only these roster machines
      --roster string            Roster file (default ~/.config/brain/fleet.yaml, else mini0, mini1, macbook)
      --strict                   Exit non-zero on warnings too (missing wrapper, stale registry path)
      --timeout duration         Per-store read timeout (default 20s)
```

#### bd brain stores edit-back

Declare edit-back for a store in ~/.config/brain/stores.yaml.

Edit-back is per store and OFF by default. When a store accepts edit-back,
'bd render-import' may write an edit made to one of the store's rendered
markdown files (entries/&lt;kind&gt;/&lt;slug&gt;.md) back into the bead the file names.
When a store does not declare it, its render stays one-way: 'bd
render-import' running there reports the file-vs-row differences and writes
nothing.

The substrate stays the authority either way: a render-import run applies
only the fields the import defines (title, status, priority, labels,
description), reports every change as old → new, and deletes nothing.
Dealing with the files' own deletions is 'bd render-marks' — a rendered
file that disappears marks its bead for deletion, never removes it.

  bd stores edit-back brain on
  bd stores edit-back task off
  bd stores list --verbose

```
bd stores edit-back <name> on|off [flags]
```

#### bd brain stores env

Write ~/.config/brain/stores.env from the registry (for shell wrappers)

```
bd stores env [flags]
```

#### bd brain stores epoch

Print the registry epoch (database) and the epoch applied on this machine

```
bd stores epoch [flags]
```

#### bd brain stores list

List all registered stores

```
bd stores list [flags]
```

**Flags:**

```
  -v, --verbose   Include each store's about blurb as an extra column
```

#### bd brain stores poller

The poller is the backstop that keeps an idle machine converged: launchd runs
'brain stores sync --quiet' at load and every 5 minutes, logging under
~/Library/Logs/brain/stores-sync.log. A laptop that slept for a week converges
within one tick of waking, in one step.

  brain stores poller print        the launch agent plist, nothing written
  brain stores poller install      write ~/Library/LaunchAgents/com.brain.stores-sync.plist and load it
  brain stores poller uninstall    unload and remove it (sync is idempotent; stopping only stops catch-up)
  brain stores poller status       is it installed and loaded

```
bd stores poller [flags]
```

##### bd brain stores poller install

Install and load the launch agent on this machine

```
bd stores poller install [flags]
```

**Flags:**

```
      --dry-run          Say what would be done and change nothing
      --interval int     Seconds between runs (default 300)
      --no-load          Write the plist but do not run launchctl
      --wrapper string   Absolute path of the brain wrapper the job runs (default ~/.local/bin/brain)
```

##### bd brain stores poller print

Print the launch agent plist (writes nothing)

```
bd stores poller print [flags]
```

**Flags:**

```
      --interval int     Seconds between runs (default 300)
      --wrapper string   Absolute path of the brain wrapper the job runs (default ~/.local/bin/brain)
```

##### bd brain stores poller status

Say whether the launch agent is installed and loaded

```
bd stores poller status [flags]
```

##### bd brain stores poller uninstall

Unload and remove the launch agent

```
bd stores poller uninstall [flags]
```

**Flags:**

```
      --dry-run   Say what would be done and change nothing
      --no-load   Remove the plist without running launchctl
```

#### bd brain stores reconcile

Merge an out-of-sync brain back into one.

Without --apply it only reads: it compares this machine's stores.yaml and alias
wrappers and the database's namespace record with the shared registry, and
writes
  ~/.config/brain/reconcile-report.md        what is certain and what is not
  ~/.config/brain/reconcile-decisions.yaml   one line per name the database knows
                                             that no registry lists (undecided)

With --apply it adopts the certain stores and aliases into the registry (one
transaction, one epoch), applies every name you decided 'store' in the
decisions file, and then syncs this machine. Names left undecided stay
undecided; 'not-a-store' names are never registered. Run it on one machine
(the one whose stores.yaml is the truth), then 'brain stores sync' (or just
wait) everywhere else.

```
bd stores reconcile [flags]
```

**Flags:**

```
      --apply              Write the adoption into the shared registry (default: report only)
      --decisions string   Decisions file (default ~/.config/brain/reconcile-decisions.yaml)
      --report string      Report file (default ~/.config/brain/reconcile-report.md)
```

#### bd brain stores registry

Inspect the shared store registry (export, log, report)

```
bd stores registry [flags]
```

##### bd brain stores registry export

Print the registry snapshot (state, entries, machine reports) as JSON

```
bd stores registry export [flags]
```

##### bd brain stores registry log

Show the registry's append-only history, newest first

```
bd stores registry log [flags]
```

**Flags:**

```
      --limit int   How many rows (default 30)
```

##### bd brain stores registry report

Record a machine's applied state (used by client machines through their relay host)

```
bd stores registry report [flags]
```

**Flags:**

```
      --machine-json string   The machine report as JSON
```

#### bd brain stores remove

Remove a store name from the brain fleet.

When the shared registry is adopted, the name is tombstoned in the registry in
one transaction (epoch +1). This machine then syncs at once and every other
machine on its next command or within 5 minutes: each removes only its own
GENERATED wrapper, ~/.pi/agent/bin link and stores.yaml/stores.env line.
Nothing else is touched: the store's data directory, its rows in the database
and its id prefix all stay (a removed store can be created again later).
A wrapper that brain did not generate is reported, never deleted.

--local-only (or a registry that is not adopted yet) keeps the old behaviour:
the line leaves this machine's stores.yaml and stores.env is regenerated;
no other machine is told.

```
bd stores remove <name> [flags]
```

**Flags:**

```
      --local-only   Act on this machine only; do not write the shared store registry (other machines are not told)
```

#### bd brain stores rename

Rename a connected store end-to-end. Does, in order:

  1. Rename ~/data/&lt;old-name&gt;/  →  ~/data/&lt;new-name&gt;/
     (if the path follows the default ~/data/&lt;name&gt;/ convention).
     A custom path is left in place; only the registry key changes.
  2. Rewrite the wrapper at ~/.local/bin/&lt;old-name&gt; to ~/.local/bin/&lt;new-name&gt;
     pointing at the new path. Old wrapper is removed unless --keep-old-wrapper.
  3. Update ~/.config/brain/stores.yaml: &lt;old-name&gt; → &lt;new-name&gt;.
  4. Regenerate ~/.config/brain/stores.env.

What this does NOT do:
  - The underlying Dolt database name stays the same — existing bead IDs
    keep their original prefix (e.g. agent-XXXXX stays agent-XXXXX after
    renaming 'agents' to 'robots'). The transfer-verb's builtin alias
    table maps the old name to the new for legacy lookups.
  - The exfiltrated markdown root moves with the directory, so future
    'bd render' calls write to ~/data/&lt;new-name&gt;/entries/ automatically.

Examples:
  brain stores rename agents robots
  brain stores rename fishes whales
  brain stores rename agents robots --keep-old-wrapper   # leave both names working

```
bd stores rename <old-name> <new-name> [flags]
```

**Flags:**

```
      --keep-old-wrapper   Leave ~/.local/bin/<old-name> in place pointing at the renamed path
      --local-only         Act on this machine only; do not write the shared store registry (other machines are not told)
```

#### bd brain stores render-all

Iterate every store registered in ~/.config/brain/stores.yaml and
trigger markdown exfiltration for each one. The current bd binary is
re-invoked once per store with BEADS_DIR pinned, so per-store summaries
land on stderr exactly as a stand-alone 'bd render-all' would.

Useful after:
  - Installing a new bd binary that changes the exfiltration contract
    (every-kind exfil, store-derived root, etc.).
  - Restoring a store from backup where the markdown sidecar drifted.
  - Adding a new store to the registry and wanting it populated.

A federation-level summary line is emitted on stderr at the end:
  Federation: &lt;N&gt; stores OK, &lt;M&gt; stores failed (rendered &lt;R&gt;, failed &lt;F&gt;)

JSON mode (--json) replaces the per-store stdout streams with one
structured object per store and a top-level summary.

Exit codes:
  0 — every store reported success
  1 — at least one store had per-bead failures or could not be opened

```
bd stores render-all [flags]
```

**Flags:**

```
      --json   Emit a structured JSON object instead of per-store text summaries
```

#### bd brain stores rewrite-wrappers

Rewrites the BEADS_DIR and BRAIN_KNOWLEDGE_ROOT lines of each existing
~/.local/bin wrapper (registered stores and 'brain stores alias' wrappers) to
"$HOME/…" in place, preserving every other env pin and the exec line. Stores
with no wrapper are skipped. Each wrapper is then read through the way
'brain stores doctor' does.

Exits non-zero, naming every store that failed, if any wrapper cannot be
written or does not resolve a database. 'brain remote set' and
'brain remote install' run this on the brain host.

```
bd stores rewrite-wrappers [flags]
```

**Flags:**

```
      --jobs int           Verify this many stores concurrently (default 8)
      --timeout duration   Per-store read timeout (default 20s)
```

#### bd brain stores set-about

Attach a short description to a registered store, stored in the
registry (~/.config/brain/stores.yaml) only. The wrapper script is left
unchanged. View blurbs with 'brain stores list --verbose'.

Pass an empty string to clear the blurb.

Examples:
  brain stores set-about robots "agent-detected tooling defects"
  brain stores set-about ideas ""     # clear the blurb

```
bd stores set-about <store> <blurb> [flags]
```

#### bd brain stores shell-hook

Print a shell function that, when you type a store name this machine has no
wrapper for yet, asks the registry (the cached copy, instantly) and, if the
name is a store, runs it through 'brain store'. Add to your shell rc:

  eval "$(brain stores shell-hook zsh)"

```
bd stores shell-hook [zsh|bash] [flags]
```

#### bd brain stores sync

Reconcile this machine with the fleet's store registry.

The data of every store lives in the one shared database; so does the list of
stores (names, prefixes) and a version counter, the epoch, that goes up in the
same transaction as every create, rename or remove. What a machine can lack is
only the local shim that points at that data: the wrapper command in
~/.local/bin, its line in stores.yaml / stores.env, its BEADS_DIR skeleton, and
its ~/.pi/agent/bin link. 'sync' computes the difference and applies it.

  - It reads state, not events: a machine that slept through ten changes
    converges in one run.
  - It is idempotent: a second run changes nothing.
  - It never deletes data and never touches a file brain did not generate (a
    wrapper without the 'Auto-generated by brain stores' marker is reported,
    not rewritten or removed). Replaced or removed files are backed up under
    ~/.local/share/brain/registry-backup/&lt;time&gt;/ with a restore.sh.
  - A removed store leaves a tombstone; each machine removes only its own
    generated wrapper and link. Database rows and data directories stay.

Triggers: the launchd poller (every 5 minutes and at load), the ride-along on
every bd/brain command, and create/rename/remove on the machine that made the
change. --dry-run prints the plan and changes nothing.

--machine NAME runs the sync on that fleet machine over ssh (see
'brain stores doctor --fleet' for the roster).

Exit codes: 0 converged (or nothing adopted yet), 1 partial (items listed),
3 the registry could not be read.

```
bd stores sync [flags]
```

**Flags:**

```
      --dry-run          Print the plan and change nothing
      --machine string   Run the sync on this fleet machine over ssh (default: this machine)
      --quiet            Print only when something changed or failed (for the poller's log)
      --trigger string   What started this run (recorded for the log: timer, manual, ...) (default "manual")
      --wait duration    How long to wait for another sync running here (default 30s)
```

#### bd brain stores verify

Compare this machine's real wrappers, agent links, stores.yaml and stores.env
with the registry and print what is missing or orphaned. Changes nothing.
'brain stores doctor --fleet' runs this over ssh on every machine in the roster.

```
bd stores verify [flags]
```

**Flags:**

```
      --probe int   Also read through the wrappers of the newest N stores
```

### bd brain unify

brain stores each keep their own Dolt database on a shared server. 'unify'
consolidates them into ONE authoritative database while preserving the logical
separation between stores.

The mechanism is prefixes. A bead id is already self-describing
("brain-se7t.389" is in the "brain" namespace), so store identity is already
encoded in the primary key and unification needs no schema change, no id
rewrite and no query rewrite. What the separate databases carried implicitly —
which physical database a row came from, which store declares which prefix —
is written into brain_stores and brain_store_prefixes.

Phases:

  plan    read production and print the deterministic mapping: participating
          databases, namespace ownership, and every id that exists in more
          than one database and what becomes of it
  build   construct the unified database in an isolated Dolt server started
          under --data-dir, reading production but never writing to it
  replay  apply the changes the sources made since the build into the unified
          database, so it can be kept current until it becomes the reference
  verify  compare the unified database against production mechanically, by
          row count, content size and an order-independent content digest;
          --reference live compares against the sources as they stand now

Duplicated ids. An id that more than one store holds is merged into one bead
when the copies are identical. When the copies differ, nothing is picked and
nothing is discarded: each copy becomes a bead of its own under a new id
(&lt;authoring store's prefix&gt;-&lt;12 hex of sha256(id NUL store)&gt;), carrying that
copy's row and child rows, and the original id becomes an open conflict bead
that says so and lists both copies. Links other beads hold to the original id
still point at it, so they reach the conflict bead.

Production safety: this command group opens production read-only. The builder
writes only to a Dolt server it starts itself, so a build cannot modify a live
store even by accident.

```
bd brain unify [flags]
```

#### bd brain unify build

Construct the unified database in an isolated Dolt server from live store data

```
bd brain unify build [flags]
```

**Flags:**

```
      --aliases string                JSON file {wrapper name (BD_NAME): store} for wrappers whose name is not their store's; recorded in brain_store_aliases so a wrapper resolves to its store
      --data-dir string               scratch directory for the isolated dolt server holding the unified database (required)
      --database string               name of the unified database inside the isolated server (default "brain_unified")
      --dolt-bin string               dolt binary used to start the isolated server (default "dolt")
      --host string                   dolt sql-server host holding the production stores (default "127.0.0.1")
      --include-database strings      unregistered database that holds several prefixes but is a store, not a cross-store replica; it participates as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --port int                      dolt sql-server port holding the production stores (default 3307)
      --project-id strings            project id of a store neither its database nor the registry identifies, as <store>=<uuid> (repeatable); a store left without one only draws a warning from the build
      --rescue-orphans-from strings   replica database that stays excluded as a store but whose beads no store holds are brought in as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --template string               store whose schema the unified database inherits (default: the store with the most beads)
      --timeout duration              overall time budget for the build (default 4h0m0s)
```

#### bd brain unify plan

Print the deterministic mapping from every current store into the unified database

```
bd brain unify plan [flags]
```

**Flags:**

```
      --host string                   dolt sql-server host holding the production stores (default "127.0.0.1")
      --include-database strings      unregistered database that holds several prefixes but is a store, not a cross-store replica; it participates as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --json                          emit the plan as JSON
      --port int                      dolt sql-server port holding the production stores (default 3307)
      --project-id strings            project id of a store neither its database nor the registry identifies, as <store>=<uuid> (repeatable)
      --rescue-orphans-from strings   replica database that stays excluded as a store but whose beads no store holds are brought in as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --template string               store whose schema the unified database inherits (default: the store with the most beads)
      --timeout duration              overall time budget for the plan (default 20m0s)
```

#### bd brain unify ready

ready asks the running server that hosts the merged database whether it holds
everything the sources hold. It is a replay that decides everything and applies
nothing, so it is change-driven and fast: per source it compares the Dolt head
with the recorded starting point, lists the tables that changed since it
(dolt_diff_summary), and compares the tables Dolt does not version, and the small
state tables, by fingerprint with what was recorded. It prints READY, or what a
replay would reload, and exits non-zero when more than --max-pending items
differ. Sources that are being written to are never identical for long; what
matters is how small the difference is at the moment you repoint, because the
post-repoint catch-up ('replay --protect-after') closes the rest.

With --verify it also runs the recorded verification of the merged database
(every table's count, size and digest against what the build and the replays
recorded), which proves the merged database itself is intact.

```
bd brain unify ready [flags]
```

**Flags:**

```
      --database string               name of the merged database (default "brain_unified")
      --dolt-bin string               dolt binary (unused with --merged-port; kept for symmetry) (default "dolt")
      --host string                   dolt sql-server host holding the source stores (default "127.0.0.1")
      --include-database strings      unregistered database that holds several prefixes but is a store, not a cross-store replica; it participates as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --max-pending int               ready when at most this many items (beads, tables, state tables, collision records) differ
      --merged-host string            host of a RUNNING dolt server that hosts the merged database (with --merged-port; replaces --data-dir) (default "127.0.0.1")
      --merged-port int               port of a RUNNING dolt server that hosts the merged database; the replay updates it in place and nothing is stopped
      --port int                      dolt sql-server port holding the source stores (default 3307)
      --rescue-orphans-from strings   replica database that stays excluded as a store but whose beads no store holds are brought in as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --timeout duration              overall time budget (default 30m0s)
      --verify                        also run the recorded verification of the merged database (a few minutes)
```

#### bd brain unify replay

A merged database is built from a moment in the past. Every bead written,
edited or closed in a source store after that moment exists only in the source.
'replay' carries those changes into the merged database, so it can be kept
current until it becomes the reference.

For every source it reads the Dolt history since the commit 'unify build'
recorded (brain_unify_source_commits), finds the beads and the database-state
tables that changed, and re-reads exactly those from the source as it stands
now. Inserts, updates and deletes are one operation: the merged database's rows
for a changed bead are made equal to what the sources hold for it. Duplicated
ids are handled by the build's own functions: identical copies stay one bead;
copies that differ become a conflict bead plus one minted bead per copy, so a
change that creates, edits or removes such a duplicate re-derives the conflict
bead and its copies. Every duplicate is recorded in brain_unify_collisions.
Tables Dolt keeps no history for (wisps) are reloaded whole. The next replay
starts where this one read.

Anything it cannot resolve confidently is a refusal that names the store and the
table, and a refused replay leaves the merged database untouched:

  - a merged database built before builds recorded their commits (rebuild it)
  - a store that joined, left or moved to another database since the build
  - a source whose history no longer contains its recorded commit
  - a source table the build imported rows from that is gone
  - a table or column the merged schema does not have
  - a merged database built before differing copies became conflict beads
  - a minted copy id that two copies derive, or that an existing bead has

Prove a replay with 'unify verify --reference live', which compares the merged
database against the sources as they stand. Sources are only ever read.

```
bd brain unify replay [flags]
```

**Flags:**

```
      --data-dir string               directory holding the merged database built by 'unify build' (required)
      --database string               name of the merged database (default "brain_unified")
      --dolt-bin string               dolt binary used to start the server over the merged database (default "dolt")
      --dry-run                       decide and report what would be reloaded; apply nothing
      --host string                   dolt sql-server host holding the source stores (default "127.0.0.1")
      --include-database strings      unregistered database that holds several prefixes but is a store, not a cross-store replica; it participates as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --merged-host string            host of a RUNNING dolt server that hosts the merged database (with --merged-port; replaces --data-dir) (default "127.0.0.1")
      --merged-port int               port of a RUNNING dolt server that hosts the merged database; the replay updates it in place and nothing is stopped
      --no-commit                     do not make a Dolt commit of the merged database (use once stores write to it)
      --port int                      dolt sql-server port holding the source stores (default 3307)
      --protect-after string          UTC time (2006-01-02 15:04:05) the stores were repointed to the merged database: a bead the merged database changed after it is not reloaded from the older databases
      --rescue-orphans-from strings   replica database that stays excluded as a store but whose beads no store holds are brought in as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --timeout duration              overall time budget for the replay (default 4h0m0s)
```

#### bd brain unify verify

Compare the unified database against production by counts, size and content digest

```
bd brain unify verify [flags]
```

**Flags:**

```
      --data-dir string               directory holding the unified database built by 'unify build' (required)
      --database string               name of the unified database (default "brain_unified")
      --dolt-bin string               dolt binary used to start the server over the unified database (default "dolt")
      --host string                   dolt sql-server host holding the production stores (default "127.0.0.1")
      --include-database strings      unregistered database that holds several prefixes but is a store, not a cross-store replica; it participates as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --merged-host string            host of a RUNNING dolt server that hosts the merged database (with --merged-port; replaces --data-dir) (default "127.0.0.1")
      --merged-port int               port of a RUNNING dolt server that hosts the merged database; the replay updates it in place and nothing is stopped
      --port int                      dolt sql-server port holding the production stores (default 3307)
      --reference string              what to compare the merged database against: 'recorded' (the fingerprints the build or last replay took) or 'live' (the sources as they stand now; the acceptance test for a replay) (default "recorded")
      --rescue-orphans-from strings   replica database that stays excluded as a store but whose beads no store holds are brought in as db:<name> (repeatable; give the same value to plan, build, replay and verify)
      --template string               store whose schema the unified database inherited
      --timeout duration              overall time budget for verification (default 3h0m0s)
```
