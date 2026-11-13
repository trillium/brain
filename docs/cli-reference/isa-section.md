---
id: isa-section
title: bd isa-section
slug: /cli-reference/isa-section
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc isa-section`

## bd isa-section

Set one of the twelve canonical ISA document sections on an isa-kind issue.

bd isa-section writes to the isa_sections table (issue_id, section_name, body)
as an UPSERT, and atomically touches issues.isa_updated_at = NOW() so the row's
"last touched" semantics match the rest of the ISA substrate.

Valid section names (lower_snake_case, case-sensitive):
  changelog, constraints, criteria, decisions, features, goal,
  out_of_scope, principles, problem, test_strategy, verification, vision

Input source is required; exactly one of:

  --value-from-file &lt;path&gt;   read the section body from a file
  --value-stdin              read the section body from stdin

isa-section is only valid for kind=isa. Calling it on any other kind exits 2.
Missing issue exits 1.

Examples:
  bd isa-section isa-001 problem    --value-from-file ./sections/problem.md
  cat changelog.md | bd isa-section isa-001 changelog --value-stdin

```
bd isa-section <id> <section-name> [flags]
```

**Flags:**

```
      --value-from-file string   Path to a file whose contents become the section body
      --value-stdin              Read the section body from stdin
```
