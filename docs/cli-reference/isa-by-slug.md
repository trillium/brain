---
title: "bd isa-by-slug"
description: "Resolve a slug to a brain id (O(1) lookup against the issues.slug column)"
---

{/* AUTO-GENERATED: do not edit manually */}

Generated from `bd help --doc isa-by-slug`.

Resolve an ISA slug to its brain id. Lookup is O(1) against the
issues.slug column (added in F1a; auto-populated in F1d).

  bd isa-by-slug ship-the-thing   # prints the id on success, exit 0
                                  # missing slug exits 1 with stderr message

isa-by-slug is restricted to kind=isa rows. A slug present on a non-isa row
is treated as not-found and exits 1.

```
bd isa-by-slug <slug> [flags]
```
