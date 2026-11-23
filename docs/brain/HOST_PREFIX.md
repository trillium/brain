# A store's prefix is its identity: minting and `stores verify-host`

A bead's id prefix decides which store it is in. A task bead minted as
`lifespan-…` is in the wrong store, not just oddly named.

## How the minted prefix is chosen

1. **Unified database (`brain_unified`), brain build.** The store a wrapper
   pins (`BD_NAME`) reads its own row of `brain_unified_config`, so `task`
   mints `task-…` and `robots` mints `agent-…` whatever the database-wide
   `config.issue_prefix` says. That value is the template store's seed
   (`lifespan`) and is never the answer for a pinned store.
2. **Anywhere the store's own row cannot be consulted** (a database that is not
   unified, or a shape probe that failed), a pinned store takes the
   `issue-prefix` in its `config.yaml` before the database-wide value
   (`issueops.ResolveMintPrefix`). A failed probe is no longer cached for the
   life of the process.
3. **Stock bd** mints from the database-wide `config.issue_prefix` and ignores
   `BD_NAME` and `config.yaml`. On the shared database every store therefore
   mints `lifespan-…`. A host running stock bd cannot be fixed by its wrapper;
   it needs the brain build.

The database-wide `issue_prefix` is shared state and is not changed by any of
this.

## `brain stores verify-host`

```
brain stores verify-host [--json] [--bd <path>] [--timeout 20s]
```

Exit 1, with one `FAIL` line and a fix per cause, when:

| check | fails when |
| --- | --- |
| `bd-build` | the bd the wrappers run has no `brain` key in `version --json` |
| `registry` | `~/.config/brain/stores.yaml` or `~/.config/trillium-stores/stores.yaml` is missing |
| `wrapper` | a store in the registry has no executable `~/.local/bin/<store>` |
| `mint-prefix` | a dry-run mint through the store's wrapper does not carry that store's prefix; the finding also gives the database `issue_prefix` |

The expected prefix is the store's `config.yaml` `issue-prefix`, else its
`BD_NAME`, else its registry name (a store's prefix is not always its name:
`ideas` mints `idea-…`). Backup copies of wrappers are ignored; only registered
stores are checked.

The dry-run is `<wrapper> store-prefix probe --json`: it resolves the prefix with
the create path's own code and inserts nothing, so no bead is left behind. It is
not attempted under stock bd (which has no such verb, and a real mint would
leave a bead); the `bd-build` failure already names that cause.

`--json` prints one object (`ok`, `bd`, `checks`, `stores`, `failing`) for
dotfiles drift checks; the exit code is the same.
