---
id: group
title: bd group
slug: /cli-reference/group
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc group`

## bd group

A group is a bead that tracks other beads with non-blocking "tracks"
edges. Members are never blocked by the group and tracking does not change
ready-work calculation. When every tracked member is closed, the group
closes itself.

Use a group for a push with a finish line. Keep long-lived project beads
for durable knowledge rather than as a to-do list.

Members must live in the same store as the group.

```
bd group [flags]
```

### bd group add

Track more beads in a group

```
bd group add <group-id> <member-id>... [flags]
```

### bd group create

Create a finish-line group, optionally tracking members

```
bd group create <title> [member-id...] [flags]
```

**Flags:**

```
  -d, --description string   Group description
  -p, --priority int         Group priority (0-4) (default 2)
```

### bd group remove

Stop tracking beads in a group

```
bd group remove <group-id> <member-id>... [flags]
```

### bd group show

Show a group's members and progress

```
bd group show <group-id> [flags]
```

### bd group sync

Close every open group whose members are all closed

```
bd group sync [flags]
```
