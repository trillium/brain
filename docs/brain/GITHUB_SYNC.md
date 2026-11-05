# Syncing a store with GitHub issues

`brain github` keeps one store's items and one GitHub repository's issues in
step. You pick, per store, **which repository** and **which way changes
travel**. Nothing else is affected: another store has its own pairing, or none.

```text
brain github pair trillium/brain                          # two-way (the default)
brain github pair trillium/brain --direction brain-to-github
brain github pair trillium/brain --direction github-to-brain
brain github sync --dry-run                               # preview, changes nothing
brain github sync                                         # do it
```

Run these through the store's own command (`brain`, `task`, `robots`, ...). The
command name is how brain knows which store you mean.

## Pairing a store with a repository

```text
brain github pair <owner/repo> [--direction D] [--conflict POLICY] [--force] [--no-verify]
brain github unpair
brain github status
```

`pair` saves three things in **that store's own config** (the same `github.*`
keys the integration always read, so it needs no new place to look):

| key                 | meaning                                                         |
|---------------------|-----------------------------------------------------------------|
| `github.repository` | the repository, `owner/repo`                                    |
| `github.direction`  | `two-way`, `brain-to-github` or `github-to-brain`               |
| `github.conflict`   | `prefer-newer`, `prefer-local` or `prefer-github` (two-way only) |
| `github.namespace`  | the store name written into every mirrored issue                |

In the shared brain database every store has its own config rows, and the sync
position is kept per store too, so pairing one store touches no other. `pair`
checks that the repository exists and your login can see it (skip with
`--no-verify`), refuses to silently repoint a store that is already paired
(`--force` repoints and restarts the sync position), and syncs nothing.
`unpair` forgets the pairing; no item and no issue is deleted.

Pair **one repository with one store**. Two stores two-way paired to the same
repository would each import the other's native issues.

## Choosing the direction

| direction         | who is the source          | what `sync` does                                                                                           |
|-------------------|----------------------------|------------------------------------------------------------------------------------------------------------|
| `two-way`         | neither                    | new and changed items go to GitHub, new and changed issues come into brain; an item changed on both sides is settled by the conflict policy |
| `brain-to-github` | brain                      | GitHub is a mirror. Items are created and updated as issues. Nothing is ever pulled. An edit made to a mirrored issue on GitHub is overwritten by the next sync. |
| `github-to-brain` | GitHub                     | Brain is a mirror. Issues are created and updated as items. Nothing is ever pushed. An edit made in brain to a mirrored item is overwritten by the next sync. |

On a two-way pairing, `--pull-only` or `--push-only` narrows a single run to one
half. On a one-way pairing the opposite flag is refused with a message, because
it contradicts the pairing: `brain github sync --pull-only` on a
`brain-to-github` pairing fails instead of quietly pulling. `brain github push`
and `brain github pull` (which name items) obey the same rule.

A store with a repository but no recorded direction behaves as `two-way`, as
`brain github sync` always has. For a one-off look at a repository without
saving a pairing, `brain github sync --dry-run --repo owner/repo --direction two-way`
runs ad hoc. An ad hoc `--repo` that differs from a recorded pairing is allowed
only with `--dry-run`.

### Conflicts (two-way only)

An item is in conflict when it was changed in brain **and** its issue was
changed on GitHub since the last sync. The policy decides:

- `prefer-newer` (**the default**): whichever side was edited last wins.
- `prefer-local`: brain's version wins.
- `prefer-github`: GitHub's version wins.

Set it on the pairing (`pair --conflict prefer-local`) or for one run with
`--prefer-newer`, `--prefer-local` or `--prefer-github`. One-way pairings have no
conflicts: the source always wins.

## What is carried, and what is not

Carried both ways: **title, description, open/closed state, priority, type,
labels**. Closing an item closes its issue; reopening one reopens it; closing or
reopening an issue does the same to its item. A status GitHub has no word for
(`in_progress`, `blocked`, `deferred`, `pinned`, a custom status) travels as a
`status::<name>` label and comes back unchanged. Priority and type travel as
`priority::<level>` and `type::<kind>` labels.

Not carried:

- **Comments.** The inherited GitHub sync has never moved them and this one
  does not either.
- **Dependencies and parent/child links.**
- **Deletions.** Deleting an item does not delete its issue, and the reverse.
  Close an item to close its issue. An issue whose item was deleted is left
  alone until someone edits it on GitHub; that edit pulls it back in as an item
  again (a full re-sync after `unpair`/`pair` does the same).
- **An item's assignee** is not sent to GitHub; a GitHub assignee is brain's
  only when the issue has one. A pull never blanks an item's own assignee.
- **System-internal item types** (`message`, `molecule`, `gate`, `event`) and
  **ephemeral items** never go to GitHub. Pull requests are never imported.

## Safe to run again and again

Every item and its issue are linked in two ways, so a repeat run never makes a
second copy of either:

1. The item remembers the issue's URL (`external_ref`). A URL that points into a
   *different* repository is not a link, so repointing a pairing cannot attach
   items to the wrong issue numbers.
2. The issue's body ends with a hidden line, invisible on GitHub:
   `<!-- brain-sync store=brain id=brain-7k2 -->`. It names the store and the
   item. If the stored URL is ever lost (an interrupted run, a restored backup),
   the next sync finds the issue by its marker and **links** it instead of
   creating another. `--dry-run` reports these as `link`.

The marker also names the store, so a repository shared by two stores never
imports one store's mirrors as the other's items. An issue someone created on
GitHub gets its marker on the sync after it is imported; until then it is linked
by URL only.

A sync that has nothing to do writes nothing: it compares each item with its
issue and skips the ones that already match, so re-running does not bump
issue timestamps or churn the history. Edits made in brain while a sync runs
are never overwritten by an older GitHub copy; they are pushed by the next run.

### Reading the plan

```text
$ brain github sync --dry-run

Plan (dry run, nothing changed): brain <-> trillium/brain (two-way)
  GitHub would: create 14, update 3, link 0 existing
  brain  would: create 2, update 1
  unchanged/skipped 410, conflicts 0, errors 0
```

`GitHub would create 14` is 14 new issues. `link` is issues that already exist
for an item whose stored URL was lost. The dry run asks GitHub for the same
information a real run does, so its counts are what a real run would do. Add
`--json` for the same numbers as JSON.

## Authentication

Set `GITHUB_TOKEN`, or `bd config set github.token ...` (kept in the config file
and never written to the shared database), or just be logged in with the GitHub
CLI: with no token configured, `gh auth token` is used. Tokens are never
printed or committed; `brain github status` shows only the first four
characters. GitHub Enterprise: set `github.url` / `GITHUB_API_URL`.

## Keeping it running (optional, off by default)

Nothing runs on its own until you ask.

- **Foreground:** `brain github sync --watch --interval 10m` syncs every ten
  minutes until you stop it (minimum 30s). A failed run is reported and retried.
- **macOS launchd job:** `brain github schedule install --interval 15m` writes
  `~/Library/LaunchAgents/dev.brain.github-sync.<store>.plist` and loads it. The
  job runs the store's own command with plain `github sync`, so it follows the
  store's pairing. `schedule print` shows the job without installing,
  `schedule status` says whether it is loaded, `schedule remove` unloads and
  deletes it. It refuses to install for a store with no recorded pairing. The
  GitHub login must be available to a background process (`GITHUB_TOKEN` in the
  environment the job gets, or a `gh` login).
- Elsewhere, a cron entry running `brain github sync` does the same job.

## Troubleshooting

- *`this pairing is brain-to-github: nothing is pulled`*: you asked for the half
  the pairing excludes. Change the pairing (`pair --force --direction ...`) or
  drop the flag.
- *`cannot tell which store this is`*: run the command through the store's own
  wrapper so its name is known.
- *Items keep reappearing as new*: check `brain github status`; the repository
  must be the one the items were synced with. Links into another repository are
  deliberately ignored.
- *Hit a rate limit*: the sync stops cleanly and the next run continues; links
  and the marker make a half-finished run safe to repeat.
