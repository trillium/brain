---
id: stores
title: bd stores
slug: /cli-reference/stores
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc stores`

## bd stores

Manage the registry of bd stores federated under brain.

The registry lives at ~/.config/brain/stores.yaml. Each registered store
can be searched via 'brain search', transferred to via 'brain transfer',
and synced via 'brain repo sync'.

Run 'brain stores env' to regenerate ~/.config/brain/stores.env for
shell wrapper scripts that need BRAIN_STORE_* variables.

```
bd stores [flags]
```

### bd stores add

Register a store in the brain federation registry

```
bd stores add <name> <beads-dir> [flags]
```

**Flags:**

```
      --local-only   Act on this machine only; do not write the shared store registry (other machines are not told)
```

### bd stores alias

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

### bd stores create

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

### bd stores doctor

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

### bd stores edit-back

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

### bd stores env

Write ~/.config/brain/stores.env from the registry (for shell wrappers)

```
bd stores env [flags]
```

### bd stores epoch

Print the registry epoch (database) and the epoch applied on this machine

```
bd stores epoch [flags]
```

### bd stores list

List all registered stores

```
bd stores list [flags]
```

**Flags:**

```
  -v, --verbose   Include each store's about blurb as an extra column
```

### bd stores poller

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

#### bd stores poller install

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

#### bd stores poller print

Print the launch agent plist (writes nothing)

```
bd stores poller print [flags]
```

**Flags:**

```
      --interval int     Seconds between runs (default 300)
      --wrapper string   Absolute path of the brain wrapper the job runs (default ~/.local/bin/brain)
```

#### bd stores poller status

Say whether the launch agent is installed and loaded

```
bd stores poller status [flags]
```

#### bd stores poller uninstall

Unload and remove the launch agent

```
bd stores poller uninstall [flags]
```

**Flags:**

```
      --dry-run   Say what would be done and change nothing
      --no-load   Remove the plist without running launchctl
```

### bd stores reconcile

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

### bd stores registry

Inspect the shared store registry (export, log, report)

```
bd stores registry [flags]
```

#### bd stores registry export

Print the registry snapshot (state, entries, machine reports) as JSON

```
bd stores registry export [flags]
```

#### bd stores registry log

Show the registry's append-only history, newest first

```
bd stores registry log [flags]
```

**Flags:**

```
      --limit int   How many rows (default 30)
```

#### bd stores registry report

Record a machine's applied state (used by client machines through their relay host)

```
bd stores registry report [flags]
```

**Flags:**

```
      --machine-json string   The machine report as JSON
```

### bd stores remove

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

### bd stores rename

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

### bd stores render-all

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

### bd stores rewrite-wrappers

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

### bd stores set-about

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

### bd stores shell-hook

Print a shell function that, when you type a store name this machine has no
wrapper for yet, asks the registry (the cached copy, instantly) and, if the
name is a store, runs it through 'brain store'. Add to your shell rc:

  eval "$(brain stores shell-hook zsh)"

```
bd stores shell-hook [zsh|bash] [flags]
```

### bd stores sync

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

### bd stores verify

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
