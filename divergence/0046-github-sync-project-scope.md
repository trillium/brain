---
id: 0046
title: scope a github sync to one project's items and send them to that project's repository
isc: []
status: proposed
created: 2026-11-05
updated: 2026-11-05
commits: []
touches:
  - cmd/bd/github.go
  - cmd/bd/github_project.go
  - cmd/bd/github_project_sync.go
  - cmd/bd/github_project_test.go
  - cmd/bd/github_project_embedded_test.go
  - cmd/bd/github_sync_embedded_test.go
  - internal/tracker/engine.go
  - internal/tracker/types.go
  - internal/tracker/engine_test.go
  - docs/brain/GITHUB_SYNC.md
  - docs/CLI_REFERENCE.md
  - website/docs/cli-reference/github.md
  - divergence/0046-github-sync-project-scope.md
upstream_rebase_notes: |
  `cmd/bd/github.go` and `internal/tracker/*` are upstream files with brain
  edits (see 0045). This change adds three flags to `github sync`, one hook
  call in `runGitHubSyncOnce` and one engine option (`KeepSyncPosition`, which
  skips the last-sync write). `github_project*.go` are brain-only; resolve
  `ours`. Other trackers are unaffected because none set the option.
---

# Why

The first full-store preview of `brain github sync` (store `brain` against the
public `trillium/brain`) would have created 3,242 issues, many of them about
clients, money, machines and accounts. Pairing a whole store with a repository
is the wrong unit for that: the unit that belongs in a repository is a
project. brain keeps project records in the `projects` store and the captain
wants only the items that belong to a project published, one way, into that
project's repository.

# What changed

- **`github sync --project <record id>`** limits a run to the items that belong
  to the project record and sends them to the repository the record names, one
  way (brain to GitHub). It reads the record from the shared database, so a
  record in `projects` scopes a sync of any store.
- **Membership is explicit.** An item belongs when it is under the record in
  the parent-child hierarchy (any depth, any store), or carries
  `project:<record id>` (for the record or any record beneath it), or a label
  the record opts in with metadata `project_labels`. `--project-label` adds
  labels for one run. Slug labels such as `project:brain` are not guessed to
  mean a record, because a slug does not say which record it means.
- **The repository comes from the record.** Metadata `repo`/`github_repo`, a
  `repo:` label, a github.com `external_ref`, then an `origin github.com/...`
  line in the description. Local paths and bare names are skipped and
  reported. `--repo` may stand in for it on a dry run only.
- **Public repositories are opt-in.** The plan prints the repository's
  visibility; a real run into a public one stops unless `--allow-public`.
- **The plan lists every item** with the reason it is included (also in
  `--json` under `project`).
- **The rest of the store is untouched,** and the recorded sync position is
  not moved by a project run (`SyncOptions.KeepSyncPosition`).

# Not changed

A pairing still ties a whole store to a repository. A project run into a
repository other than the store's pairing is a dry run only. Pulling is
unchanged and is refused for a project run.
