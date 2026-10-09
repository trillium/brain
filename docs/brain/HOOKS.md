# Hooks: guards, observers, and warnings

A hook is a small program you declare for a store. It is told about every
mutation and can do one of two things: **refuse** it, or **notice** it. It can
never write. The store is the only writer.

This page covers how to declare a hook, what the three failure policies mean,
and what a warning guarantees. (The older `.beads/hooks/on_create`,
`on_update`, `on_close` scripts and the git hooks installed by `bd hooks` are a
different, unchanged mechanism; the verbs here are `bd hook`, singular.)

## Declaring a hook

One file per hook, in the store's own `.beads` directory:

```
<project>/.beads/hooks.d/<name>.toml
```

There is no daemon and no registry. `bd` lists the directory at the start of
every command. The file name is the hook's name; if you write a `name = ...`
key it must match, or the file is refused.

```toml
# .beads/hooks.d/no-secrets.toml
run    = '''
payload=$(cat)                       # the mutation, as JSON, on stdin
case "$payload" in
  *AKIA*) echo "looks like an AWS key" >&2; exit 2 ;;
esac
'''
policy = "guard"                     # guard | observer | (omit: warns)
when   = "create"                    # create | update | close | delete | * (default *)
timeout = "5s"                       # default 10s
refusal_exit = 2                     # default 2; guards only
```

| key            | meaning                                                                 |
|----------------|-------------------------------------------------------------------------|
| `run`          | required. Executed with `/bin/sh -c`, payload on stdin.                 |
| `policy`       | `guard`, `observer`, or leave it out (see below).                       |
| `when`         | which mutations fire it. `update` also covers labels, dependencies and comments. A status change made through `bd update` is an `update`, not a `close`. |
| `timeout`      | a hook is killed after this long.                                       |
| `refusal_exit` | the exit status that means "refuse". Guards only.                       |

Check your work without writing anything:

```
bd hook list                 # every hook, and every file that cannot be loaded
bd hook test no-secrets --event create --payload-file payload.json
```

### What the hook is told

Stdin is one JSON document: `event`, `command`, `store` (the store's name, never
a path), `actor`, and `issue` (`id`, `title`, `description`, `status`,
`priority`, `issue_type`, `assignee`, `labels`, `created_by`). Updates carry
`updates`; closes carry `reason`. For a **create** guard the issue is the
proposed row and has no `id` yet. Observers run after the write, so they see the
`id`.

## The three policies

**`guard` — fails closed.** Runs *before* the write.
- exit `0`: the write proceeds.
- exit `refusal_exit` (default 2): the write is refused. Whatever the hook
  printed to stderr is the reason.
- anything else — another exit status, a timeout, the shell not starting — also
  refuses, because a guard that could not finish deciding must not let the write
  through.

The refusal names the hook and is the reason the bead was never created:

```
Error: bead not created: refused by guard hook "no-secrets" (…/.beads/hooks.d/no-secrets.toml): looks like an AWS key
Error: bead not created: guard hook "license" (…) failed closed [hookfailed]: hook exited 7: license server down (its refusal exit is 2); a guard that cannot finish deciding refuses
```

**`observer` — fails open.** Runs *after* the write (after commit, inside a
transaction). It cannot block or undo anything. Exit `0` is silent. Any other
outcome — non-zero exit, timeout, would-not-start — leaves the write in place and
records a **warning**.

**no `policy` — warns.** The hook has not said what its failure should mean, and
`bd` will not decide for it in either direction. It runs after the write like an
observer and never blocks, but it produces a warning **every time it runs**, pass
or fail, until you declare a policy. It is a nudge to settle the question.

A hook definition that cannot be understood is never guessed at. An unknown key
(`polcy`), an unknown policy or `when`, a `name` that disagrees with the file, a
missing `run`, a bad timeout, a duplicate name, a stray non-`.toml` file or
directory in `hooks.d`: each refuses every command that could write, naming the
file and the problem. Read-only commands and `bd hook ...` keep working so you can
diagnose it.

## What a warning guarantees

A warning is not a quieter failure. When an observer or an unset-policy hook
degrades, `bd`:

1. writes a **durable record** into the store (the config table, key
   `hookwarning.<id>`, JSON value) — versioned and synced with the database,
   readable by anything that can read the store;
2. prints one line on stderr naming the hook, the reason and the record id;
3. keeps the record `open` until someone acknowledges it.

A record says: `id` (sortable, `hw-<utc time>-<random>`), `created_at`, `hook`,
`path`, `event`, `subject` (the bead), `command`, `reason`
(`hook-failed` | `policy-unset`), `detail` (what degraded, with the hook's own
output), `status` (`open` | `acknowledged`), and `ack_by` / `ack_at` once
acknowledged.

```
bd hook warnings                         # open warnings
bd hook warnings --status all --json     # machine-readable, for a router or a repair agent
bd hook warnings --ack <id>              # or --ack-all
```

If a warning cannot itself be recorded (for example the config write fails), the
write has already happened, so `bd` does not pretend it did not: it prints
`HOOK WARNING NOT RECORDED` on stderr with the whole warning inline. A stored
record that cannot be parsed makes `bd hook warnings` exit non-zero.

A refusal is not a warning; it is the command's own error and leaves nothing
behind except the exit status.

The guarantee stops at the record. Nothing here sends the warning anywhere or
retries anything: pointing a person or a repair agent at `open` records is the
job of whatever reads them.

## Hooks never write

A hook is handed no way to the store. It gets a payload on stdin and returns an
exit status; the warning record is written by the store layer, not the hook.
A hook that runs `bd create` itself is refused:

- the hook's environment has every `BEADS_*`, `BD_*`, `BRAIN_*`, `DOLT_*` and
  `MYSQL_*` variable removed and its working directory is an empty temporary
  directory, so it has no coordinates for the store;
- the process is marked as hook-started, and `bd` refuses every write
  (`operation 'create' refused: this process was started by a hook …`);
- while a hook runs, `bd` keeps `hooks-running.<pid>.lock` in the store's `.beads`
  directory, and any `bd` write whose ancestors include that process is
  refused — so a hook that knows the path and scrubs the marker is still refused.

Reads from a hook are allowed.

## Where hooks run, and what they do not cover

Guards and observers sit in the storage layer closest to the database, so every
`bd` path that creates, updates, closes, deletes, labels, links or comments goes
through them, including inside transactions. Not covered: bulk maintenance paths
that bypass those methods (import, `bd dolt pull` merges, compaction, delete by
source repo), and proxied-server mode — where hooks are declared, writes are
refused there rather than run unguarded.
