---
id: store
title: bd store
slug: /cli-reference/store
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc store`

## bd store

Resolve &lt;name&gt; from the shared registry at call time and run the command
against that store, exactly as its wrapper would:

  brain store recipes list
  brain store recipes create "Pasta" --type task

The registry is read through the epoch ride-along (and cached under
~/.config/brain/registry.cache.json), so a store created on another machine a
moment ago works here immediately, before 'brain stores sync' has generated its
wrapper. An unknown name triggers one sync and one retry.

  brain store --check &lt;name&gt;   exit 0 when the cached registry has the name
                               (no database access; used by the shell hook)

'brain stores shell-hook zsh|bash' prints a command-not-found handler that
sends an unknown command name here when the registry has it.

```
bd store <name> [command…] [flags]
```
