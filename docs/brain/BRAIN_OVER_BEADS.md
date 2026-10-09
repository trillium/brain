# Brain over Beads

What brain does that upstream beads does not, and how each of the new features works. This page is the picture-first companion to [`WHAT_BRAIN_ADDS.md`](WHAT_BRAIN_ADDS.md) (the inventory, with state and evidence per feature). Each section here shows one feature as a diagram and a few lines of prose, then points at the page that holds the detail.

Brain was forked from beads at `1.1.0-rc.1`. Upstream is now at `1.3.1`. Every comparison below is against **beads 1.3.1**, and it says where upstream has started to catch up. The diagrams are written for GitHub's Mermaid renderer.

Contents: [the gap at a glance](#the-gap-at-a-glance) · [stores and the unified database](#stores-and-the-unified-database) · [one write, end to end](#one-write-end-to-end) · [guard and observer hooks](#guard-and-observer-hooks) · [durable event outbox](#durable-event-outbox) · [markdown edit-back](#markdown-edit-back-and-mark-for-deletion) · [prefix claim, release, history](#prefix-claim-release-and-history) · [conflict beads](#conflict-beads) · [shared-database backup](#shared-database-backup)

## The gap at a glance

```mermaid
flowchart LR
  subgraph UP["Upstream beads 1.3.1"]
    direction TB
    U1["Issues, dependencies, labels, ready queue"]
    U2["One database per project"]
    U3["on_create / on_update / on_close scripts<br/>run after the write, cannot refuse it"]
    U4["validation.on-create<br/>checks markdown headings only"]
    U5["Events journal<br/>records changes, delivers nothing"]
    U6["bd backup, once per project"]
  end
  subgraph BR["Brain"]
    direction TB
    B1["Many named stores, one binary"]
    B2["Unified database, one namespace per store"]
    B3["Prefix ownership with an audit trail"]
    B4["Conflict beads when merged copies differ"]
    B5["Guard hooks that can refuse a write"]
    B6["Event outbox, retried until acknowledged"]
    B7["Markdown render with opt-in edit-back"]
    B8["One backup for a shared database"]
  end
  U2 -. "extended by" .-> B1
  U2 -. "extended by" .-> B2
  U3 -. "joined for new hooks by" .-> B5
  U5 -. "delivery added by" .-> B6
  U6 -. "made shared-aware by" .-> B8
```

| Need | Upstream beads 1.3.1 | Brain |
|---|---|---|
| Refuse a bad write | No. The `on_*` scripts run after the write and cannot refuse it. `validation.on-create` can only check markdown section headings. | A guard hook sees the proposed write first and can refuse it, naming itself. |
| Tell other systems about changes | The events journal (`bd events`) records changes. Nothing sends them anywhere. | The outbox delivers each event to HTTP or command subscribers and retries until every one acknowledges. |
| Many trackers on one machine | One database per project. `bd federation` means peer sync across machines, which is a different thing. | A registry of named stores, a wrapper per store, federated search, and one unified database with namespaces. |
| Who owns an id prefix | A static `allowed_prefixes` list, which `--force` on `bd create` bypasses. | Ownership is recorded and enforced on every mint, and `--force` cannot bypass it on the unified database. Claims and releases are logged. |
| Beads as markdown files | No. | Every bead is rendered to `entries/`. Edits can flow back if the store allows it. |

Brain's own rules for the hooks, the outbox and the backup are not repeated here; each section links to the page that owns them.

## Stores and the unified database

Each store is a small wrapper command that pins `BD_NAME` and the database location. Since the switchover, the stores share one database, `brain_unified`, and each store sees only its own namespace unless asked to look wider (`--wide`). See [`WHAT_IS_BRAIN.md`](WHAT_IS_BRAIN.md) for the federation and [`MERGING_DATABASES.md`](MERGING_DATABASES.md) for how separate store databases become one.

```mermaid
flowchart TB
  REG["Store registry<br/>~/.config/brain/stores.yaml"] --> WR
  subgraph WR["Wrappers, one per store"]
    direction LR
    W1["task"]
    W2["decision"]
    W3["robots"]
    W4["... one per registered store"]
  end
  WR -->|"BD_NAME=store"| BIN["One bd binary"]
  BIN --> DB[("brain_unified")]
  subgraph DB2["Inside brain_unified"]
    direction LR
    N1["task namespace"]
    N2["decision namespace"]
    N3["robots namespace"]
    P[("brain_store_prefixes<br/>who owns which prefix")]
  end
  DB --- DB2
  BIN -->|"default: narrow read, own namespace"| N1
  BIN -.->|"--wide: every namespace"| DB2
  X["Write with no BD_NAME"] -->|"refused, never minted under a guessed store"| BIN
```

## One write, end to end

This is where the new features plug in. The store is wrapped in layers (`PersistentPreRunE` in `cmd/bd/main.go` builds them): the guard hooks sit closest to the database, the legacy `on_*` script hooks wrap them, and the markdown renderer is outermost. The outbox is not a store layer. It runs in `PersistentPostRunE`, after the command's write.

On a write, the order is:

1. Before the command runs: the `hooks.d` definitions are loaded (a malformed one refuses every command that could write), the store is opened and wrapped, and the outbox preflight checks the backlog.
2. Guards run first, before the write. The first refusal wins.
3. The write and its Dolt commit.
4. Declared observers run, in the same layer as the guards.
5. The legacy `on_create` / `on_update` / `on_close` scripts run.
6. The markdown file is rendered.
7. After the command returns: one outbox row per changed bead, then the Dolt auto-commit, auto-backup, the opportunistic outbox delivery pass, and the outbox commit.

```mermaid
sequenceDiagram
  autonumber
  participant C as bd command
  participant P as Preflight
  participant G as Guard hooks
  participant S as Store (Dolt)
  participant O as Observers
  participant L as Legacy on_* scripts
  participant R as Markdown render
  participant X as Event outbox
  C->>P: load hooks.d, open store, check outbox backlog
  alt hook definitions unusable, or backlog at its bound
    P-->>C: refuse before anything is written
  else ready
    C->>G: proposed write as JSON on stdin
    alt guard refuses, fails, or times out
      G-->>C: write refused, hook named in the error
    else every guard passes
      G->>S: write and commit
      S->>O: declared observers run
      O-->>S: a failure becomes a warning record
      S->>L: legacy scripts run
      L->>R: render entries/kind/slug.md
      R-->>C: command returns
      C->>X: PostRun: one event row per changed bead
      X->>X: auto-commit, backup, then deliver within the time budget
    end
  end
```

## Guard and observer hooks

`bd hook list` · `bd hook warnings` · `bd hook test`

Each hook is one TOML file in `.beads/hooks.d/` with a `run` command, a `when` event, and a declared `policy`. The policy decides what a failure means. Upstream's `.beads/hooks/on_*` scripts still work alongside and still cannot refuse. Full reference: [`HOOKS.md`](HOOKS.md).

```mermaid
flowchart TB
  D["Hook file: .beads/hooks.d/name.toml"] --> POL{"Declared policy"}
  POL -->|guard| GB["Runs before the write"]
  GB --> GE{"Exit status"}
  GE -->|"0"| OK["Write goes ahead"]
  GE -->|"refusal_exit, default 2"| RF["Refused. The hook's stderr is the reason"]
  GE -->|"other exit, timeout, cannot start"| FC["Refused. A guard that cannot decide fails closed"]
  POL -->|observer| OA["Runs after the write commits"]
  OA --> OE{"Exit status"}
  OE -->|"0"| SIL["Nothing recorded"]
  OE -->|"anything else"| W1["Write stays. Warning recorded"]
  POL -->|"not set"| UA["Runs like an observer"]
  UA --> W2["Warning on every run: no failure policy declared"]
  W1 --> WR[("Warning record in the store<br/>config key hookwarning.id")]
  W2 --> WR
  WR --> ACK["bd hook warnings --ack id"]
  BAD["Malformed or ambiguous hook file"] -->|"every write refused until fixed"| POL
```

### A hook can observe and refuse, but never write

A hook that runs `bd create` itself is stopped by three independent layers, each of which holds if the one before it is defeated.

```mermaid
flowchart LR
  H["Hook process tries bd create"] --> L1{"Has database coordinates?"}
  L1 -->|"no: BEADS_*, BRAIN_*, BD_*, DOLT_*, MYSQL_* stripped, empty working dir"| NO1["No database found"]
  L1 -->|"hook supplies BEADS_DIR"| L2{"BD_INSIDE_HOOK set?"}
  L2 -->|yes| NO2["Refused: process started by a hook"]
  L2 -->|"scrubbed with env -u"| L3{"Parent bd holds hooks-running.pid.lock?"}
  L3 -->|"yes, found in process ancestry"| NO3["Refused: process started by a hook"]
```

### Where shaped-data checks belong

A required shape for a bead is a guard hook. This example refuses a `decision` bead that does not carry an options list, and lets everything else through. It was tested on a scratch store (a fresh `bd init` under its own `HOME`, a binary built with `CGO_ENABLED=1 go build -tags gms_pure_go`), with `bd hook test` and two real `bd create` calls.

```toml
# .beads/hooks.d/decision-shape.toml
policy = "guard"
when   = "create"
run    = '''
jq -e '.issue.issue_type != "decision"
       or ((.issue.description // "") | test("(^|\n)Options:\n- "))' >/dev/null \
  || { printf "%s\n" "decision beads need a description with an 'Options:' line followed by a '- ' list" >&2; exit 2; }
'''
```

The hook payload carries `id`, `title`, `description`, `status`, `priority`, `issue_type`, `assignee`, `labels` and `created_by`. It does **not** carry `metadata`, so a guard cannot check metadata keys. A first version of this example that tested `.issue.metadata.options` refused every `decision` bead, including one created with valid `--metadata`, because the field is absent from the payload. The example therefore checks the description, which the payload does carry. A check that must read metadata has no hook today.

With `jq` on `PATH`, a decision whose description has no `Options:` list is refused, and one with the list passes:

```
$ bd hook test decision-shape --payload-file p-bad.json     # description: "we need a database"
exit:    2
stderr:  decision beads need a description with an 'Options:' line followed by a '- ' list
verdict: REFUSE: bead not created: refused by guard hook "decision-shape" (…/.beads/hooks.d/decision-shape.toml): …

$ bd hook test decision-shape --payload-file p-good.json    # description ends "Options:\n- dolt\n- sqlite"
exit:    0
verdict: pass

$ bd create "pick a db" -t decision -d "we need a database"
Error: bead not created: refused by guard hook "decision-shape" (…): decision beads need a description with an 'Options:' line followed by a '- ' list      # exit 1

$ bd create "pick a db" -t decision -d $'we need a database\nOptions:\n- dolt\n- sqlite'
✓ Created issue: scratch-g19 — pick a db                                                                          # exit 0
```

> **Limits.** Import, `bd dolt pull` merges, compaction and delete-by-source-repo bypass the guarded store methods, and proxied-server mode refuses writes while hooks are declared. A status change through `bd update --status closed` is an `update` event, not a `close` event. A refusal is not recorded as a warning. It shows up only as the command's error.

## Durable event outbox

`bd outbox list` · `deliver` · `ack` · `record` · `purge`

The outbox is opt-in with `change-events.outbox.enabled`. Every write adds one row per changed bead to the store's own `event_outbox` table. The row is versioned in Dolt and freezes the list of subscribers at that moment. Delivery is at least once, and subscribers drop duplicates on (`store`, `seq`). Full reference: [`event-outbox.md`](event-outbox.md).

```mermaid
stateDiagram-v2
  [*] --> Pending : write command ends, event row added
  Pending --> Pending : attempt fails, retry after 1s, 2s, 4s ... capped at 1h
  Pending --> PartlyAcked : some frozen subscribers acknowledged
  PartlyAcked --> PartlyAcked : another subscriber acknowledges
  PartlyAcked --> Retired : last frozen subscriber acknowledged
  Pending --> Retired : all acknowledged at once
  Retired --> [*] : bd outbox purge --before RFC3339 time
  note right of Pending
    Acknowledged means HTTP 2xx or exit 0 within the timeout,
    or an explicit bd outbox ack.
    Time and attempt count never retire an event.
  end note
```

```mermaid
flowchart LR
  W["Next write command"] --> Q{"Pending events would exceed max-pending?<br/>default 10000"}
  Q -->|no| GO["Write proceeds, event added"]
  Q -->|yes| STOP["Refused before it writes, with what to do"]
  STOP --> FIX["bd outbox deliver --all, or ack"]
  FIX --> W
```

The check is `pending + 1 > max-pending`, made in the preflight before the command mutates anything. The `outbox` verbs, `config`, `vc`, `dolt`, `doctor`, `version`, `help` and the read-only commands are exempt, so the operator can always get out.

> **Limits.** The event is inserted in a second transaction just after the write (in the command's post-run), so a process killed in that window leaves a committed write with no event. `bd outbox record <issue-id> --command <name>` repairs that by hand. Closing the window needs the insert inside the store's mutation transaction, which is left as a follow-up. There is no delivery daemon: retries happen after writes or when someone runs `bd outbox deliver`. Server-mode stores only; an embedded store refuses writes while the outbox is enabled.

## Markdown edit-back and mark-for-deletion

`bd render-import` · `bd render-marks` · `bd stores edit-back`

Brain renders every bead to `entries/kind/slug.md`. Edit-back lets a store accept edits made to those files. It is off by default, per store, and a deleted file never deletes a bead. Full reference: [`EDIT_BACK.md`](EDIT_BACK.md).

The checks run in this order, for every store. Whether the store declared `edit_back` is decided last: a store that did not declare it still gets every refusal below, and a file that would have been applied is reported as ignored instead.

```mermaid
flowchart TB
  E["Edited markdown file"] --> RI["bd render-import, one store at a time, under that store's BD_NAME"]
  RI --> Q0{"File readable, frontmatter parses, has an id?"}
  Q0 -->|no| R0["Refused: cannot-read, cannot-parse or no-name"]
  Q0 -->|yes| Q2{"File's id is in this store's namespace?"}
  Q2 -->|no| R1["Refused: outside-namespace"]
  Q2 -->|yes| Q2b{"Bead exists in this store?"}
  Q2b -->|no| R1b["Refused: unknown-bead"]
  Q2b -->|yes| Q2c{"Slug and manifest path match the bead?"}
  Q2c -->|no| R1c["Refused: slug-mismatch or not-manifested-path"]
  Q2c -->|yes| Q2d{"Bead marked for deletion?"}
  Q2d -->|yes| R1d["Refused: edited-while-marked"]
  Q2d -->|no| Q3{"File older than the bead's last update?"}
  Q3 -->|yes| R2["Refused: stale-file. The database wins"]
  Q3 -->|no| Q4{"Any change to the five fields, and is the file unambiguous?"}
  Q4 -->|"ambiguous or unreadable"| R4["Refused: ambiguous-labels, ambiguous-body, priority-unreadable"]
  Q4 -->|"no change"| CL["Clean, nothing to do"]
  Q4 -->|change| Q1{"Store declares edit_back?"}
  Q1 -->|no| IG["Reported as ignored. Bead unchanged"]
  Q1 -->|yes| AP["Applied to title, status, priority, labels, description<br/>old and new values printed, history kept in Dolt"]
```

```mermaid
stateDiagram-v2
  [*] --> Rendered : bead written, file recorded in the render manifest
  Rendered --> Marked : file deleted, then render-import runs
  Marked --> Marked : renders skip it, edits refused
  Marked --> Rendered : bd render-marks clear id, file re-created
  note right of Marked
    Label marked-for-deletion.
    The bead is never removed.
  end note
```

The deletion scan reads the render manifest (`entries/.render-manifest.json`) and runs on every `render-import`, whatever the store's edit-back setting. Without a manifest, deletions are not scanned and the run says so.

> **Limits.** Nothing schedules `render-import`; it is an explicit run. A moved file reads as a deletion of the old path plus a refusal of the new one. The stale check compares to the second with 2 seconds of slack, so a real edit within 2 seconds of a render is not detected as stale.

## Prefix claim, release, and history

`bd store-prefix add` · `release` · `history`

On the unified database, every id prefix has one recorded owner, checked on every mint, and `--force` cannot bypass it. Every claim and release writes an event in the same transaction, to the append-only `brain_store_prefix_events` table. Full reference: [`PREFIX_RELEASE.md`](PREFIX_RELEASE.md).

```mermaid
flowchart TB
  S["bd store-prefix release p --confirm"] --> C0{"Run under a store wrapper (BD_NAME) and --confirm given?"}
  C0 -->|no| X0["Refused: no namespace to release as, or no --confirm"]
  C0 -->|yes| C1{"Valid prefix shape?"}
  C1 -->|no| X1["Refused"]
  C1 -->|yes| C2{"Prefix recorded?"}
  C2 -->|no| X2["Refused: nothing to release"]
  C2 -->|yes| C3{"Caller's store is the owner?"}
  C3 -->|no| X3["Refused: names owner and caller"]
  C3 -->|yes| C4{"Ownership decided at build time?"}
  C4 -->|yes| X4["Refused: build-decided"]
  C4 -->|no| C5{"Any live bead under the prefix?"}
  C5 -->|yes| X5["Refused, with no override flag"]
  C5 -->|no| OK["One transaction: append release event, then remove ownership"]
  OK --> H["bd store-prefix history p<br/>shows every claim and release"]
```

> **Limits.** The trail starts at adoption: a claim made before the event table existed has no event row, and `history` says so instead of inventing one. A transfer is two acts (release, then claim), and the next claim wins the gap between them.

## Conflict beads

When `bd brain unify` merged the separate store databases, 31 ids existed in more than one store with different content. Brain keeps every copy instead of picking a winner, and leaves an open conflict bead at the original id. The 92 identical duplicates (of 123 duplicated ids) were merged into one bead each. This is the real example from the merge. Full reference: [`MERGING_DATABASES.md`](MERGING_DATABASES.md#duplicated-ids-identical-copies-merge-differing-copies-become-conflict-beads).

```mermaid
flowchart LR
  A["Store brain<br/>agent-0bq, open"] --> U["bd brain unify"]
  B["Store robots<br/>agent-0bq, closed"] --> U
  U --> C["agent-0bq<br/>open conflict bead<br/>label unify-conflict"]
  U --> M1["brain-3cab06c61505<br/>brain's copy, with its comments and links"]
  U --> M2["agent-9b081b3a31bd<br/>robots' copy, with its comments and links"]
  C -.->|"tracks, blocks nothing"| M1
  C -.->|"tracks, blocks nothing"| M2
  O["Other beads that linked to agent-0bq"] --> C
```

The new ids are the authoring store's prefix plus the first 12 hex characters of `sha256(original id, 0x00, store)`, so a rebuild or replay produces the same ids.

> **Limits.** There is no command yet to resolve a conflict bead (fold the copies, close one, close the conflict). A `blocks` link to a conflicted id is kept, so it now waits on the open conflict: in the real data, 8 inbound `blocks` rows (4 in the brain store, 4 in the task store) are in that position.

## Shared-database backup

Upstream auto-backup runs once per project. On a database shared by many stores, that meant one private copy of the same data per store. Brain recognises the shared database and backs it up once, to the `default` destination registered with `bd backup init <path>`. See the Auto-Backup section of [`../CONFIG.md`](../CONFIG.md#auto-backup).

The checks run in this order. The throttle and the change check are made twice, once before and once after taking the lock, because the store that held the lock may have just finished a backup.

```mermaid
flowchart TB
  W["Any store's command triggers auto-backup<br/>backup.enabled is true"] --> Q1{"Database has brain_unified_config?"}
  Q1 -->|"no"| UPS["Per-store backup, exactly as upstream"]
  Q1 -->|"cannot tell"| REF["Refused, no backup taken"]
  Q1 -->|"yes, shared"| QT{"Last backup within backup.interval?<br/>default 15m"}
  QT -->|yes| THR["Throttled, nothing to do"]
  QT -->|no| Q4{"A default backup destination registered?"}
  Q4 -->|no| REF
  Q4 -->|yes| Q3{"Database changed since the last backup?"}
  Q3 -->|no| UNCH["Unchanged, nothing to do"]
  Q3 -->|yes| Q2{"Named lock bd-shared-backup:database free?"}
  Q2 -->|"held by another store"| SKIP["Reports in-progress, moves on"]
  Q2 -->|"taken"| RECHK["Read state, destination and HEAD again,<br/>stop if another store just backed up"]
  RECHK --> SYNC["Sync to the default destination"]
  SYNC --> ST[("brain_shared_backup_state<br/>last commit, time, destination")]
```

> **Limits.** Every refusal prints a warning that no backup was taken. It never falls back to a per-store copy.

## Sources

Upstream behaviour was checked against an installed `bd 1.3.1`. Brain behaviour was checked against the code on this branch: `cmd/bd/main.go` (`PersistentPreRunE`, `PersistentPostRunE`), `internal/storage/hook_guard_decorator.go`, `internal/hooksdef/`, `cmd/bd/outbox*.go`, `internal/brain/editback/editback.go`, `internal/storage/issueops/unified_namespaces.go`, `internal/brainunify/conflict.go` and `internal/storage/versioncontrolops/shared_backup.go`.
