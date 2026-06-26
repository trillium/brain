---
id: 0016
title: bd isa-by-slug --json silently emits plain text (no JSON shape implemented)
isc: []
status: open
created: 2026-06-26
updated: 2026-06-26
commits: []
touches:
  - cmd/bd/isa_by_slug.go
upstream_rebase_notes: |
  isa-by-slug is a brain-only verb (depends on the `slug` column on
  `issues`, brain F1a addition). Any --json wire shape addition is
  brain-only and resolves `ours` on rebase against bd upstream.
---

# Symptom

`bd isa-by-slug <slug> --json` accepts the flag (because `--json` is a
persistent root flag on rootCmd), but the verb implementation ignores
it and emits plain text — just the bare brain ID followed by a
newline. No JSON envelope, no error.

This bites any consumer that calls the verb with `--json` to be
robust against ID-shape changes and then tries to parse the output
with `jq` or `JSON.parse`. The parse fails, the consumer treats it as
"slug not found", and falls back to a mint path — minting a duplicate
row.

# Reproduction

```bash
brain isa-by-slug 20260625_isa-lifter-disk-canonical --json
# brain-fc1f

# Same as without --json:
brain isa-by-slug 20260625_isa-lifter-disk-canonical
# brain-fc1f
```

Compare to the equivalent shape on other verbs:

```bash
brain isa-show brain-fc1f --json | head -3
# {
#   "id": "brain-fc1f",
#   "slug": "20260625_isa-lifter-disk-canonical",
```

`isa-show` honors `--json`; `isa-by-slug` does not.

# Expected Behavior

Either:

Option A (preferred) — Emit JSON when `--json` is set:

```json
{ "id": "brain-fc1f", "slug": "20260625_isa-lifter-disk-canonical" }
```

Optional fields: `kind`, `isa_phase`, `isa_updated_at` — but minimum
viable is `{id, slug}`. Slug echo lets the consumer round-trip verify
the lookup. Missing slug stays the same exit-1-with-stderr.

Option B — Explicitly reject `--json` on this verb (not a great UX,
but at least the consumer's parse failure surfaces). Print to stderr:
"isa-by-slug does not support --json; pipe through `jq -Rn 'inputs |
{id: .}'` if you need JSON."

Option C — Document the silence. Add `--json on this verb is a no-op,
output is always plain ID` to the help text. Worst option — silent
flag absorption is a footgun.

# Current Workaround

`hooks/IsaLifter.hook.ts` `resolveBrainId` uses `runBdRaw` (the
text-tolerant wrapper) and validates the bare ID against the
store-agnostic regex `^[a-z][a-z0-9]*-[a-z0-9.]+$`:

```typescript
function resolveBrainId(slug: string, cwd: string): string | null {
  // bd v1.0.5: `isa-by-slug --json` prints the bare ID as plain text, not JSON.
  // Use runBdRaw and trim.
  const r = runBdRaw(['isa-by-slug', slug, '--json'], { cwd });
  if (!r.ok) return null;
  const id = (r.value || '').trim();
  return BD_ID_RE.test(id) ? id : null;
}
```

The `--json` flag is passed for forward-compat (so the day the verb
starts emitting real JSON, the consumer doesn't have to change), but
the parse path tolerates the current plain-text output.

# Why It Matters

- Footgun: a consumer who trusts `--json` semantics writes
  `JSON.parse(stdout)` and silently gets ID confusion. The mint-on-
  not-found path then makes duplicate rows.
- Inconsistent contract: every other ISA verb honors `--json`
  (`isa-list`, `isa-show`, `isa-render-pending`). The odd one out is
  the most-called-from-scripts verb.
- Lifter idempotency: the consumer-side workaround is durable, but if
  the bare-ID format ever changes (e.g. UUID instead of base32), the
  regex breaks and the consumer can't fall back to a JSON envelope
  because there isn't one.

# Recommendation

Add the JSON shape. Two lines in `runISABySlug` after the existing
text emit:

```go
if rootJSON {
    fmt.Printf(`{"id":%q,"slug":%q}` + "\n", id, slug)
    return
}
fmt.Println(id)
```

(Where `rootJSON` is the bool bound by the persistent root flag.
That's the same plumbing `isa-show.go` uses.)

Then update help text and `bd isa-by-slug --help` to mention
`--json`.
