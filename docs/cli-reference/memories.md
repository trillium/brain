---
title: "bd memories"
description: "List or search persistent memories"
---

{/* AUTO-GENERATED: do not edit manually */}

Generated from `bd help --doc memories`.

List all memories, or search by keyword.

Examples:
  bd memories              # list all memories
  bd memories dolt         # search for memories about dolt
  bd memories "race flag"  # search for a phrase

```
bd memories [search] [flags]
bd memories [command]
```

## bd memories recall

Retrieve the full content of a memory by its key.

Examples:
  bd memories recall dolt-phantoms
  bd memories recall auth-jwt

```
bd memories recall <key> [flags]
```
