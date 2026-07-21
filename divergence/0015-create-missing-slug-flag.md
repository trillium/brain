---
id: 0015
title: bd create has no --slug flag; ISA mint requires two-step (create then patch --field=slug)
isc: []
status: open
created: 2026-06-26
updated: 2026-06-26
commits: []
touches:
  - cmd/bd/create.go
  - internal/brain/verb/new/new.go
upstream_rebase_notes: |
  The `--slug` flag exists in doctrine and downstream documentation
  (PAI/USER/DOCUMENTATION_ETHOS.md, multiple skill workflows) but does
  NOT exist on the binary as of bd v1.0.5. The discrepancy is a
  documentation/implementation drift, not a regression. Adding the flag
  is a brain-side addition (kind=isa is brain-only), so any landing
  patch belongs in the brain divergence track, resolved `ours` on
  rebase.
---

# Symptom

Documentation across the PAI surface (DOCUMENTATION_ETHOS.md, multiple
skills, divergence 0007) claims:

```bash
brain create --type=isa "<title>" --slug=<slug>
```

is the canonical mint path for ISA-kind rows. The flag does not exist
on the binary:

```bash
$ brain create --type=isa --slug=foo "test"
Unrecognized flag --slug. What did you expect to happen here? Filing a feature request...
Filed: brain-hjpr
Error: unknown flag: --slug
```

(The "filing a feature request" line is the cobra unknown-flag handler
auto-minting a brain entry for the missing feature — itself a useful
behavior but not a substitute for the actual flag.)

# Reproduction

```bash
brain create --type=isa --slug=test-slug-bug-repro "test bug 2 slug flag"
# Unrecognized flag --slug.
# Error: unknown flag: --slug
# exit 1
```

vs. the documented expectation:

```bash
brain create --type=isa --slug=ship-the-thing "Ship the thing"
# Expected: prints new brain id, exit 0, row has slug='ship-the-thing'
```

# Expected Behavior

Add `--slug <string>` as a flag on `bd create`. Validation: slug must
match `^[a-z0-9][a-z0-9_-]*$` (matches existing brain slug regex), be
unique within the kind (UPSERT not allowed at this level — duplicates
fail with a clear error), and be permitted ONLY when `--type=isa` or
`--type=knowledge` or `--type=both`. Tasks don't get slugs (existing
schema convention).

Single-step mint becomes:

```bash
brain create --type=isa --slug=ship-the-thing "Ship the thing"
# brain-abcd
```

# Current Workaround

Two-step mint as encoded in `hooks/IsaLifter.hook.ts` `mintBrainISA`:

```typescript
const create = runBdRaw(['create', '--type=isa', title], { ... });
const id = create.value.match(BD_ID_INLINE)[0];
const slugPatch = runBdRaw(['patch', id, '--field=slug', `--value=${slug}`], { ... });
```

Hazards of the workaround:

1. Non-atomic — the row exists with no slug between the two calls. A
   crash or timeout in the gap leaves a slug-less ISA row that
   `isa-by-slug` can't find, so the next lifter run mints a duplicate.
2. Doubles bd commit volume — two Dolt commits per mint instead of
   one.
3. Doctrine drift — every call site that read DOCUMENTATION_ETHOS.md
   and tried the documented form silently broke. The first lifter
   bring-up burned a session on this (brain-fc1f ISA, ISC-5 retro).

# Recommendation

Add the flag. It's a small addition: a `*string` on the cobra
flagset, a validation pass in the create handler (uniqueness check
against `issues.slug` within `issue_type` partition), and a substrate
write that sets `slug` in the same transaction as the row insert.

Atomicity is the load-bearing property — the workaround works but
leaves the system one well-timed kill -9 away from duplicate rows.

# Documentation Updates Needed Post-Fix

- `PAI/USER/DOCUMENTATION_ETHOS.md` — update the canonical example
- `~/.claude/PAI/DA_IDENTITY.md` — current text says "the `--slug`
  flag is broken in v1.0.5 — use the two-step mint workflow"; change to
  reflect the fix and re-enable the documented form.
- `hooks/IsaLifter.hook.ts` `mintBrainISA` — collapse to single
  `runBdRaw` invocation once the flag lands.
- `divergence/0007-brain-new.md` — note that brain create now supports
  --slug (was conspicuously absent in the original).
