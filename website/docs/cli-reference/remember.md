---
id: remember
title: bd remember
slug: /cli-reference/remember
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc remember`

## bd remember

Store a memory that persists across sessions and account rotations.

Memories are injected at prime time (bd prime) so you have them
in every session without manual loading.

Each memory also gets a companion knowledge issue holding the same text, so
the insight can be commented on, tagged, shown, and searched like any other
bead. The memory key and the issue ID both name it: the key is what 'bd
memories' and 'bd prime' read, the issue ID is what comment/tag/show take
(and the key resolves to it). Pass --no-bead to store the memory alone.

Examples:
  bd remember "always run tests with -race flag"
  bd remember "Dolt phantom DBs hide in three places" --key dolt-phantoms
  bd remember "auth module uses JWT not sessions" --key auth-jwt
  bd remember "prod deploy needs a second approver" --no-bead

```
bd remember "<insight>" [flags]
```

**Flags:**

```
      --key string   Explicit key for the memory (auto-generated from content if not set). If a memory with this key already exists, it will be updated in place
      --no-bead      Store the memory only; do not mint the companion knowledge issue that comment/tag/show act on
```
