---
id: github
title: bd github
slug: /cli-reference/github
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc github`

## bd github

Commands for syncing a brain store with the issues of a GitHub repository.

A store is paired with one repository, and the pairing names the direction
changes travel (see 'github pair'):

  two-way          items and issues are kept in step both ways (default)
  brain-to-github  brain is the source; GitHub issues mirror it
  github-to-brain  GitHub is the source; brain items mirror it

Configuration can be set via 'bd config' or environment variables:
  github.token / GITHUB_TOKEN           - Personal access token
  github.owner / GITHUB_OWNER           - Repository owner
  github.repo / GITHUB_REPO             - Repository name
  github.repository / GITHUB_REPOSITORY - Combined "owner/repo" format
  github.url / GITHUB_API_URL           - Custom API URL (GitHub Enterprise)

With no token configured, 'gh auth token' (the GitHub CLI's login) is used.

```
bd github [flags]
```

### bd github pair

Record which GitHub repository this store syncs with, and which way changes travel.

  brain github pair trillium/brain                                  two-way (default)
  brain github pair trillium/brain --direction brain-to-github      brain is the source
  brain github pair trillium/brain --direction github-to-brain      GitHub is the source
  brain github pair trillium/brain --conflict prefer-local          brain wins two-way conflicts

The pairing is saved in this store's own config and touches no other store.
Nothing is synced by pairing; run 'brain github sync --dry-run' to see what the
first sync would do.

Pair one repository with one store. Two stores two-way paired with the same
repository would each import the other's native issues.

```
bd github pair <owner/repo> [flags]
```

**Flags:**

```
      --conflict string    Two-way conflict policy: prefer-newer (default), prefer-local or prefer-github
      --direction string   Direction: two-way (default), brain-to-github or github-to-brain
      --force              Repoint a store that is already paired with a different repository
      --no-verify          Do not check that the repository exists and is reachable
```

### bd github pull

Pull one or more items from GitHub.

Accepts bead IDs or external references as positional arguments.
Equivalent to: bd github sync --pull-only --issues &lt;refs&gt;

```
bd github pull [refs...] [flags]
```

**Flags:**

```
      --dry-run   Preview pull without making changes
```

### bd github push

Push one or more beads issues to GitHub.

Accepts bead IDs as positional arguments.
Equivalent to: bd github sync --push-only --issues &lt;ids&gt;

```
bd github push [bead-ids...] [flags]
```

**Flags:**

```
      --dry-run   Preview push without making changes
```

### bd github repos

List GitHub repositories that the configured token has access to.

```
bd github repos [flags]
```

### bd github schedule

Manage a background job that runs 'github sync' for this store.

  brain github schedule print      show the launchd job without installing it
  brain github schedule install    write the job and load it (macOS launchd)
  brain github schedule status     say whether the job is installed and loaded
  brain github schedule remove     unload and delete the job

The job runs the store's own command, so it uses the store's pairing and
direction. It needs a recorded pairing ('github pair'), and the GitHub login
the sync uses (GITHUB_TOKEN or the gh CLI's login) must be available to a
background process. Nothing is scheduled until 'install' is run.

Without launchd, 'brain github sync --watch --interval 10m' keeps syncing in a
terminal, and the same command line works in a cron entry.

```
bd github schedule [flags]
```

#### bd github schedule install

Write the launchd job and load it

```
bd github schedule install [flags]
```

**Flags:**

```
      --interval duration   Time between syncs (minimum 1m) (default 15m0s)
```

#### bd github schedule print

Show the launchd job without installing it

```
bd github schedule print [flags]
```

**Flags:**

```
      --interval duration   Time between syncs (minimum 1m) (default 15m0s)
```

#### bd github schedule remove

Unload and delete the launchd job

```
bd github schedule remove [flags]
```

#### bd github schedule status

Say whether the launchd job is installed and loaded

```
bd github schedule status [flags]
```

### bd github status

Display current GitHub configuration and sync status.

```
bd github status [flags]
```

### bd github sync

Synchronize this store's items with the issues of its paired repository.

The direction comes from the store's pairing ('github pair'). A store with no
recorded direction syncs two-way:
- Pulls new/updated issues from GitHub into brain
- Pushes brain items to GitHub

An item and its issue stay linked: the item remembers the issue's URL, and the
issue carries a hidden marker naming the item, so running sync again never
creates a second copy of either. Closing or reopening on one side is carried
to the other. Comments are not synced.

On a two-way pairing, --pull-only or --push-only narrows a single run to one
half. On a one-way pairing the flag for the opposite half is refused.

If an item and its issue were both changed since the last sync, the conflict
policy decides: --prefer-newer (default) keeps whichever side was edited last,
--prefer-local keeps brain's version, --prefer-github keeps GitHub's. The
policy can be recorded on the pairing with 'github pair --conflict'.

Use --dry-run first: it prints what each side would create, update or link,
with counts, and changes nothing.

With --watch the command stays running and syncs every --interval.

```
bd github sync [flags]
```

**Flags:**

```
      --direction string    Direction for this run only when the store has no recorded pairing (two-way, brain-to-github, github-to-brain)
      --dry-run             Show what would be synced without making changes
      --interval duration   Time between syncs with --watch (minimum 30s) (default 5m0s)
      --issues string       Comma-separated bead IDs to sync selectively (e.g., bd-abc,bd-def). Mutually exclusive with --parent.
      --parent string       Limit push to this bead and its descendants (push only). Mutually exclusive with --issues.
      --prefer-github       On conflict, use GitHub version
      --prefer-local        On conflict, keep local beads version
      --prefer-newer        On conflict, use most recent version (default)
      --pull-only           Only pull GitHub issues into brain
      --push-only           Only push brain items to GitHub
      --repo string         Run against this owner/repo for this run only, without saving a pairing
      --watch               Keep running and sync every --interval (off by default; see 'github schedule' for a background job)
```

### bd github unpair

Forget the repository, direction and conflict policy for this store.

Items keep the link they remember to their issues and the issues keep their
markers, so pairing the same repository again picks up where it left off. No
item and no issue is deleted.

```
bd github unpair [flags]
```
