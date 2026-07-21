---
id: 0014
title: bd isa-show --json wire shape doesn't match isa-list --json (progress as nested object vs top-level fields)
isc: []
status: open
created: 2026-06-26
updated: 2026-06-26
commits: []
touches:
  - cmd/bd/isa_show.go
  - internal/brain/verb/isashow/isashow.go
upstream_rebase_notes: |
  This divergence does not exist on bd upstream. ISA verbs are brain-only.
  Any reshape to ISADoc must coordinate with isa_list.go output shape and
  any downstream consumers that parse the JSON (PAI/.claude/hooks/IsaLifter.hook.ts
  is the load-bearing consumer as of 2026-06-26).
---

# Symptom

`bd isa-show <id> --json` exposes ISA progress as a nested object:

```json
{
  "isa_progress": { "m": 14, "n": 15 }
}
```

But `bd isa-list --active --json` exposes it the same nested way:

```json
{ "isa_progress": { "m": 14, "n": 15 } }
```

The consumer-facing wire is internally consistent (both verbs use nested
`isa_progress`), but the `IsaLifter` hook in PAI was written against a flat
`isa_progress_m` / `isa_progress_n` shape that exists in the underlying
substrate (`issues.isa_progress_m`, `issues.isa_progress_n` columns) and in
the `bd patch --field=isa_progress_m --value=14` write path.

So the hook reads `current.isa_progress_m` (gets `undefined` because the
JSON has nested `isa_progress: {m, n}` instead), concludes the value is
unset, and idempotency breaks: every lifter run re-patches progress even
when brain already has it. The diff loop is also unable to skip the
patch when values match, because it can't read the current value.

# Reproduction

```bash
# Find any ISA with non-zero progress
brain isa-list --active --json | jq '.[] | select(.isa_progress.m > 0) | .id' | head -1
# brain-fc1f (or whatever)

brain isa-show brain-fc1f --json | jq '{isa_progress, isa_progress_m, isa_progress_n}'
# {
#   "isa_progress": { "m": 14, "n": 15 },
#   "isa_progress_m": null,
#   "isa_progress_n": null
# }
```

The flat fields are not present in the JSON, even though the substrate
stores them in flat columns and they are written via flat `--field`
patches.

# Expected Behavior

Option A — Flatten the wire (preferred): emit `isa_progress_m` and
`isa_progress_n` as top-level integer fields. Drop or deprecate the
nested `isa_progress` object. Rationale: writes use flat column names
(`bd patch --field=isa_progress_m`), so reads should too. Symmetry
matters when a hook is reading-then-writing.

Option B — Dual-emit (back-compat safe): emit BOTH `isa_progress: {m, n}`
AND `isa_progress_m` / `isa_progress_n` top-level. Lets existing
consumers keep working while new consumers can use the flat form.

Option C — Flatten everywhere (most invasive): also reshape `isa-list
--json` to flat. This is the most consistent end state but touches the
most consumers.

Recommendation: Option B for the next bd release (zero-break, lets
PAI's IsaLifter parse flat fields immediately), then Option A on a
subsequent major bump after consumers migrate.

# Current Workaround

`hooks/IsaLifter.hook.ts` in PAI normalizes both shapes — accepts
`isa_progress: {m, n}` OR `isa_progress_m` / `isa_progress_n` and
flattens locally before the diff comparison. See the `readBrainISA`
helper and the `progressString` callsite. Workaround is durable but
costs a couple lines of normalization in every brain consumer; would
prefer the wire shape match the write shape so consumers don't have to
think about it.

# Where the Bug Lives

`internal/brain/verb/isashow/isashow.go` lines 70-108 — `ISAProgress`
struct + `ISADoc.ISAProgress` field with `json:"isa_progress"` tag.
Reshape happens here.

The substrate columns are at `internal/storage/dolt/schema.go` (or
wherever the migrations live) as `isa_progress_m INT` and
`isa_progress_n INT`. The flat columns are the source of truth; the
nested wire is an artifact of the JSON marshaler.

# Why It Matters

The IsaLifter hook (PAI's load-bearing disk→brain sync, see
`brain-fc1f` ISA) needs read-then-diff-then-write semantics to be
idempotent. When read returns `undefined`, the hook always patches,
which:

1. Burns Dolt commit volume (every ISA edit produces N patch commits
   instead of zero when values are unchanged).
2. Pollutes the structured jsonl log at
   `MEMORY/STATE/isa-lifter.jsonl` with phantom `fields_patched`
   entries that don't reflect real state change.
3. Makes the `ISC-8` idempotency criterion impossible to verify
   without the workaround.
