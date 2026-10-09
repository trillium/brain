# Divergence 0017 — `bd dep add` silently writes orphan rows for cross-store targets

**Filed:** 2026-06-26
**Source:** PAI multi-store cross-link audit (`~/.claude/PAI/MEMORY/STATE/brain-cross-store-link-audit.md`)
**bd version at filing:** v1.0.5
**Status:** Resolved — cross-store targets remain visible in reader views.

## Symptom at filing

`bd dep add <local-id> <foreign-id>` returned exit 0 with `✓ Added dependency: <local-id> depends on <foreign-id> (blocks)` when `<foreign-id>` lived in a different store (different bd prefix, different `.beads` dir, different Dolt database). The row was persisted to the `dependencies` table with `depends_on_issue_id IS NULL` and `depends_on_external='<foreign-id>'`, but user-facing read verbs hid it.

At filing, sibling verbs — `bd link`, `bd brain link`, `bd dep relate` — errored with `no issue found matching "<foreign-id>"`. Only `bd dep add` accepted the foreign-prefix ID silently.

## Reproduction at filing

The following output records the pre-fix behavior.

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

# At filing, every read path hid the edge:
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

## Resolution

The reader-rendering design was restored. A cross-store `dep add` continues to
persist its target in `depends_on_external`; the reader no longer drops an edge
because its target has no row. Since unification all stores share one `issues`
table, so:

- a target that exists (bare ID) displays as the real bead;
- a legacy `external:<store>:<id>` value is resolved to `<id>` before the
  lookup, which surfaces those edges as real beads;
- a target with no row (typo or deleted bead) stays visible, marked
  unresolved (`(not found)` in `dep list`; `"unresolved": true` in JSON)
  instead of looking like a P0 bead with a zero timestamp.

`dep list` (text and JSON), `brain related`, and `show --json` all behave this
way. `dep add` still accepts a missing target. Cross-store targets remain leaf
nodes rather than triggering a traversal.

The user-facing contract is documented in [Dependencies and Gates](../docs/DEPENDENCIES.md#external-and-cross-store-dependencies).

## Why this mattered

Cross-store linking is a primary use case for a multi-store brain federation.
Hiding a successfully persisted edge made users retry it, accumulate unseen
rows, and work around the CLI with metadata fields.

## Historical PAI mitigation

Before the reader fix, the audit recommended canonicalizing
`metadata.<store>_<kind>_id` (for example, `metadata.brain_isa = "brain-fc1f"`
on a decision row) as a durable cross-store reference pattern. That workaround
is not required for a dependency edge; use the documented dependency contract
above when an edge is the intended relationship.

## Regression coverage

`internal/storage/dolt/cross_store_edges_test.go` covers an absent target
(returned with its ID, type, and unresolved marker) and the legacy
`external:<store>:<id>` form (resolved to the real bead).
`TestResolveExternalDepTarget` in `internal/storage/issueops` covers the
prefix parsing.

## Related divergence docs

- 0014 — `bd isa-show --json` wire-shape drops `isa_progress_m/n`
- 0015 — `bd create` lacks `--slug` flag
- 0016 — `bd isa-by-slug --json` silently emits plain text

These together suggest a v1.1.0 cleanup pass on the "write succeeds, read disagrees" footguns — same class of bug, different verbs.

## Historical test artifacts

The original audit left an orphan row in `~/data/brain` as evidence:

```sql
DELETE FROM dependencies WHERE issue_id='brain-5wcg' AND depends_on_external='person-dnf';
```

Test entries (`brain-5wcg`, `brain-heg7`, `person-dnf`) closed via `bd close`.
