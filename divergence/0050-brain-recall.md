---
id: 0050
title: brain recall ranks beads with BM25 over full bodies and comments
isc: []
status: proposed
created: 2026-11-16
updated: 2026-11-16
commits: []
touches:
  - cmd/bd/brain_recall.go
  - cmd/bd/memory.go
  - cmd/bd/memory_embedded_test.go
  - cmd/bd/memory_proxied_integration_test.go
  - cmd/bd/capability_registry.go
  - cmd/bd/protocol/memory_contract_test.go
  - engdocs/ADDING_AN_ISSUEOPS_ROLE.md
  - cmd/bd/brain_capability_rows.go
  - internal/brain/verb/recall/recall.go
  - internal/brain/verb/recall/recall_test.go
  - internal/storage/dolt/unified_namespace.go
  - docs/brain/recall-eval.json
  - scripts/brain-recall-eval.sh
  - docs/CLI_REFERENCE.md
  - docs/cli-reference/recall.md
  - docs/cli-reference/memories.md
  - README.md
  - divergence/0050-brain-recall.md
upstream_rebase_notes: |
  `cmd/bd/memory.go` is an upstream file (the memory plane, upstream commit
  ed8dc5674). The brain edit moves its root `recall` command under
  `memories` (`bd memories recall <key>`) so the root name is free for this
  verb; `bd remember <key>` still reads a memory. On rebase keep the
  `memoriesCmd.AddCommand(recallCmd)` registration and drop any upstream
  `rootCmd.AddCommand(recallCmd)`. The two memory test files only change the
  argv they spawn. `capability_registry.go` is upstream: its permitted list
  loses `recall` and gains `memories recall`; the `recall` refusal row lives
  in brain's own `brain_capability_rows.go`. `unified_namespace.go` gains one exported wrapper
  (`NarrowNamespacePrefixes`); resolve `ours`. Everything else is brain-only.
---

# Why

brain is a memory. The question it gets asked most is "where did I write
about X?", and the answer is often in a comment, a close reason or deep in a
long note. `brain search` is a filter: every query word must appear as a
substring, the survivors are ordered by where the words sit, and a bead
that mentions the topic once in its title ties with one whose comments are
about nothing else. There was no ranked read of the whole bead (idea-y0d;
related robots-4m0m and project-p20f.6).

# What changed

- **`brain recall <query> [--top N] [--kind <type>] [--json] [--wide]`**
  ranks every bead in the wrapper's namespace with BM25 (k1 1.5, b 0.75,
  Lucene idf) over title, description, design, notes, acceptance criteria,
  close reason, labels and every comment, and prints score, status, id,
  kind, title and which fields matched. Title hits and title phrase hits get
  a bonus; so do label hits. Query words are lowercased, lightly stemmed,
  and words of four letters or more also match as prefixes at half weight.
- **Scoring reads Dolt, not the markdown mirror.** The exfil directory is
  not the store: with `BRAIN_NO_EXFIL=1` set by the wrappers it held 2,083
  files for 3,606 beads and 3 of them carried comments. The verb opens the
  store's raw connection (`storage.RawDBAccessor`) and scopes the read with
  the same namespace prefixes every other read uses.
- **Only candidate words cross the server boundary.** Shipping all comments
  over the tailnet cost 2.5 s and 24 MB per query, and shipping the full text
  of every prefiltered bead still cost 1.9 to 2.5 s on a query of common
  words. The verb uses a `REGEXP_LIKE` prefilter over the query terms, then
  has Dolt `REGEXP_REPLACE` every word that does not begin with a query term
  out of the matching body and comment text, so what ships is the candidate
  words plus `CHAR_LENGTH` document lengths. Counting stays local: the
  tokenizer in `internal/brain/verb/recall` is the single owner of the
  exact, stem and prefix rules, so a server-side count can never disagree
  with it (a server-side per-term count was measured at about 1.2 s per
  term per table and rejected). The reads go over a compressed MySQL
  connection pool, with the comment and body text partitioned by
  `CRC32(id)` into eight and four concurrent streams. Measured 2026-11-16
  on the live brain store (3,608 beads, Dolt on a tailnet host), with the
  store-open floor (`brain show <id>`) timed alongside: with the floor at
  0.55 s a cold run was 1.2 to 1.65 s on ordinary queries and 1.4 to 2.6 s on
  the common-word query `imsg full disk access launchd`; with the floor
  itself at 0.9 to 2.3 s the same queries were 1.4 to 3.1 s and 1.7 to 4.3 s.
  Load alone (prefilter, projection, transfer) is 0.7 to 0.8 s; the rest is
  CLI startup and the tailnet round trip.
- **The pure ranking is a verb package.** `internal/brain/verb/recall`
  tokenizes, indexes and ranks `Doc`s from any `Source`; the cobra wrapper
  builds the scoped corpus. Tests cover tokenizing, comment-only matches,
  title over body, labels, id tokens, tie-breaks, filters, exact short terms,
  Unicode character lengths, and overlapping stem/prefix terms.
- **Fixed eval.** `docs/brain/recall-eval.json` holds 20 real queries with
  the bead each should return (two are answerable only from a comment);
  `scripts/brain-recall-eval.sh` scores recall and `brain search` side by
  side. On 2026-11-16: recall 16/20 in the top 3, search 16/20 (search
  already tokenizes and ranks since #6). Recall's misses are sibling beads
  on the same topic ranking above the one named, plus one whose answer is
  phrased differently from the query.
- **Read-only.** The verb never writes; it never touches the exfil files.
- **Memory read-by-key moved.** Upstream's `bd recall <key>` (memory plane)
  becomes `bd memories recall <key>`; `bd remember <key>` is unchanged. This
  is a deliberate departure from upstream protocol v0 §M1, whose CLI-level
  pin (`cmd/bd/protocol/memory_contract_test.go`) now spawns the moved verb.
  The HTTP API and its spec keep their `bd recall` wording for the memory
  exit-code contract; the contract itself is unchanged.
