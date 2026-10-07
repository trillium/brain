# Vision

`brain` exists so that one person's memory and their work survive the tools that hold them.
It serves that person and the agents acting on their behalf, from the smallest model to the largest.
It turns scattered notes, tasks, decisions and findings into one graph that both a human and a machine can read.
It owns exactly one thing: the substrate where memory and work are the same kind of record.

## The record outlives the tool

Every bead keeps the id it was born with; no schema change rewrites an identifier.
Short ids serve agents, which parse them; long kebab-case names serve people, who read and speak them; both are kept and both are searchable.
A bead says which store it belongs to, and that answer is recorded rather than inferred.
The substrate is replaceable; the words are not.

## Markdown is for the reader, not the recovery

Markdown exists so that simpler agents and people can sort through the record as documents.
It is a setting, not a side effect: each store declares whether it exfiltrates, and where its markdown lands.
The default shape is one tree with a directory per store, and the data directory itself is configurable.
Exfiltration can also be aimed: a command can render a chosen query into a chosen directory.
It renders on mutation, so the documents stay current for the agents that read them.
Editing a rendered file can be made to pass back into its bead, per store, when that store wants it.
Deleting a rendered file never deletes a bead: it marks the bead for deletion, so an accident in a synced folder cannot destroy the record.
It is a view of the substrate, and the substrate stays the authority.

## A silent failure is the defect class

A store that is broken and a store with nothing to do never look the same.
A database that cannot be reached is a loud failure, never an empty result.
A link whose target exists in no store is refused when it is written, naming the store that should own the target.
A failure names the thing that broke, in the command that broke.
A partial write is a failure, not a result.
A number that under-reports - one id for many, a search that misses most of the content - is a defect even when it exits zero.
A warning is not a quieter failure: it says what degraded, and it can be routed to a person or a repair agent instead of being read and stepped over.

## Fix the mechanism, not the symptom

A fix names its root cause and removes it, rather than tolerating the bad value.
Two things that must agree agree by construction, not by convention.
A guard ships with the test that states what it refuses.
When two stores hold the same id, that id becomes a duplication record: both copies receive new ids, and the collision record keeps the lineage.
A link that crossed stores becomes an ordinary link in the one graph, and the boundary it crossed is recorded rather than encoded in a second edge type.
An honest gap in a record beats a fabricated entry that closes it.

## Additive, never subtractive

Opaque ids stay authoritative in machine-readable output, because tooling parses them.
A new field is added alongside the existing one, so existing readers keep working.
A format that gains a standard keeps its legacy fields.
A change that breaks a script is a breaking change, whatever the intent behind it.

## One federation, one surface

Many named stores, one binary, one search.
Stores are added deliberately: a store exists because a person created or registered it, never because a database was noticed.
Removing a store removes its data, and offers to export that data first.
A store is reachable through its own command, and that command behaves the same for every store.
Every registered store is asked to answer a read, and the answer is checked rather than assumed.
The federation converges on one database on one machine; a subordinate copy is a future capability, not a current one.

## Every mutation is an event

Every change emits an event, and events are durable: they are delivered to subscribers with retries until acknowledged.
A subscriber being down delays an event; it never loses one.
The event log is part of the record, not a side channel to it.

## Hooks observe, and may refuse

A hook may refuse a write before it happens, and the refusal is surfaced as the reason the bead was never created.
Whether a hook's own failure blocks the write is declared by that hook: a guard that must refuse fails closed, an observer fails open.
A hook nobody has configured warns, so the gap is visible rather than silently resolved in either direction.
Either way the failure or the gap is reported, because a hook that fails silently is worse than no hook.
The store remains the only writer, so a write is never the work of a hook.

## A programmatic surface, deliberately opened

Every action a person can take is available through an API, so other layers can be built on it.
That surface is opt-in: it is activated deliberately, layer by layer, never exposed by default.
The store is the only writer, which is what keeps the graph honest, not the interface it is reached through.

## The design record is maintained by the agents doing the work

Design documents and divergence entries are written and revised by the agents that do the work, and reviewed after the fact.
Every change to the record is versioned and clearly marked, and the documents carry semver.
The ISA document is retired: an ISA is a store type, not a document.

## Scope

It is not multi-user; a collaborator's stores are out of scope for now.
It is not a general-purpose issue tracker.
It is not a general sync engine; reaching another machine is a transport decision, not a new source of truth.
It is not a service for other people's data.
It does not batch writes: every mutation commits as it happens.
Personal content lives in the stores, never in this repository; the repository carries the tool, the design and the tests.
The repository holds itself to the standard it asks of its tools: a fix lands with the test that states what it refuses.

A change aligns when it makes a failure louder, a record more durable, or an existing reader keep working.
A change should be resisted when it quiets a failure, rewrites an identifier or a format in place, or adds a surface whose correctness nothing checks.
