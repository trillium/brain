# Voice identifiers: the additive `name` field

Agents remember, distinguish, and speak kebab-case names far more easily
than opaque ids (`task-5wxul`). Every issue-bearing JSON payload therefore
carries a derived human/voice-facing `name` **alongside** the canonical
`id`. The id stays authoritative; the name is a display aid.

## Derivation rule (deterministic)

`name` is a pure function of the bead title, implemented once in
`internal/types` (`DisplayName`) and mirroring the brain `slug.Auto`
algorithm (`internal/brain/verb/slug`), so every non-empty result also
satisfies the slug pattern and is directly usable as `--slug`:

1. Lowercase the title.
2. Collapse each run of non-`[a-z0-9]` characters to a single `-`.
3. Trim leading/trailing `-`.
4. Cap at 64 chars, walking back to the last `-` so the name ends on a
   word boundary.
5. Titles with no alphanumerics (or empty titles) yield `""`, and the
   field is omitted (`omitempty`).

Same title → same name, on every store, without a lookup.

## Collision behaviour

The function is many-to-one: two beads whose titles kebab to the same
string **share a name** (e.g. `"Fix login bug"` and `"Fix  login bug!!"`
both read as `fix-login-bug`). Consequences:

- `name` is NEVER unique, NEVER a lookup key, and NOT stable across
  title edits. Do not feed it to `show`, `update`, `close`, or any id
  parameter — resolution behaviour is unchanged.
- The canonical `id` remains the sole disambiguator. When two beads
  share a name, speak the name first, then spell out the ids.

## Why the id remains canonical

Opaque ids are load-bearing fleet-wide: store-command output is parsed
by tooling, ids land in wake queues, receipts, beads' own dependency
fields, and supervision records. Replacing ids would break parsers and
silently corrupt cross-store references. The additive `name` keeps every
existing `id` consumer working unchanged.

## Reuse note (no second name field)

Two name-like stores already exist and are deliberately untouched:

- `issues.slug` — persisted, unique-indexed, required for ISA docs and
  optional otherwise. Backfilling it for every bead would turn a display
  change into a write-path migration (collision errors on create).
- `issues.metadata.brain_slug` — the exfiltrator's stable on-disk
  filename, scoped per kind/flat layout.

`name` reuses their derivation shape but persists nothing: a derived
projection with no schema change and no write-path failures.

## How an agent should present a bead when speaking

1. Say the `name` first: *"fix-login-bug"*.
2. Keep the `id` available for action: *"task-5wxul, fix-login-bug"*.
3. Run every command with the `id`, never the name.

## Command surfaces that gained the field (API change)

Every `--json` payload that embeds an issue object now includes `name`
next to `id`: `show`, `list`, `ready`, `blocked`, `update`, `create`
(including `--dry-run` preview), `export`, `tree`/`explain` variants,
and any other command marshaling `types.Issue` (directly or via
`IssueDetails`, `IssueWithCounts`, ready/blocked items). Human-readable
formats are unchanged.

## Before / after (`bd show --json`)

Before:

```json
{"id": "task-5wxul", "title": "Use kebab-case bead names as returned IDs"}
```

After (id byte-for-byte identical, `name` added):

```json
{
  "id": "task-5wxul",
  "name": "use-kebab-case-bead-names-as-returned-ids",
  "title": "Use kebab-case bead names as returned IDs"
}
```
