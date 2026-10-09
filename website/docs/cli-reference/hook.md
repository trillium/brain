---
id: hook
title: bd hook
slug: /cli-reference/hook
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc hook`

## bd hook

Hooks declared in &lt;beadsDir&gt;/hooks.d/&lt;name&gt;.toml observe mutations and may
refuse them. A guard hook runs before a write and can refuse it (fail-closed);
an observer runs after (fail-open); a hook that declares no policy warns.

Every failure of an observer or unset-policy hook becomes a durable warning
record in the store. Hooks never write: the store is the only writer.

See docs/brain/HOOKS.md.

```
bd hook [flags]
```

### bd hook list

List every hook declared in hooks.d and report every definition that cannot be
loaded. Exits non-zero when any definition is unusable. Writes nothing.

Examples:
  bd hook list
  bd hook list --json

```
bd hook list [flags]
```

### bd hook test

Run a declared hook exactly as the write path would — same shell, same
sanitized environment, same timeout — against a sample or supplied payload, and
print its exit status, output and what its declared policy would do with it.
Writes nothing.

Examples:
  bd hook test no-secrets --event create
  bd hook test no-secrets --payload-file payload.json

```
bd hook test <name> [flags]
```

**Flags:**

```
      --event string          Event to run the hook for: create, update, close or delete (default "create")
      --payload-file string   JSON file holding the payload to pipe to the hook
```

### bd hook warnings

List the durable warning records hooks have produced. A warning says which hook
degraded and what happened; it stays "open" until acknowledged, so it can be
routed to a person or a repair agent instead of being read and stepped over.

Records live in the store's config table under the "hookwarning." prefix, so
anything that can read the store can read them (bd hook warnings --json).

Examples:
  bd hook warnings                      # open warnings
  bd hook warnings --status all --json
  bd hook warnings --ack hw-20261008T101500Z-1a2b3c4d
  bd hook warnings --ack-all

```
bd hook warnings [flags]
```

**Flags:**

```
      --ack stringArray   Acknowledge the warning with this id (repeatable)
      --ack-all           Acknowledge every open warning
      --status string     Show warnings with this status: open, acknowledged or all (default "open")
```
