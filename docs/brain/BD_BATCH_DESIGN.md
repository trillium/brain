# `bd batch` — one-transaction, many-ops primitive

Status: IMPLEMENTED-EXTENSION (brain-dev, 2026-07-20). Motivating brain entry: brain-g6rho.
Branch: `feat/bd-batch` off `feat/isa-substrate-f1` (the live-binary source).

## ⚠️ CORRECTION (2026-07-20)

`bd batch` ALREADY EXISTED in the codebase (and on the LIVE binary) — a
write-oriented runner that executes a line grammar in a SINGLE dolt transaction
(one DOLT_COMMIT, all-or-nothing rollback). This is architecturally superior to
the in-process cobra re-dispatch this doc originally proposed; that proposal is
obsolete. The real gap was GRAMMAR COVERAGE: the existing grammar
(close/update/create/dep) did not include the verbs a guard/obligation discharge
needs.

DONE on this branch: extended the existing `batch.go` grammar with
`label add <id> <label>`, `label remove <id> <label>`, and a `description` key
for `update`. A full discharge — rate (`label add … helpful`) + record
(`update … description="## Decision…"`) + ack (`close …`) — now runs as ONE
`guard batch` call in one transaction. Verified: build clean, existing batch
tests green, live-shaped smoke committed all three ops and set label+description+
closed on the target beads.

DEFERRED: a `note <id> <text>` op — `AddComment` is a stub in the embedded-dolt
Transaction backend (`embeddedTransaction: AddComment not implemented`), so it
was dropped rather than shipped half-working. Add it once AddComment lands in the
embedded backend (server-mode may already support it — untested).

REMAINING: teach the ObligationGuard/BehaviorEnforcement Stop hook to emit a
single pre-filled `guard batch` heredoc; add unit tests for the new label/update
grammar. See task-xpay.

---
_Original design (obsolete re-dispatch approach) retained below for provenance._


## Problem

Every federated-store CLI (`guard`, `robots`, `task`, `brain`, ...) is a thin shell
wrapper over the same `bd` Go binary. Each invocation is one process boot + one
shared-Dolt (:3307) connect — measured ~0.18–0.30s. Agents can chain commands in a
single shell call, but that is still N sequential process spawns.

Same-verb, many-ID ops already batch: `close [id...]`, `reopen [id...]`,
`delete [id...]`, `label add [id...] [label]`. The unsolved friction is **cross-verb**
batches — e.g. an obligation discharge is `label add` (rate) + `close` (ack) +
`update -d` (record) = three different verbs that cannot collapse into one `bd`
invocation today. Cost: ~1s of spawns for a 4-item discharge, plus the token/cognitive
cost of an agent emitting 3–4 distinct commands.

## Goal

A single `bd batch` verb that executes an arbitrary mix of verbs in ONE process against
ONE cached Dolt connection. General across every store and every verb — NOT a narrow
`guard discharge` (one-primitive / retirement discipline: no special verb per workflow).

## Feasibility (verified)

- The store is memoized per-repo in `internal/storage/embeddeddolt/cache.go`
  (`cache[key]` with refcount). So if multiple ops run in one process, they REUSE the
  same open Dolt store — the batch win (avoid N reconnects) comes for free.
- Cobra supports in-process re-dispatch: `rootCmd.SetArgs(opArgs); rootCmd.Execute()`.
  Re-dispatch re-runs `PersistentPreRunE`/`PersistentPostRunE` per op, which is DESIRABLE
  — each op still emits its own change-event (change_events.go) and metrics.

## Sharp edges (must handle in impl)

1. **Flag state leaks between ops.** Cobra persists parsed flag values on the command
   objects. Between ops, reset flags (walk `rootCmd` + subcommands, restore defaults) or
   re-parse cleanly. This is the #1 correctness risk.
2. **`PersistentPreRunE` side effects per op.** It calls `setupGracefulShutdown()`
   (re-registers signal handlers), starts a telemetry span, and reads `os.Args[1:]` for
   the `bd.args` attribute (main.go:674) — wrong for re-dispatched ops (cosmetic only).
   Signal-handler re-registration per op should be made idempotent or hoisted.
3. **Error semantics.** Decide per-op failure policy: `--stop-on-error` (default) vs
   `--continue`. Emit a per-op result line so callers see which op failed.
4. **Output contract.** Default: one result line per op (`ok <verb> <id>` / `err <verb> …`).
   `--json`: array of `{op, ok, id?, error?}`. Keep stdout parseable.
5. **Dolt auto-commit / txn.** With the shared store, ops share the working set. Decide
   whether batch wraps all ops in one Dolt transaction (atomic all-or-nothing) or commits
   per op. v0: per-op (matches today's semantics); `--atomic` is a later flag.

## Input format

Newline-delimited ops on stdin. Two accepted shapes:

```
# shell-ish (simplest for hooks to emit): one op per line, shell-tokenized
label add guard-a guard-b helpful
close guard-c
update guard-d -d "## Decision\n..."
```

```json
// or JSON-lines when args contain awkward quoting
{"args": ["label","add","guard-a","helpful"]}
{"args": ["close","guard-c"]}
```

`bd batch` reads until EOF, tokenizes each line (shell-words for the plain form, or
`.args` for JSON), and re-dispatches. `bd batch --ops-file <f>` as an alternative to stdin.

## Downstream UX win

Once `bd batch` exists, teach the ObligationGuard / BehaviorEnforcement Stop hook to emit
a SINGLE pre-filled line instead of 3–4:

```
guard batch <<'OPS'
label add guard-hwoa helpful
close guard-lvmi
update guard-2sk0 -d "## Decision\n..."
OPS
```

One paste, one spawn, all obligations discharged.

## Test plan

- Unit: tokenizer (plain + JSON-lines), flag-reset-between-ops (op B must not inherit op A's
  flags), error policy (stop vs continue), JSON output shape.
- Integration (embedded dolt, no server): batch of create+label+close against a temp store;
  assert single store open (refcount), each op's change-event emitted, exit code reflects
  policy.
- Regression: a batch with a bad verb mid-stream under `--stop-on-error` leaves prior ops
  applied and halts; under `--continue` applies the rest and exits nonzero.

## Rollout

Prototype on `feat/bd-batch`; build + `go test ./cmd/bd/ -run TestBatch`. NOTHING installed
over live `~/.local/bin/bd` without captain install-consent. Ship behind no flag (additive
verb); the hook change lands only after `bd batch` is installed.
