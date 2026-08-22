---
id: transfer
title: bd transfer
slug: /cli-reference/transfer
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc transfer`

## bd transfer

brain transfer moves an existing brain doc from its current store
to a destination store on the same Dolt SQL server, in a single
atomic transaction.

&lt;id&gt;    The source doc's id (e.g. "inbox-abc"). The first token before
        the "-" is treated as the store prefix; the prefix must resolve
        to a known store.

&lt;dest&gt;  The destination store name. Must be one of the registered store
        names — for the canonical PAI federation these are:
        brain, tasks (alias: task), projects (alias: project),
        agents (alias: agent), inbox, decisions (alias: decision),
        ideas (alias: idea), life, questions (alias: question), assert.

On success the source row is closed with a close_reason recording the
destination id, a new row is created in the destination store under
that store's prefix, and a 'supersedes' edge is written into the
source store's dependencies table (with depends_on_external set to
the new dest id) so the move is queryable from either side. All
changes commit as one transaction — a failure at any step rolls back
everything.

Examples:
  brain transfer inbox-abc brain    # move an inbox capture into the brain hub
  brain transfer inbox-abc task     # move an inbox capture into tasks
  brain transfer assert-fye brain   # move an assertion into the brain hub

```
bd transfer <id> <dest> [flags]
```
