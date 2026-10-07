# What brain adds over upstream beads

This is the **differential feature inventory**: one scannable list of the features brain supports that upstream beads does not, with state and evidence per feature. It complements [`WHAT_IS_BRAIN.md`](WHAT_IS_BRAIN.md) (the plain-English primer) and [`../../VISION.md`](../../VISION.md) (the approved vision). The retired v0.3 project ISA it once cited as "the spec" lives on as a frozen historical record at [`archive/ISA-v03.md`](archive/ISA-v03.md).

The framing rule — recorded in `divergence/0005` as the reason the earlier `BRAIN_VS_BD.md` was withdrawn and reframed in `divergence/0006` — is that **brain IS bd, renamed**: a Go fork that keeps beads' versioned Dolt substrate and graph-shaped issue model and layers a federation model on top. So this document is *not* a verb-vocabulary mapping and *not* an essay. It is an inventory: one feature per entry, grouped by area, state visible at a glance.

## The pinned upstream reference

Every "upstream does not have this" claim below is against this ref, after checking:

- **Repository:** `https://github.com/gastownhall/beads`
- **Tag:** `v1.1.0-rc.1` → commit `fa4dce4548d8d15d5478b9ad7e4f6ee7cbfabaa1` ("chore: re-land v1.1.0-rc.1 release prep (#4486)")
- **Why:** brain's `README.md` records the upstream base as `1.1.0-rc.1`; `cmd/bd/version.go:19` pins `Version = "1.1.0-rc.1"` at brain head; the upstream tag resolves to exactly that commit.
- **Cross-check:** upstream has advanced past the pin (latest observed upstream main, `a4509deb2`). Absences were re-checked there where noted — no listed brain feature gained an upstream equivalent at that later ref, `bd patch` being the one near-miss (§7, G-36).

The verb-level difference was established mechanically: both binaries were built (`go build -tags gms_pure_go`) — the fork at brain head `15351ba8258d25a1ab4763d1116df23296e7ed8a`, upstream at the pinned ref — and their top-level help surfaces diffed. Result (raw captures in [`divergence/0020-brain-feature-inventory.md`](../../divergence/0020-brain-feature-inventory.md)'s evidence):

- **Verbs upstream `v1.1.0-rc.1` lacks entirely (14):** `brain`, `isa-by-slug`, `isa-list`, `isa-render`, `isa-render-all`, `isa-render-pending`, `isa-section`, `isa-show`, `patch`, `render`, `render-all`, `store-prefix`, `stores`, `transfer`.
- **Persistent flag upstream lacks (1):** `--wide`.
- Upstream has *no* command upstream-only at this ref: the fork is a strict superset at the verb level.

Everything past verbs (federation model, unified database, identifiers, exfiltration, memory extensions) was established by reading the trees (`git show fa4dce454:<path>`, the brain tree at head) and the evidence named per entry.

**State legend:**

- **shipped** — in the binary and in daily use in the operator's federation.
- **built + proven** — in the binary with behavioral proof on real data, but not deployed/cut over (the unification tranche).
- **code** — wired in the binary; behavior visible in help/code, not part of daily operation yet.
- **deferred / unbuilt** — declared but no code.

## 1. The store federation (one binary, many named stores)

The trap this group avoids: upstream *has* a verb named `federation` (`bd federation sync/status/add-peer/…`, `cmd/bd/federation.go` at the pinned ref) — cross-machine peer sync. Brain's store federation is a *different animal*: many named stores on one machine, one registry, one search. Same word, unrelated mechanism.

| # | Feature | What it does | Where | State | Evidence | Upstream at `v1.1.0-rc.1` |
|---|---------|--------------|-------|-------|----------|---------------------------|
| A-1 | **One binary, many stores** | Each store is a thin shell wrapper (`~/.local/bin/<store>`) pinning `BEADS_DIR`, `BD_NAME`, `BRAIN_KNOWLEDGE_ROOT`; `BD_NAME` drives argv-equivalent dispatch, display name, and (post-cutover) namespace resolution | `README.md` "How it ships"; `cmd/bd/main.go` (`BD_NAME` overrides help name; `BRAIN_STORE_*` env) | shipped | README wrapper shape; `main.go:1550-1553` | not present — upstream bd is one directory, one `.beads` |
| A-2 | **Store registry** | `~/.config/brain/stores.yaml` (legacy `~/.config/pai/stores.yaml` kept as compat symlink) drives both the binary and wrapper generation; regenerated `~/.config/brain/stores.env` exports `BRAIN_STORE_*` / `BRAIN_STORES_LIST` (+ `PAI_*` aliases) | `cmd/bd/brain_stores.go`; `docs/BRAIN_DEPAI_MIGRATION.md` | shipped | README "Store registry"; de-PAI migration doc | not present — no registry, no cross-database config |
| A-3 | **`stores create` — one-shot store provisioning** | `stores create <name>` does dolt init + entries dir + CLI wrapper + registry write + env regen in one **idempotent** command; `--no-wrapper` supported | `cmd/bd/brain_stores.go` (`create <name>`, line 404) + `brain_stores_create_test.go` | shipped | README (idempotence called out); tests in tree | not present — upstream has no store creation verb |
| A-4 | **`stores add/remove/list`** | Register an existing dolt directory without creating files; unregister (files untouched); list the registry | `cmd/bd/brain_stores.go` (lines 300/331/355) | shipped | Use-lines in tree | not present |
| A-5 | **`stores alias / rename / set-about / env`** | Alias an existing store under a new name; rename; attach a blurb; regenerate `stores.env` | `cmd/bd/brain_stores.go` (lines 1098/1199/1329/1372) | shipped | Use-lines in tree | not present |
| A-6 | **`stores doctor` — identity-checked store health probe** | Probes every registered store the way an agent reaches it (through its wrapper); exit 1 carries a `FAILING STORES:` line; missing-registry reports the `__registry__` sentinel; missing-wrapper/stale-path are warnings (`--strict` promotes them to failures); `--json` structured | `cmd/bd/brain_stores_doctor.go` (+ test) | shipped | README "Store registry"; source doc comments | not present — `bd doctor` checks one installation |
| A-7 | **`stores render-all` — federation-level markdown refresh** | Runs `render-all --json` under each store's own `BEADS_DIR`/`BD_NAME` in a subprocess, per-store outcomes + a federation summary line on stderr; machine-readable `--json` | `cmd/bd/brain_stores.go` (`render-all`, line 953) | shipped | source; suppression env `BRAIN_NO_AUTO_FEATURE_REQUEST` in the same call site | not present |
| A-8 | **Federated search** | `bd search --federated` walks every registered store on the same Dolt server, sections results per store (primary first, per-store caps, unknown prefixes skipped silently); on the unified database it becomes one tree walk bucketed by `brain_store_prefixes` | `cmd/bd/brain_search_federated.go` (+ tests) | shipped (legacy walk) / built + proven (unified walk) | verb help flag text; tests; namespace-model report §R1 | upstream `search` is single-database — no federation concept |
| A-9 | **`transfer` — atomic cross-store move** | Moves a brain doc from one store to another (creates in the destination, closes at the source — the inbox-promotion pattern) | `cmd/bd/brain_transfer.go` via `internal/brain/verb/transfer/`; `divergence/0008`'s family | shipped | top-level verb in help diff (fork-only list) | not present (`"transfer"` string does not occur anywhere upstream at either ref) |
| A-10 | **`brain` verb parent** | The knowledge-graph vocabulary: `brain new`, `brain link`, `brain related`, `brain recast`, `brain promote`, with `brain unify` nested beneath (§2) | `cmd/bd/brain.go`, `cmd/bd/brain_link.go`, `brain_new.go`, `brain_related.go`, `brain_recast.go`, `brain_promote.go` | shipped | `brain --help` capture; divergence trail 0004–0011 | not present |

## 2. The unified database and the namespace model (`brain unify`)

Newest tranche — merged at brain head `15351ba8258d25a1ab4763d1116df23296e7ed8a`, **behaviorally proven on scratch copies of real frozen data, not yet deployed and nothing cut over**. Evidence of record (cited by path, per the task brief):

- `/Users/mini0/fm_home/mini0-ops/data/brain-unify-probe/report.md` — collision machinery end-to-end (44/44 verify PASS, winner-flip determinism) + the stories-slice create-path proof, and the defect list that drove items 3–5.
- `/Users/mini0/fm_home/mini0-ops/data/brain-unify-verify/report.md` — post-verification work: namespace config layer, namespace render root + slug-overwrite refusal, namespace-scoped reads + one-walk federated search, the collision review decision, one backup/restore of the unified database (43/43 tables digest-identical).
- `/Users/mini0/fm_home/mini0-ops/data/brain-unify-store-create/report.md` — create-path equivalence for `stories`, `task`, `brain`, `robots` through unified wrappers; the pre-fix silent `lifespan-` mint reproduced, then removed.
- `/Users/mini0/fm_home/mini0-ops/data/brain-unify-namespace-model/report.md` — wide/narrow modes, storeless-mint refusal, runtime prefix claims, fresh-store provisioning (46 raw transcripts).

Design doc: `docs/design/brain-single-database.md`.

| # | Feature | What it does | Where | State | Evidence |
|---|---------|--------------|-------|-------|----------|
| B-1 | **`brain unify plan / build / verify`** | Consolidates every per-store Dolt database into ONE database (`brain_unified`) while preserving logical separation. `plan` prints the deterministic mapping (prefix ownership, collisions); `build` constructs it on an isolated server without touching production; `verify` compares row count, content size and order-independent digests | `cmd/bd/brain_unify.go` | built + proven | probe report (verify PASS 44/44; build+verify PASS twice incl. winner-flip); verify report (518/518 recorded fingerprints) — not present upstream at either ref (no unify verb, no `brain_unified`) |
| B-2 | **Id-collision machinery with a winner rule** | Deterministic rule chain: prefix owner holds the copy → newest `updated_at` wins → lexicographic tiebreak; content disagreement blocks the build without `--allow-collisions`; every losing row preserved in full in `brain_unify_collisions.losing_row` with `_captured_from_store` provenance; child rows of losers counted in `brain_unify_import_log` | `cmd/bd/brain_unify.go`, `internal/storage/…` | built + proven | probe report §Probe A (all three tie-break tiers exercised; 31 divergent ids reviewed column-by-column) |
| B-3 | **Collision review decision (human-in-the-loop)** | All 31 divergent collision ids reviewed; winner rule accepted, recover-nothing decision recorded; `brain_unify_collisions.losing_row` keeps every losing row recoverable | verify report + `item2-collision-review.md`; commit `c290bc6f3` | decision recorded | verify report §Item 2 |
| B-4 | **Namespace-scoped config/metadata layer** | On the unified database, `config`/`metadata`/`local_metadata` (and derived custom statuses/types) route to the re-keyed `brain_unified_*` tables filtered by the wrapper's namespace; the project-identity check compares against the store's OWN `_project_id`; no `BD_NAME` → documented degrade to the seeded plain tables | `internal/storage/issueops/unified_config.go`, commit `0dd496ca3` | built + proven, not deployed | verify report §Item 3 (205 tests ok); store-create report §4.1/§4.2 (identity refusal + correct prefix through a non-template wrapper) |
| B-5 | **Namespace-derived render root + no cross-id slug overwrite** | Markdown renders nest under the wrapper's namespace when the shared `BEADS_DIR` parent disagrees; the renderer refuses to overwrite an existing file whose frontmatter `id` belongs to a different bead | `internal/brain/exfiltrator/exfiltrator.go`, `cmd/bd/brain_config.go`, commit `716bf240b` | built + proven, not deployed | verify report §Item 4; store-create report §4.5 (derived-root nesting proven live) |
| B-6 | **Namespace-scoped read paths** | `list`/`search`/`ready`/`blocked`/`stale`/`count`/`statistics`/`render-all` (`IterIssues`) all scope to the wrapper's own namespace(s) — the probe's "my store shows the whole federation" leak fixed; exact-id lookups untouched; legacy databases unchanged | commit `2fa23685d` | built + proven, not deployed | namespace-model report R1 / R4 (exact narrow/wide numbers per store); store-create report §4.3–4.4; probe report Question (c) (pre-fix leak) |
| B-7 | **Deliberate wide and narrow read modes (`--wide`)** | New persistent `--wide` flag; narrow (default) = own namespace, wide = unscoped all-stores read including unattributed prefixes. The flag, not a config, so neither mode substitutes silently; inert on legacy stores. `--federated` is the precedent of the same wide read, bucketed per store | `cmd/bd/main.go` (`--wide`), `internal/storage/dolt/unified_namespace.go`, commit `831ea8612` | built + proven, not deployed | namespace-model report R1 (5727 vs 22541 counts; unit test `TestScopeNamespacesWideThenNarrowModeIsExplicitPerProcess`); fork-only flag in the help diff |
| B-8 | **No storeless mint (refusal, not silent borrowing)** | On `brain_unified` without a `BD_NAME`, every minting path (`create`, `q`, import, bulk, compaction) refuses with the reason and the way out instead of silently writing under whichever store won "most beads" (the pre-fix defect minted `lifespan-…` ids with exit 0) | `internal/storage/issueops/unified_namespaces.go` (`RefuseStorelessMint`, `ErrStorelessNamespace`), `internal/storage/domain/db/config.go`, commit `831ea8612` | built + proven, not deployed | namespace-model report R2 (both refusals + base-binary template-mint contrast); store-create report §4.2 (mechanism named) |
| B-9 | **Namespace ownership enforced on every mint** | `ValidateNamespaceOwnership` runs on ids — minted, explicit (`--id`), `--prefix`-overridden, or hierarchical — against the namespace's own `brain_store_prefixes` record; `--force` cannot bypass it on the unified database | same files as B-8 | built + proven, not deployed | namespace-model report R2 (`brain-fake01 --force` refusal transcript) |
| B-10 | **Runtime prefix claims (`bd store-prefix add/list`)** | A prefix can be claimed at run time for a store: recorded in `brain_store_prefixes` with `owner_reason` (`operator-added` / `store-created` / build-time `declared-by-store-config`), usable to mint the moment it lands (`--prefix` on `create`/`q`). Conflict rule: the record is never rewritten — first claimant wins, rivals refused by name; hyphenated prefixes refused by shape. No release verb by design this round | `cmd/bd/store_prefix.go` (+ test), `internal/storage/issueops/unified_namespaces.go`, commit `831ea8612` | built + proven, not deployed | namespace-model report R3 (claim → mint → visibility → conflict transcripts) |
| B-11 | **`stores create` provisions a namespace, not a database** | On a server hosting the unified shape, `stores create boop` seeds the namespace in one transaction (scoped `issue_prefix`, own `_project_id`, `brain_stores` row, name-prefix claim `store-created`) and writes a wrapper pinning `dolt_database: brain_unified`; legacy servers keep the pre-cutover flow; probe failure fails loudly | `cmd/bd/brain_stores.go` (`provisionServerStore`), commit `15351ba82` | built + proven, not deployed | namespace-model report R4 (`boop` end-to-end: empty list `[]`, mint, ready, counts, cross-namespace refusal) |
| B-12 | **Build durability: the initial dolt commit actually commits** | Dolt 2.x turned `dolt_commit` into a procedure; the build's history-initializing commit had been failing silently leaving zero dolt history until the first write | commit `3d5ae9cc7` | built + proven (fix) | verify report (probe carry-forward fixed in code) |
| B-13 | **Backup/restore of the unified database** | Dolt-native backup + restore of `brain_unified` re-verified 43/43 tables identical by whole-row digests; restore demands precedence: pre-initialized target + `--force` | verify report §Item 6 | proven (one-shot), not recurrence-automated | verify report (270 MB backup; restore spot checks) |

**Honest residuals of this tranche** (recorded so the inventory doesn't oversell it): no build/verify scope flag (per-store staging requires a single-database source server); the verifier compares against the build's recorded fingerprints, so build+verify must run on frozen sources; `brain_unify_collisions` preserves only `issues` losers (child rows as counted logs); `doctor`'s global checks are whole-database; one read-scope quirk (`agent-identity-29u` visible via the robots wrapper from `allowed_prefixes`); proxied-server clients address only the `issue_prefix` namespace gate. None has a code fix at head; all are recorded in the reports above.

## 3. Brain verbs and the ISA substrate

| # | Feature | What it does | Where | State | Evidence | Upstream at `v1.1.0-rc.1` |
|---|---------|--------------|-------|-------|----------|---------------------------|
| C-1 | **`brain new <kind> <title>`** | Create a brain doc of `kind = task | knowledge | both | isa`; kind rides the existing `issues.issue_type` with no schema migration; `kind=isa` allocates `<prefix>-isa-XXXXX` ids; `--slug` validated, collisions exit 2 | `cmd/bd/brain_new.go` (divergence/0007, ISC-104–106) | shipped | tests `brain_new_isa_embedded_test.go`, `brain_test.go` | not present — no kind discriminator, no isa ids (upstream Tests types carry none of these values) |
| C-2 | **`brain link` — typed graph edges incl. two new edge types** | `brain link <from> <to> --type=…` with `extends` and `learned-from` added to bd's edge-type set (which already had relates-to/supersedes/discovered-from) | `cmd/bd/brain_link.go`, `internal/brain/verb/link/`; `internal/types/types.go` (divergence/0008, ISC-109; first-tranche 0001/0002) | shipped | verb tests; help capture | upstream has `bd link` (dependency edges) but not the `extends`/`learned-from` types |
| C-3 | **`brain related` — BFS subgraph walk** | Walks edges from a center doc, ordered by edge type, depth-capped (default 2) with cycle detection | `cmd/bd/brain_related.go`, `internal/brain/verb/related/` (divergence/0009, ISC-111) | shipped | `verb_test.go`; help capture | upstream `bd dep`/`bd graph` are dependency-shaped, not a typed BFS walk over mixed kinds |
| C-4 | **`brain recast` — kind shift in place** | `brain recast <id> --to=<kind>`: same id, edges and comments preserved; status defaults to open on knowledge→task/both transitions; idempotent | `cmd/bd/brain_recast.go`, `internal/brain/verb/recast/` (divergence/0010, ISC-151) | shipped | tests; help capture | not present |
| C-5 | **`brain promote` — misdirect-guard, not a duplicate** | Prints a redirect hint (`brain recast` vs `bd promote`) and exits non-zero; deliberately does NOT reimplement promote | `cmd/bd/brain_promote.go` (divergence/0010, ISC-152) | shipped | UX-only affordance | upstream `bd promote` (wisp → bead) EXISTS — brain adds only the vocabulary redirect on top |
| C-6 | **The ISA verb family** | `isa-list`, `isa-show`, `isa-by-slug`, `isa-section <id> <section>` (per-section UPSERT over the twelve canonical sections), `isa-render` (canonical IsaFormat v2.7 markdown to disk), `isa-render-all`, `isa-render-pending` (stale-markdown detection) | `cmd/bd/isa_*.go`; `internal/brain/verb/{isarender,isasection,isashow}/`; schema migrations `0054_add_isa_columns`, `0055_create_isa_sections`, `0056_add_slug_unique` | shipped | verbs in fork-only help list; migration files in tree; retired ISA §ISA primitives (`docs/brain/archive/ISA-v03.md`) | not present — no ISA concept, no isa columns/sections tables, no slug unique index upstream |
| C-7 | **`bd patch <id>` — non-interactive single-field patch** | Patch a field on an issue without an editor | `cmd/bd/patch.go` (fork PR #3) | shipped | fork-only in help diff | not present at the pinned ref; present at later upstream `a4509deb2` — the one near-miss; brain shipped it first and independently (self-merged PR #3) |
| C-8 | **Ranked multi-token search** | Search tokenizes multi-word queries, searches descriptions and bodies, ranks results (title weights above body) | `cmd/bd/search_rank.go` (fork PR #6, commit `826a06e3e`) | shipped | tests in tree | upstream `search` at the pinned ref is LIKE-on-title; no tokenize/rank at either ref |
| C-9 | **Open defects in the verb surface (recorded, not hidden)** | `bd create` has no `--slug` flag (divergence/0015, open); `isa-by-slug --json` returns text (0016, open); `isa-show --json` shape mismatch (0014, open) | `divergence/0014-0016` | open defects | divergence docs | n/a — fork-local deltas |

## 4. Voice identifiers and slugs

| # | Feature | What it does | Where | State | Evidence | Upstream at `v1.1.0-rc.1` |
|---|---------|--------------|-------|-------|----------|---------------------------|
| D-1 | **Voice identifiers (`name` field)** | Every issue-bearing JSON payload carries a derived additive kebab-case `name` alongside the canonical `id`; pure function of the title (64-char cap, collision-sharing documented); the id stays authoritative | fork PR #24 (commit `cbc98e049`); `docs/brain/VOICE_IDENTIFIERS.md` | shipped | `version` lineage shows the commit on this head; docs | not present upstream at either ref (no kebab/voice-name in `internal/types`) |
| D-2 | **`brain_slug` on every create** | Every minted issue carries `metadata.brain_slug`; slug is unique per database (migration `0056_add_slug_unique`); renders can key on it (with the B-5 overwrite refusal) | exfiltrator + migrations | shipped | store-create report (rows field-for-field identical `brain_slug` slots); migration files | not present upstream (`"brain_slug"` string zero hits at either ref) |

## 5. The markdown exfiltration bridge

| # | Feature | What it does | Where | State | Evidence | Upstream at `v1.1.0-rc.1` |
|---|---------|--------------|-------|-------|----------|---------------------------|
| E-1 | **Markdown render on every mutation, every kind** | `BrainExfiltrationDecorator` stacked on `HookFiringStore` renders `entries/{kind}/{slug}.md` (or flat `entries/<slug>.md` under `BRAIN_EXFIL_FLAT=1`) on every write — not just the brain trio but every issue type bd recognizes; write-ahead checkpoint at `entries/.checkpoint.json` before render, cleared after | `internal/brain/exfiltrator/exfiltrator.go(+_test,_bench)`; `cmd/bd/brain_config.go`; divergence/0012 (ISC-117–121) | shipped | benchmark 10.4 ms/op (ISC-120); tests | not present — no markdown render path anywhere upstream (`BRAIN_EXFIL`, `brain_slug`, `IsaFormat` all zero hits) |
| E-2 | **Store-derived exfil root** | Resolution `BRAIN_KNOWLEDGE_ROOT` → `dirname($BEADS_DIR)/entries` → `~/data/brain/entries`, so each store's markdown lands next to its own `.beads` automatically; namespace nesting added post-unification (B-5) | `cmd/bd/brain_config.go` (`brainKnowledgeRoot`) | shipped (chain) / built+proven (nesting) | store-create report §4.5 derivation proof | not present |
| E-3 | **On-demand re-render: `bd render <id>`, `bd render-all`** | Re-emit markdown from the substrate after corruption or root change; `render-all` prints `Exfiltrated N / M beads…`, supports `--json` | `cmd/bd/render.go` | shipped | fork-only verbs in help diff | not present |
| E-4 | **Federation-level re-render (`stores render-all`)** | Same, run across every registered store with per-store outcomes (A-7) | `cmd/bd/brain_stores.go` | shipped | source | not present |

## 6. Memory and kv (upstream features brain extended)

These exist upstream at the pinned ref (`cmd/bd/memory.go`, `cmd/bd/kv.go`) — listed because brain's delta over upstream is the feature, and the inventory is only useful if inherited-vs-added is separated.

| # | Feature | What it does | Where | State | Evidence | Upstream at `v1.1.0-rc.1` |
|---|---------|--------------|-------|-------|----------|---------------------------|
| F-1 | **`bd remember` mints a companion knowledge bead** | A remembered insight gets a durable bead (P3, out of ready's face) indexed from the memory key to the bead id; `--no-bead` opts out; hints read the command name the user actually typed (`memoryToolName()` under `BD_NAME` dispatch) | `cmd/bd/memory.go` (extends upstream file) | shipped | diff vs upstream rc.1 in tree; store-create evidence of beads flowing | upstream `remember` stores the memory only — no companion bead, no index |
| F-2 | **kv namespace reservation** | `membead.*` keys are reserved for the memory→bead index; user kv writes forging that namespace are refused | `cmd/bd/kv.go` + `kvkeys.MemoryBeadPrefix` | shipped | diff vs upstream rc.1 in tree | not present (upstream kv has no reserved memory-bead namespace) |

## 7. Tooling and packaging

| # | Feature | What it does | Where | State | Evidence | Upstream at `v1.1.0-rc.1` |
|---|---------|--------------|-------|-------|----------|---------------------------|
| G-1 | **Double semver versioning** | `bd version 1.1.0-rc.1+brain.0.5.0 (sha: branch@sha)`; `--combined` prints only the token; `--json` exposes `brain` / `combined` fields; releases cut with `make brain-release BUMP=…` | `cmd/bd/version.go`, `version_test.go`, Makefile | shipped | binary prints `1.1.0-rc.1+brain.0.5.0 (dev)` in capture; upstream prints `1.1.0-rc.1 (dev: fa4dce4548d8)` | upstream lacks the `+brain.*` build-metadata track entirely |
| G-2 | **`change-events.jsonl` emission + multi-id reporting** | Opt-in (`change-events.enabled`): one append-only JSON line per mutated issue, so multi-issue commands report each touched id rather than last-touched only | `cmd/bd/change_events.go` (fork PR #3) | code (opt-in, off by default) | source + PR #3 commit | not present |
| G-3 | **De-PAI migration compatibility** | Registry path `~/.config/brain` with legacy `~/.config/pai` compat symlink; `BRAIN_STORE_*` env with `PAI_*` aliases; ISA exfil default flip | `docs/BRAIN_DEPAI_MIGRATION.md` | shipped | doc + README registry section | n/a — upstream has no PAI layer to strip |
| G-4 | **Auto-filed feature requests (declared, not in this tree)** | README claims unknown flags on `brain` verbs file a feature request automatically; at head `15351ba82` the only trace is the suppression env `BRAIN_NO_AUTO_FEATURE_REQUEST=1` set around subprocess walks (`brain_stores.go:1024`, `brain_stores_doctor.go:239`) — the filing mechanism itself is elsewhere or unlanded | `cmd/bd/brain_stores*.go` (env), README claim | **declared/unverified in-tree** | grep: only the opt-out env exists; no filing code found | not present |

## 8. Declared but not built (so the list stays honest)

From the retired ISA (`archive/ISA-v03.md`) and the docs — features named as brain's own, with no code at head:

- **FTS5 search index + `brain reindex`** (ISC-126–129) — never built; no `internal/storage/fts/` package. Brain search today is the ranked SQL search (C-8), in-Dolt.
- **The reconciler** `brain reconcile [--check]` — idempotent markdown rebuild, orphan removal, drift check (ISC-122–125) — unbuilt; `render-all` is the partial answer in daily use today.
- **Pulse `/brain/*` read-only module** (ISC-130–136) — deferred to v0.3.1 by First-Tranche Decision #3 (Go-only constraint).
- **v0.2 legacy namespace + `migrate-v02`** (ISC-137–141) — unbuilt.
- **Kind-sharded verb guards** (`state`/`depends-on`/`file` refusing knowledge kinds, ISC-112–116) — unchecked in the ISA; no separate guard commands exist in the binary.
- **Prefix release/transfer verb** — deliberately absent this round ("the record is never rewritten silently"; namespace-model report R3).
- **Unified-database cutover itself** — proven on copies only; production still runs the per-store federation. No store has been re-pointed.

## 9. What could not be determined

- Whether upstream beyond `a4509deb2` (latest observed) plans nd of the features above — inventory checked the two observed upstream refs, nothing further. New upstream landings may close specific rows (as `bd patch` almost did); re-verify against the then-current upstream before citing absences after this date.
- Whether the auto-filed feature-request mechanism (G-4) landed somewhere outside the fork's `cmd/bd` tree; only its suppression env var is reachable in-tree.
- Upstream's exact intent for `bd federation` (peer sync) vs brain's store federation: mechanically different at both refs (§1), but treat any upstream renaming here as a vocabulary watchpoint when rebasing.

## Artifact index

- Help-surface captures and verb lists: `/Users/mini0/fm_home/mini0-ops/data/brain-feature-inventory/help-diff/` (`fork-help.txt`, `upstream-help.txt`, `fork-only-verbs.txt`, `upstream-only-verbs.txt`, sub-command captures).
- Unification evidence of record: the four report paths in §2.
- Divergence trail: `../../divergence/` — this inventory's delivery entry is [`../divergence/0020-brain-feature-inventory.md`](../../divergence/0020-brain-feature-inventory.md).
