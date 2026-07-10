# OKF v0.1 + Obsidian Vault Verification

How to verify that a brain store's exfiltrated `entries/` directory conforms
to the Open Knowledge Format (OKF) v0.1 and opens cleanly as an Obsidian
vault.

There are two tiers of check:

1. **Headless `make okf-check`** — the primary, CI-safe gate. Pure
   file-based Go, no desktop app. Run this in CI and locally.
2. **Official `obsidian` CLI** — a richer, interactive secondary check. It
   is a **client to a running Obsidian desktop instance**, so it is
   **not** headless and **not** suitable for CI. Use it for manual
   graph/link inspection when the app is installed.

> Scope: this covers verification tooling for ISC-10..12 of ISA `isa-6zq`
> (slug `okf-obsidian-vault`). The frontmatter that makes entries conformant
> (`type`, `tags`) is produced by the exfiltrator (Workstream 1, commit
> `e358b8651`).

---

## 1. Headless conformance check (primary, CI)

### What it verifies

OKF v0.1 requires, and this checker enforces on every non-reserved `.md`:

| # | Requirement | Checker behavior |
|---|-------------|------------------|
| #1 | Parseable frontmatter | File must start with a `---` … `---` YAML block that parses as YAML. |
| #2 | Non-empty `type` | That frontmatter must carry a non-empty string `type`. |
| #3 | Reserved `index.md` | A **nested** `index.md` must **not** carry a frontmatter block. The **bundle-root** `index.md` (`<store>/entries/index.md`) is the one exception — it MAY carry `okf_version` frontmatter (progressive-disclosure root; ISC-9), provided the block parses and holds only benign bundle-metadata keys (`okf_version`, and optionally `title`/`tags`). |

**Reserved files** (`index.md`, `log.md`) are exempt from #1/#2 — they
legitimately have no frontmatter. Dotfiles/dot-directories (`.git`,
`.obsidian`, `.checkpoint.json`, …) are skipped.

> **Root-index exception (ISC-9).** `render-all` scaffolds a reserved
> `index.md` in every directory of the vault for OKF progressive disclosure.
> All of them are frontmatter-free **except** the bundle-root
> `<store>/entries/index.md`, which declares `okf_version: "0.1"`. The checker
> treats the `index.md` at the root of the path being checked as the bundle
> root and permits that one `okf_version` block; any **nested** `index.md`
> carrying frontmatter is still flagged under #3. A root index whose
> frontmatter carries a key outside the benign set (e.g. a stray `type` or
> `status`) is also flagged — the exemption is deliberately narrow.

The checker collects **all** violations (it does not stop at the first),
prints a per-file report, and exits **non-zero** on any violation, **0**
when clean.

### Run it

```bash
# Default: checks the checker's own hermetic testdata fixture — a fast,
# dependency-free green smoke test.
make okf-check

# Check a real rendered store (read-only; never mutates the store):
OKF_CHECK_PATH="$HOME/data/brain/entries" make okf-check
```

Or invoke the tool directly for more control:

```bash
# One or more paths: a store root, an entries/ dir, or a single .md file.
go run ./tools/okf-check "$HOME/data/brain/entries"

# Machine-readable output for scripting / CI annotations:
go run ./tools/okf-check --json "$HOME/data/brain/entries"
```

`--json` emits:

```json
{
  "checked": 1882,
  "passed": 0,
  "failed": 1882,
  "violations": [
    { "file": "…/entries/2026-05-30.md", "reason": "OKF #2: frontmatter is missing a non-empty `type` field" }
  ]
}
```

### Expected results

- A **freshly rendered** store (post-Workstream-1) → exit 0, zero
  violations.
- The **live `~/data/*` stores today** → expected to **FAIL**: their entries
  were exfiltrated before Workstream 1 landed and carry `kind` but no
  `type`. They are brought into conformance by the Workstream 5 migration
  (`render-all`), not by editing files. The checker flagging them is
  correct behavior, and the tool is strictly read-only — running it against
  live data never modifies anything.

### Where it's wired in CI

`scripts/ci/pr-core.sh` runs `go test … ./...`, which includes
`./tools/okf-check/...` — so the checker's **self-test** (the
conformant/nonconformant fixtures) is already a required PR gate.

To additionally gate a **rendered store's** conformance in CI (i.e. run the
checker against real exported markdown, not just its fixtures), a maintainer
should add one line to `scripts/ci/pr-core.sh` after the `go test`
invocation:

```bash
# OKF v0.1 conformance of the rendered vault (headless).
OKF_CHECK_PATH="<path-to-rendered-store>/entries" make okf-check
```

This is deferred rather than wired now because CI has no rendered store
checked out, and the live `~/data/*` stores intentionally still fail
pre-migration (Workstream 5). Wire it once a conformant store exists in the
CI environment (e.g. a post-`render-all` fixture store committed to the
repo).

---

## 2. Official `obsidian` CLI (secondary, interactive)

The official Obsidian CLI (shipped with Obsidian **1.12+**, Feb 2026) gives
richer graph/link verification, but it is a **client to a running desktop
app** — it drives the live app with the vault open. It **cannot** run
headless and is therefore **not** a CI gate; use it for manual inspection.

### Prerequisites

Neither the Obsidian desktop app nor its CLI is installed on this machine
(`obsidian` is not on `PATH`; there is no `/Applications/Obsidian.app`).
Before using this section:

1. Install Obsidian **1.12 or newer** (<https://obsidian.md/download>).
2. Enable the CLI per Obsidian's docs (the `obsidian` binary ships with the
   1.12+ desktop app; on macOS it is under the app bundle — expose it on
   `PATH` or use the full path).
3. Launch the desktop app and open the store's `entries/` directory as a
   vault (the CLI talks to this running instance).

### Commands for compliance verification

Each command targets a vault via a `file="..."` argument pointing at a path
inside the open vault. Run these against a **rendered** store's `entries/`:

| Command | Purpose |
|---------|---------|
| `obsidian unresolved` | List broken/unresolved `[[wikilinks]]` — should be empty for a clean vault. |
| `obsidian orphans` | List notes with no inbound/outbound links — surfaces entries whose `brain link` edges did not render (relevant once Workstream 2 lands). |
| `obsidian properties file="…"` | Inspect a note's frontmatter properties (confirms `type` and `tags` render as Obsidian properties). |
| `obsidian eval code="…"` | Run custom JS over the vault for bespoke conformance assertions (e.g. iterate all notes and assert each has a non-empty `type`). |

> The exact flag/argument syntax for these subcommands should be confirmed
> against `obsidian help` / `obsidian <cmd> --help` in your installed
> version — verify against `obsidian help` before scripting. The command
> **names and purposes** above are the stable part; treat any specific
> argument form as "verify against `obsidian help`".

### What "clean vault" looks like

- `obsidian unresolved` → empty (no broken links).
- Graph view is non-empty and reflects `brain link` edges (once Workstream 2
  materializes edges as `[[slug]]` links; until then the graph will be
  sparse — that is expected pre-Workstream-2, not a conformance failure).
- `obsidian properties` shows `type` and `tags` as first-class properties on
  entries.

---

## Relationship between the two checks

| | Headless `make okf-check` | Official `obsidian` CLI |
|---|---|---|
| Runs in CI | ✅ yes | ❌ no (needs running app) |
| Desktop app required | ❌ no | ✅ yes |
| Checks `type` + parseable frontmatter | ✅ | partial (`properties`, `eval`) |
| Checks graph / unresolved links | ❌ (deferred to Workstream 2) | ✅ (`unresolved`, `orphans`) |
| Authoritative for the gate | ✅ | secondary/interactive only |

The headless checker is the source of truth for the CI gate. The `obsidian`
CLI is the human-in-the-loop richer check when the app is available.
