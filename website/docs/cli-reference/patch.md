---
id: patch
title: bd patch
slug: /cli-reference/patch
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc patch`

## bd patch

Patch a single field on an issue, non-interactively.

bd patch is the primary mutation path for ISA-substrate fields:

  isa_phase, isa_progress_m, isa_progress_n,
  isa_effort, isa_mode, isa_started_at, isa_updated_at, slug

ISA fields are only valid on issues with kind=isa. Patching an ISA field on
any other kind exits with code 2.

Slug is special: it is patchable on any kind and does NOT touch
isa_updated_at.

Non-ISA, non-slug fields are routed to 'bd update' validation; if your
field is not in the patch allowlist, use 'bd update' instead.

Examples:
  bd patch isa-001 --field isa_phase --value BUILD
  bd patch isa-001 --field isa_progress_m --value 7
  bd patch bd-001  --field slug --value my-new-slug

```
bd patch <id> [flags]
```

**Flags:**

```
      --field string   Field name to patch (required)
      --value string   New value for the field (required)
```
