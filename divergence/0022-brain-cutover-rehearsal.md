---
id: 0022
title: cutover rehearsal — the runbook, and the unified-database behaviours it observed live
isc: []
status: landed
created: 2026-10-07
updated: 2026-10-07
commits: [b0611394ae3ca60c8d8036f72287a9b236aadbae]
touches: [docs/design/brain-cutover-runbook.md, docs/design/brain-single-database.md, divergence/0022-brain-cutover-rehearsal.md]
upstream_rebase_notes: |
  Doc-only entry; no code changes. The rehearsal drove no new divergence — it
  exercised the previously landed namespace model (831ea8612, 15351ba82,
  0dd496ca3, 2fa23685d, 716bf240b) end to end for the first time as a cutover.
  On rebase, the runbook's observed behaviours must still match the code it
  names: if the per-namespace identity guard, the storeless-mint refusal, the
  `--force` ownership refusal, or the render-slug overwrite refusal change
  shape, this entry and the runbook's §"[the design's promises]" command table
  need updating, because they quote the refusal texts verbatim.
---

# Why

(Number 0022: when this branch rebased onto `8bdc87001`, main had already issued
`divergence/0021` to `0021-brain-prefix-release-design.md`; ids never get reissued,
so this entry took the next free number. The rehearsal happened first; the prefix-
release design commits were authored in parallel.)
#g

The unified database existed and was verified (518/518 checks), but the act of
re-pointing one store's wrapper at it had never been exercised. The rehearsal
(`divergence` trail keeps design promises in code-centred docs; this entry keeps
the *observational* record: what the first real re-point looked like, including
the failure text an operator will see and the gaps found before production
could find them).

# What changed

- **`docs/design/brain-cutover-runbook.md`** (new) — the cutover runbook, written
  from what the rehearsal took, not from the design: the exact sequence for
  moving one store, the two-file pin edit and its revert, the per-runbook
  rollback proof (30/31 tables byte-identical; the one differing row is bd's
  per-open `local_metadata` tip stamp, with the mechanism named), the operator-
  visible changes, two rehearsal-infra findings that predict serving-machine
  requirements (a running Dolt server does not discover a newly placed
  database directory until restart; the port pin lives in both `metadata.json`
  and the wrapper and mismatching them yields a misleading error), the whole-
  database cost of per-store backups after cutover, a rejection of the
  two-server port-changing rehearsal shape for the real act (recommends
  cutover against the shared server so only `dolt_database` ever changes), and
  the honest list of what a real cutover on the serving machine additionally
  requires (placement + restart, deployed build, undecided binding/auth,
  concurrent-write window, real stores.yaml verification).
- No code change accompanies this commit; the behaviours recorded are the ones
  the namespace-model commits promised, and this entry is their observation
  record, not a new divergence.
