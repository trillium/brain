# Divergence 0017 — `bd dep add` silently writes orphan rows for cross-store targets

**Filed:** 2026-06-26
**Source:** PAI multi-store cross-link audit (`~/.claude/PAI/MEMORY/STATE/brain-cross-store-link-audit.md`)
**bd version:** v1.0.5

## Symptom

`bd dep add <local-id> <foreign-id>` returns exit 0 with `✓ Added dependency: <local-id> depends on <foreign-id> (blocks)` even when `<foreign-id>` lives in a different store (different bd prefix, different `.beads` dir, different Dolt database). The row is persisted to the `dependencies` table with `depends_on_issue_id IS NULL` and `depends_on_external='<foreign-id>'`, but every user-facing read verb hides it.

Sibling verbs — `bd link`, `bd brain link`, `bd dep relate` — all error correctly with `no issue found matching "<foreign-id>"`. Only `bd dep add` accepts the foreign-prefix ID silently.

## Reproduction

```sh
# In store A:
$ bd -C ~/data/brain create --type=task "audit-host" --quiet
brain-5wcg

# In store B:
$ bd -C ~/data/person create --type=knowledge "audit-target" --quiet
person-dnf

# Cross-store dep add — exit 0, looks successful:
$ bd -C ~/data/brain dep add brain-5wcg person-dnf
✓ Added dependency: brain-5wcg depends on person-dnf (blocks)
$ echo $?
0

# But every read path hides the edge:
$ bd -C ~/data/brain dep list brain-5wcg --json
[{"issue_id":"brain-5wcg","depends_on":"brain-heg7","type":"related","status":"open"}]
# ^ only the WITHIN-store edge appears. The person-dnf edge is gone from view.

$ bd -C ~/data/brain brain related brain-5wcg
# Only brain-heg7 appears under RELATED. person-dnf is invisible.

$ bd -C ~/data/brain show brain-5wcg | grep -A2 RELATED
RELATED
- brain-heg7: ...
# No person-dnf.

# Direct SQL proves the row IS in storage:
$ dolt sql -q "SELECT issue_id, type, depends_on_issue_id, depends_on_external FROM dependencies WHERE issue_id='brain-5wcg'"
+------------+---------+---------------------+---------------------+
| issue_id   | type    | depends_on_issue_id | depends_on_external |
+------------+---------+---------------------+---------------------+
| brain-5wcg | related | brain-heg7          | NULL                |
| brain-5wcg | blocks  | NULL                | person-dnf          |  ← orphan
+------------+---------+---------------------+---------------------+
```

## Expected behavior

One of:

1. **Reject foreign IDs at write time.** Match sibling verbs (`bd link`, `bd dep relate`) — error with `no issue found matching "<foreign-id>"` and exit non-zero. This is the simplest fix and produces the strongest invariant: `dep add` write success implies the edge is visible to `dep list`.
2. **Render `depends_on_external` rows in read verbs.** Teach `bd dep list`, `bd brain related`, `bd show` to surface external-target rows with a clear marker (e.g. `→ person-dnf (external)`). This preserves the write but closes the gap between persisted state and visible state.

Option 1 is the safe default. Option 2 enables real cross-store graph semantics but requires the read verbs to know about the federation registry and to handle "target store not resolvable" gracefully.

## Why this matters

Cross-store linking is the dominant use case for a multi-store brain federation. The current state silently swallows edges that the user reasonably believes they wrote, leading to:

- **Data loss from the user's perspective.** `brain show` says no link exists; the user adds it again; orphan accumulates.
- **Lifter complexity.** PAI lifters (`PersonLifter`, `decisions-lifter`) already work around this with `metadata.<store>_<kind>_id` fields and explicit "expected to fail and is swallowed" comments around their `bd link` calls. These workarounds are correct but encode a footgun that should not exist at the CLI layer.
- **Doctor invariants.** `cmd/bd/doctor/integrity.go` is currently the ONLY consumer of `depends_on_external` outside `brain transfer`. Every orphan row is a future false positive in an integrity check.

## Current PAI mitigation

The audit recommended canonicalizing `metadata.<store>_<kind>_id` (e.g. `metadata.brain_isa = "brain-fc1f"` on a decision row in `~/data/decisions`) as the durable cross-store reference pattern. PersonLifter already uses `metadata.brain_slug`; decisions-lifter uses `metadata.parent_isa`. Both work without `bd link` and survive across stores.

## Suggested patch direction

In `cmd/bd/dep.go` (or wherever `dep add` resolves arguments), perform the same ID-resolution check that `bd link` does before writing. If `depends_on_issue_id` cannot be resolved within the current store, error out:

```go
if depID == nil && !explicit_external_flag {
    return fmt.Errorf("no issue found matching %q (use --external if you intend an external reference)", target)
}
```

An `--external` opt-in flag preserves the existing `depends_on_external` write path for callers that genuinely need it (`brain transfer` is the only known one today) without making it the silent default.

## Related divergence docs

- 0014 — `bd isa-show --json` wire-shape drops `isa_progress_m/n`
- 0015 — `bd create` lacks `--slug` flag
- 0016 — `bd isa-by-slug --json` silently emits plain text

These together suggest a v1.1.0 cleanup pass on the "write succeeds, read disagrees" footguns — same class of bug, different verbs.

## Test artifacts

Orphan row deliberately left in `~/data/brain` as evidence:

```sql
DELETE FROM dependencies WHERE issue_id='brain-5wcg' AND depends_on_external='person-dnf';
```

Test entries (`brain-5wcg`, `brain-heg7`, `person-dnf`) closed via `bd close`.
