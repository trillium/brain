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

## Syncing one project's items

A store holds far more than one project's work, and much of it must never leave
the machine (clients, money, machines, accounts). A repository, on the other
hand, belongs to one project. `--project` joins the two: it sends **only the
items that belong to one project record**, to **the repository that record
names**, **one way, brain to GitHub**.

```text
brain github sync --project project-73bi.1 --dry-run     # the full list, changes nothing
brain github sync --project project-73bi.1               # publish it
```

Project records live in the `projects` store. Give the id of the record
(`projects list` shows the tree). The records are read from the shared
database, so a record in `projects` scopes a sync of any other store; run the
command through the store whose items you want sent (`brain`, `task`, `fleet`, ...).

### Which items belong to a project

An item belongs to the project when **either** of these is true, and nothing
else counts:

1. **It sits under the record** in the parent-child hierarchy, at any depth
   (a child task, a grandchild), in any store.
2. **It carries a label that names the project:** `project:<record id>` (for
   example `project:project-7u7w`), the same label for any record beneath the
   project, or a label the record opts in with its metadata key
   `project_labels` (for example `"project:brain"`).

The record itself is the scope, not an item, and is never sent. A title that
mentions the project, a similar-looking label (`project:project-12` is not
`project:project-1`) or a store that merely shares a name with the project do
not count. Ephemeral items and system-internal types (`message`, `molecule`,
`gate`, `event`) never go to GitHub, scope or no scope.

Labels in the form `project:<slug>` (`project:brain`, `project:firstmate`) are
already used across the stores, but a slug does not say which project record it
means, so brain does not guess. Opt in on the record:

```text
projects update project-73bi.1 --set-metadata project_labels=project:brain
```

or for one run: `brain github sync --project project-73bi.1 --project-label project:brain --dry-run`
(repeatable; `brain` is read as `project:brain`).

### Which repository

The repository comes from the **project record**, never from the store's
pairing and never from a guess. brain looks in this order and takes the first
place that names a GitHub repository:

1. metadata `repo` (or `github_repo`), as `owner/name` or a `github.com` URL
2. a label `repo:owner/name`
3. the record's `external_ref`, when it is a `github.com` URL
4. an `origin github.com/owner/name` line in the description (the form the
   repository scan writes)

A value that is not a GitHub repository (a local path such as
`/Users/me/code/brain`, a bare name, an ssh remote) is skipped and reported,
not guessed at. Two different repositories in the same place is an error. A
record that names none is an error that says how to add one:

```text
projects update project-73bi.1 --set-metadata repo=trillium/brain
```

`--repo owner/name` may stand in for a missing or different repository on a
`--dry-run` only, so a preview can name a repository the record does not name
yet. A real run goes only where the record says.

### What a project run does and does not do

- **Always one way.** It pushes; it never pulls. `--pull-only`,
  `--direction two-way` and the conflict flags are refused.
- **Leaves the rest of the store alone.** Nothing outside the project is read
  into the plan, linked or published, and the store's recorded sync position
  is not moved, so a later whole-store sync starts where it would have.
- **Refuses a public repository until you say so.** The plan prints whether
  the repository is public or private. A real run into a public repository
  stops with the item count unless `--allow-public` is given (a dry run never
  needs it); if brain cannot tell, it stops too. The dry run is the place to
  read the list before anything is published.
- **Will not publish into a repository other than the store's pairing.** If
  the store is paired with a different repository, the project run is a
  `--dry-run` only (`github unpair` first), so one store's items do not end up
  linked into two repositories.
- Everything else is as for any push: items and issues are linked by URL and
  by a hidden marker, so running it again creates no second copy.

### Reading the project plan

```text
$ brain github sync --project project-w1d --dry-run
Project scope: project-w1d  Widgets
  repository: acme/widgets (from metadata repo), private
  one way, brain to GitHub; an item belongs when it is under the record or carries one of: project:project-w1d
  2 of 3250 in this store belong to the project:
    task-a1b  [open]  Add the flange  (under project-w1d)
    task-c2d  [open]  Document the flange  (label project:project-w1d)

Dry run mode - no changes will be made

  [dry-run] Would create in GitHub: Add the flange
  [dry-run] Would create in GitHub: Document the flange

Plan (dry run, nothing changed): brain <-> acme/widgets (brain-to-github)
  GitHub would: create 2, update 0, link 0 existing
  brain  would: create 0, update 0
  project project-w1d: 2 in scope, the rest of the store (3248) is left alone
  unchanged 0, errors 0
```

`--json` carries the same list under `project.items`, with the reason for each,
plus `project.repo`, `project.repo_source` and `project.visibility`.

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
  must be the one the items were synced with.
- *Items are counted as skipped and never pushed*: they are linked into another
  repository. A sync leaves an item linked elsewhere alone, link and all, rather
  than reading its issue number in the paired repository.
- *Hit a rate limit*: the sync stops cleanly and the next run continues; links
  and the marker make a half-finished run safe to repeat.
