---
id: 0045
title: pair a store with a github repository and choose the sync direction
isc: []
status: proposed
created: 2026-11-05
updated: 2026-11-05
commits: []
touches:
  - cmd/bd/github.go
  - cmd/bd/github_pairing.go
  - cmd/bd/github_schedule.go
  - cmd/bd/sync_push_pull.go
  - cmd/bd/github_pairing_test.go
  - cmd/bd/github_schedule_test.go
  - cmd/bd/github_sync_embedded_test.go
  - internal/github/client.go
  - internal/github/mapping.go
  - internal/github/tracker.go
  - internal/github/links.go
  - internal/github/marker.go
  - internal/github/links_test.go
  - internal/github/marker_test.go
  - internal/github/tracked_store_test.go
  - internal/tracker/engine.go
  - internal/tracker/tracker.go
  - internal/tracker/types.go
  - internal/tracker/engine_metadata_test.go
  - internal/storage/dolt/slots.go
  - internal/storage/embeddeddolt/slots.go
  - docs/brain/GITHUB_SYNC.md
  - divergence/0045-github-sync-pairings.md
upstream_rebase_notes: |
  `cmd/bd/github.go`, `internal/github/*` and `internal/tracker/engine.go` are
  upstream files with brain edits; expect conflicts there and re-apply the
  intent below rather than choosing a side. Three of the edits are plain bug
  fixes upstream would want too: the nil-map panic in both `SlotSet`s, the
  `marshalTrackerMetadata` null guard, and the `externalRefChangedAfter`
  fallback. `github_pairing.go`, `github_schedule.go` and the new
  `internal/github` files are brain-only; resolve `ours`. The engine gained one
  optional interface (`LinkFinder`), one option (`PullOverwriteLocal`) and one
  stat (`Linked`); other trackers are unaffected because none implement the
  interface or set the option.
---

# Why

`brain github sync` came from upstream as one global behaviour: configure a
repository, run `sync`, get a two-way sync of the whole store. brain has dozens
of stores and the captain wants to decide per store whether GitHub is the
source, brain is the source, or both are. Running it for real also showed it
was not safe to repeat on a wrapped store (below).

# What changed

- **Per-store pairing.** `brain github pair <owner/repo> [--direction] [--conflict]`
  records the repository, direction (`two-way`, `brain-to-github`,
  `github-to-brain`), conflict policy and the store name in that store's own
  config (`github.repository|direction|conflict|namespace`). `unpair`, and
  `status` shows the pairing. No global switch. A one-way pairing refuses the
  opposite `--pull-only`/`--push-only`; `github push|pull` obey it too. A
  recorded direction that does not parse is an error, never a default.
- **One-way modes are real mirrors.** brain-to-github overwrites GitHub-side
  edits; github-to-brain overwrites brain-side edits (`PullOverwriteLocal`).
- **Links that cannot duplicate.** Pushed issue bodies end with a hidden
  `<!-- brain-sync store=<store> id=<item> -->` marker. Push asks the tracker
  (`LinkFinder`) for an existing issue carrying the item's marker before it
  creates one; a lost `external_ref` is repaired, not duplicated. Pull maps a
  marker back onto its item, skips another store's mirrors, and refuses an
  issue that repeats the marker of an item linked elsewhere. A URL for a
  different repository is no longer an `external_ref` for this pairing.
- **Writes nothing when nothing changed.** A `ContentEqual` push hook skips
  issues that already match, so repeat syncs do not PATCH every issue. Dry run
  now applies the same up-to-date test as a real push, so its counts are the
  run's counts (it used to count every linked item as an update), and it prints
  per-side tallies (`--json` too).
- **Round trip keeps what GitHub cannot say.** Any non-open/closed status gets
  a `status::<name>` label; pull keeps an item's assignee, status and type
  where the issue carries nothing different.
- **Optional schedule.** `sync --watch --interval` and a launchd job
  (`github schedule print|install|status|remove`), off by default.
- **Auth.** With no token configured, `gh auth token` is used.
- **System-internal types** (`message`, `molecule`, `gate`, `event`) and
  ephemeral items are not pushed.

# Bugs found and fixed on the way (live proof against a scratch repository)

1. A pull that created an item panicked: the engine stored the JSON literal
   `null` as the new item's metadata and both `SlotSet`s then wrote into a nil
   map. `marshalTrackerMetadata` no longer yields `null`; `SlotSet` tolerates it.
2. A pull overwrote a local edit made after the last sync. The engine's
   "re-linked since last sync" check fell back, whenever the store is wrapped
   (the markdown-mirror decorator hides `DB()`, so always on brain), to "any
   local edit counts", which switched the conflict guard off. The fallback now
   counts only items created after the last sync.
3. Dry run over-counted updates (see above).
4. The incremental pull could miss an issue created or edited just before the
   last sync (GitHub's listing lags a write). It now reads from five minutes
   before the last sync, and drops from that window any issue the store already
   tracks and that has not changed since, so the look-back cannot re-apply an
   older copy over a brain edit.

# Not changed

Comments, dependencies and deletions are not synced (the inherited GitHub
integration never did). Two stores two-way paired to one repository would each
import the other's native issues; the docs say pair one repository with one
store.
