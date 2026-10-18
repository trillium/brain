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

The registry lives at ~/.config/brain/stores.yaml (legacy path
~/.config/pai/stores.yaml is a compat symlink kept for transition).
Each registered store
can be searched via 'brain search', transferred to via 'brain transfer',
and synced via 'brain repo sync'.

Run 'brain stores env' to regenerate ~/.config/brain/stores.env for
shell wrapper scripts that need BRAIN_STORE_* variables (deprecated
PAI_STORE_* aliases are still emitted for transition).

```
bd stores [flags]
```

### bd stores add

Register a store in the brain federation registry

```
bd stores add <name> <beads-dir> [flags]
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

  1. Provision the Dolt store. The store is provisioned on the federation's
     shared Dolt sql-server whenever one can be found and proven to answer:
     the shared-server pins the caller's wrapper exported (BEADS_DOLT_*)
     first, else the server the registered stores' metadata.json already
     resolves to, else 127.0.0.1:3307. A candidate that does not answer is a
     loud refusal, never a store silently pinned to a host that cannot be
     read — and never a silent fallback to an embedded store. Pass --embedded
     to ask for the embedded shape explicitly; then metadata.json
     (dolt_mode=server) and config.yaml are written first, and
     'dolt init' runs in &lt;path&gt;/.beads.
  2. Write &lt;path&gt;/.beads/config.yaml (issue-prefix + BD_NAME), in either
     shape.
  3. Create &lt;path&gt;/entries/ for exfiltrated markdown.
  4. Write a CLI wrapper at ~/.local/bin/&lt;name&gt;. On a shared server the
     wrapper exports the four server pins (mirroring the review/brain
     wrappers); embedded stores get the minimal BEADS_DIR/BD_NAME wrapper.
     Either way it exec's bd.
  5. Register the store in ~/.config/brain/stores.yaml.
  6. Regenerate ~/.config/brain/stores.env.

Default path is $HOME/data/&lt;name&gt;. Override with --path. Skip the wrapper
with --no-wrapper if you manage shell shims another way.

If any step fails after files have been written, the verb leaves the
partial state in place and exits non-zero — re-running with the same
arguments resumes idempotently. An abandoned server-side store must be
dropped by hand on the shared server; the verb never drops a database.

Examples:
  brain stores create recipes
  brain stores create recipes --path /Volumes/extra/recipes
  brain stores create recipes --bd-binary /opt/homebrew/bin/bd
  brain stores create scratch --embedded

```
bd stores create <name> [flags]
```

**Flags:**

```
      --bd-binary string    Path to bd that the wrapper should exec (default: "bd", resolved by PATH)
      --dolt-email string   --email arg to pass to 'dolt init' (skipped if empty)
      --dolt-name string    --name arg to pass to 'dolt init' (skipped if empty)
      --embedded            Create an embedded (self-starting) store even when a shared Dolt sql-server is known
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

The same walk counts the REGISTRY against the WRAPPERS: every registered store
should have exactly one wrapper on this host (~/.local/bin/&lt;name&gt;), and every
generated wrapper should belong to a registered store. A registered store with
no wrapper is a store nothing can reach under the name it is registered under;
a generated wrapper with no registry entry is a CLI in PATH resolving a store
the federation does not know about ('brain stores orphans' lists just those).
Missing wrappers are generated by --fix; an orphan is never deleted by --fix
(see the fix line for the two commands that clear it).

The same walk checks the CONNECTION itself: that the store-host record exists
and names a server that answers, and that each wrapper reads that record rather
than a baked host pin. A wrapper pinning BEADS_DOLT_SERVER_HOST/PORT is how a
host drifts to a stale local Dolt daemon and starts failing every store with
'Access denied' (robots-r1zd5): the pin wins over the record and nothing
re-reads it.

This verb is a DRY RUN by default: it reports what drifts and the exact command
that repairs it. With --fix it converges this machine — rewriting/generating
the wrappers that do not read the record (backing up the ones it changes), and
writing the record from the process environment when the record is missing and
BEADS_DOLT_SERVER_HOST/PORT are set. --fix never touches a Dolt daemon and never
opens a store for writing.

Run it from a scheduler (launchd/cron) or a session-start probe so a
half-provisioned store or a drifted host surfaces within a day rather than a
week.

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
      --fix                Repair what drifted: converge every wrapper on the shared store-host record (backing up what it changes) and write the record from BEADS_DOLT_SERVER_HOST/PORT when it is missing. Without --fix, the run writes nothing.
      --jobs int           Probe this many stores concurrently (default 8)
      --json               Emit a structured JSON object instead of per-store text lines
      --strict             Exit non-zero on warnings too (missing wrapper, stale registry path)
      --timeout duration   Per-store read timeout (default 20s)
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

### bd stores host

Show the federation's store-host record: which Dolt sql-server the stores live
on, where that value came from, and the credential REFERENCE (MySQL user and
the path of the password file) — never a password.

The record lives at ~/.config/brain/store-host.env (override the path with
BRAIN_STORE_HOST_FILE), as KEY=VALUE lines so the generated store wrappers can
source it at run time:

  BEADS_DOLT_SERVER_HOST=&lt;host&gt;
  BEADS_DOLT_SERVER_PORT=&lt;port&gt;
  BEADS_DOLT_SERVER_USER=&lt;mysql user&gt;          # optional
  BEADS_CREDENTIALS_FILE=&lt;password file path&gt;  # optional

A process environment value of the same name overrides the record. The record
is one file per machine because where the federation lives is one fact per
machine: baking it into wrappers, metadata and shell rc files is how a host's
stores drift apart and read as 'registered but does not answer'.

The server is also probed, so 'no record', 'record says somewhere dead' and
'wrong credentials' are distinguishable without opening a store.

```
bd stores host [flags]
```

**Flags:**

```
      --json             Emit a structured JSON object
      --probe duration   Dial the resolved server this long to report whether it answers (0 to skip) (default 2s)
```

#### bd stores host set

Write the federation's one store-host record: the Dolt host and port every
store on this machine should use, plus an optional credential REFERENCE (the
MySQL user and the path of the password file).

Unrelated lines in the record are preserved, so a hand-written comment or an
extra setting survives. The file is written atomically with 0600 permissions.
Secrets are refused: pass the password file's PATH with --credentials, never a
password.

```
bd stores host set [flags]
```

**Flags:**

```
      --credentials string   Path of the password file the record should reference (never a password)
      --host string          Dolt sql-server host (required)
      --port int             Dolt sql-server port (required)
      --user string          MySQL user for this server (a credential reference, not a secret)
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

### bd stores orphans

List every generated store wrapper in ~/.local/bin whose name is not in the
federation registry (~/.config/brain/stores.yaml).

A wrapper with no registry entry is a CLI in PATH that resolves a store the
federation does not know about: either the store was renamed or removed and the
wrapper was left behind, or it was never registered. Only files this tool
generated count (the generated header is the marker), so a hand-written script
that happens to share a store's name is never reported — or touched.

Nothing is deleted: register the store again with 'brain stores add &lt;name&gt;
&lt;beads-dir&gt;', or remove the file yourself.

```
bd stores orphans [flags]
```

### bd stores remove

Unregister a store from the brain federation registry

```
bd stores remove <name> [flags]
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

Converges this machine's store wrappers on the federation's one store-host
record:

  - every registered store WITHOUT a wrapper gets the canonical wrapper
    (~/.local/bin/&lt;name&gt;), so a store is reachable under the name it is
    registered under and the registry has no wrapper-less entry;
  - each existing wrapper's BEADS_DIR and BRAIN_KNOWLEDGE_ROOT lines become
    "$HOME/…", preserving every other env pin and the exec line;
  - a server-mode store's wrapper is repointed at the shared store host config
    (~/.config/brain/store-host.env): the run-time host block is added when
    missing and any baked BEADS_DOLT_SERVER_HOST/PORT pin is dropped, because a
    leftover pin would win over the record. Embedded stores keep their minimal
    wrapper, and forwarding wrappers installed by 'brain remote install' are
    left alone;
  - registry entries recorded under another machine's home are healed to this
    machine's equivalent and persisted.

Every wrapper that changes is backed up first, under
~/.local/state/brain/wrapper-backups/&lt;run&gt;/, so a machine can be un-converged.
Each wrapper is then read through the way 'brain stores doctor' does.

--dry-run reports what would change (and still reads every wrapper through)
without writing anything: no wrapper, no registry, no backup.

This is the one step that converges a host's stores on the federation's shared
server. 'brain remote set' and 'brain remote install' run it on the brain host
and fail when a store still cannot resolve a database.

```
bd stores rewrite-wrappers [flags]
```

**Flags:**

```
      --backup             Back up each wrapper before rewriting it, under ~/.local/state/brain/wrapper-backups/<run>/ (default true)
      --bd-binary string   Path a GENERATED wrapper should exec (default "bd", resolved by PATH); existing wrappers keep their own exec line (default "bd")
      --dry-run            Report what would change (and read each wrapper through) without writing wrappers, registry, or backups
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
