---
id: store-prefix
title: bd store-prefix
slug: /cli-reference/store-prefix
sidebar_position: 999
---

<!-- AUTO-GENERATED: do not edit manually -->
Generated from `bd help --doc store-prefix`

## bd store-prefix

Manage the unified database's prefix-ownership record at runtime.

  bd store-prefix list              show every prefix, its owning store, and
                                    the reason the ownership was decided
  bd store-prefix add &lt;prefix&gt;      claim a new id prefix for this store
                                    (defaults to the wrapper's BD_NAME;
                                    claim for another store with --store)
  bd store-prefix release &lt;prefix&gt;  free this store's own id prefix (requires
                                    --confirm; refuses while it carries beads
                                    or the build decided it)
  bd store-prefix history &lt;prefix&gt;  show the ownership trail — claims and
                                    releases, newest first, with the reason
                                    each act carried

Prefixes are the mechanism "which store is this bead in" rides on: a bead's
namespace is the segment of its id before the first '-'. The command refuses
anything a reader could never scope to (a '-' inside a prefix), refuses a
prefix another store already owns (the record is never rewritten silently),
and is idempotent for the claiming store's own prefixes. A claimed prefix is
usable immediately: beads mint under it ("create --prefix &lt;prefix&gt;"), explicit
ids carry it, and every read (narrow or wide) resolves ownership from this
same record.

Release and history are the deliberate ownership-change edge
(docs/design/brain-prefix-release.md): release deletes the row only through
an explicit, confirmed act by the owner's own namespace, and every ownership
change — a claim and a release alike — appends a row to the append-only
brain_store_prefix_events audit table in the same transaction, so ownership
is never changed silently. A release is refused while the prefix still
carries any bead (absolute refusal, no --with-beads override) and for any
prefix the build decided; a transfer is release-then-claim.

On a legacy (per-store) database there is no namespace record to extend; the
command refuses instead of pretending.

```
bd store-prefix [flags]
```

### bd store-prefix add

Claim an id prefix for the named store in the unified database's
brain_store_prefixes record, so beads can mint under it immediately.

The prefix defaults to the wrapper invoking this command (BD_NAME); claim for
a different store with --store. The ownership decision is recorded with its
reason ("operator-added"), and when this namespace's scoped config is
addressable the prefix is appended to the store's allowed_prefixes so the
create path accepts it in the same breath.

Refusals are loud and non-silent:
  - a '-' inside a prefix would disagree with the first-'-' bucket every
    reader uses, so the shape refuses it;
  - a prefix another store already owns refuses with the owner's name and the
    reason it was decided — transfer it only after that store releases it;
  - claiming a prefix this store already owns is a no-op (nothing changes).

```
bd store-prefix add <prefix> [flags]
```

**Flags:**

```
      --store string   Store whose namespace claims the prefix (default: the wrapper's BD_NAME)
```

### bd store-prefix history

Print the ownership trail of one id prefix from the append-only
brain_store_prefix_events table: every claim and every release recorded from
that table's adoption onward, newest first, with the reason each act carried.

Claims are recorded from the event table's adoption onward. Runtime rows that
existed before it have no event, and none is invented for them — the gap is
stated where you are looking, rather than filled with a fabricated row.
Every ownership change lands with its event in the same transaction, so no
path changes ownership silently.

```
bd store-prefix history <prefix> [flags]
```

### bd store-prefix list

List every id prefix the unified database's brain_store_prefixes record
knows: the prefix, the store that owns it, the reason the ownership was
decided (build-time "declared-by-store-config"/"store-name-matches-prefix" or
runtime "operator-added"), and the bead count observed at claim time.

```
bd store-prefix list [flags]
```

### bd store-prefix release

Free a prefix this namespace owns on the unified database's
brain_store_prefixes record, as a deliberate act by the owning store's own
wrapper (BD_NAME): there is no --store escape on the releasing side.

The act never happens as a side effect: it refuses unless --confirm is given,
and the confirmation is recorded in the append-only event row, so "deliberate"
is stored, not asserted. On success the ownership row is deleted — the prefix
returns to exactly its pre-claim state, re-claimable by anyone through the
ordinary 'bd store-prefix add' — and a 'release' event lands in
brain_store_prefix_events inside the same transaction, naming who released,
when, and why (--reason, recorded verbatim; optional).

Refusals are loud and none is bypassable:
  - without --confirm: release is the one namespace verb whose mistake is
    subtractive, so intent must be explicit;
  - the prefix still carries beads: ABSOLUTE refusal (no --with-beads
    override) — a prefix frees only when it carries no beads; migrate the
    beads off it first, which is the explicit, auditable path;
  - the prefix is build-decided ("declared-by-store-config",
    "store-name-matches-prefix", "first-observing-source",
    "no-source-declares-or-matches"): retracting the build's mapping decision
    is a re-unification act, not this verb;
  - the act is not performed as the owner's namespace;
  - the prefix is not recorded at all — nothing to release, an error, not a
    success-shaped no-op.

To transfer a prefix to another store, release it here and claim it there.
That unclaimed window is real — the next claim wins it — so do the two acts
promptly and check 'bd store-prefix list' between them.

```
bd store-prefix release <prefix> [flags]
```

**Flags:**

```
      --confirm         Confirm the release: the act never happens without it, and the confirmation is recorded in the event row
      --reason string   Why the prefix is being released (recorded verbatim in the event row; optional, max 64 characters)
```
