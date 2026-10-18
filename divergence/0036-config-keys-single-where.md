---
id: 0036
title: one WHERE on the scoped key-list config read
isc: []
status: proposed
created: 2026-10-17
updated: 2026-10-17
commits: []
touches:
  - internal/storage/issueops/config_helpers.go
  - internal/storage/issueops/unified_config_test.go
  - divergence/0036-config-keys-single-where.md
upstream_rebase_notes: |
  The namespace routing in `internal/storage/issueops/config_helpers.go` is
  brain-only (the `UnifiedScopeForTx` selector); upstream has no
  `brain_unified_*` tables. `unified_config_test.go` is brain-only too. On a
  rebase that touches `getConfigKeysInTx`, resolve `ours` and re-apply: the
  selector's store predicate is already a complete `WHERE` clause, so the
  key-list predicate must be joined with `AND`, never concatenated.
---

# Why

Item (2) of the captain's 2026-10-17 store-configuration fixes (task-s0icm,
brain-e5tlj comment 10-17 13:49). On the unified database every `bd` call that
resolved custom config went through `getConfigKeysInTx`, which concatenated two
independently rendered predicates:

```
SELECT `key`, value FROM brain_unified_config WHERE `store` = ? WHERE `key` IN (?,?)
```

The namespace predicate comes from the unified selector as a complete clause
(`WHERE` + the store column, empty on a legacy table); the key-list predicate added
its own literal `WHERE`. Dolt rejects the doubled clause, so `ResolveCustomConfigInTx`
hit a `nil, false, nil` error path and silently fell back to `config.yaml`.
That is the brain regression introduced by the unified-namespace routing in
`0dd496ca3` (fm/brain-unify-verify): the same concatenation shape is correct for
`ORDER BY`-terminated reads, where the selector's clause is the only `WHERE`.

# What changed

- `internal/storage/issueops/config_helpers.go` — `getConfigKeysInTx` composes
  the selector's namespace predicate with the key predicate into one
  `WHERE ... AND ...` clause when a namespace is pinned, and leaves the legacy
  `... FROM config WHERE key IN (?,?)` statement byte-for-byte unchanged. No
  change to the arguments, the table selector, or any other read.
- `internal/storage/issueops/unified_config_test.go` —
  `TestGetConfigKeysInTxSingleWhere` pins both shapes with exact quoted-statement
  expectations (the quoted scoped statement cannot match the pre-fix double
  `WHERE`, so the test fails on the old code) and asserts that the scoped lookup
  returns exactly the requested keys rather than degrading to YAML.

# Brain-spec link

[`VISION.md`](../VISION.md) — a store is a namespace of one database, and the
config layer must address the wrapper's own rows. The authoritative design for
the re-keyed config tables is
[`docs/design/brain-single-database.md`](../docs/design/brain-single-database.md)
("What changes shape, and why"; "What must be true before step 3", item 3). The
historical ISC range this work descends from lives in the retired ISA at
[`docs/brain/archive/ISA-v03.md`](../docs/brain/archive/ISA-v03.md).
