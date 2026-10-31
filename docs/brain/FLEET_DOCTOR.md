# Fleet doctor: are all stores on all systems pointing at the right store?

`brain stores doctor --fleet` answers that question for every machine in the
roster (`~/.config/brain/fleet.yaml`; default mini0, mini1, macbook). It extends
the registry convergence check that already existed; the audit below runs in
addition, and `--no-audit` restores the convergence-only view.

The command exits **1 on any drift** (0 clean, 2 registry unreadable) and prints
`FLEET DRIFT: <n> item(s) on <machines>` on stderr. `--json` carries the whole
report: the convergence verdicts plus an `audit` object (`hosts`, `drift`,
`orphan_check`, `clean`).

## What it reports per machine

| Item | Source | Drift when |
| --- | --- | --- |
| Installed binary | `stores host-audit` build stamp, compared with the orchestrating clone's `origin/main` using the same classification as single-host `stores doctor` (PR 72) | not on main, behind main, dirty, or unstamped; a binary too old to have `host-audit` is `audit-unavailable` |
| `store-host.env` | the file on that machine | missing, unusable, or different from the **live host** |
| Process env | `BEADS_DOLT_SERVER_HOST` in the audit process | differs from the expected host (it overrides the file), unless the `brain` wrapper's own pin or `store-host.env` supplies that value (then it is reported there) |
| Host pins | every literal `BEADS_DOLT_SERVER_HOST/PORT` in `~/.local/bin` wrappers, in env files those wrappers `.`/`source`, and in `~/.config/brain-remote/*.env` | differs from the machine's `store-host.env` (else the live host) |
| Read probe | each store through its own wrapper (`list --limit 1`) | fails or times out |
| Write probe | each store through its own wrapper, see below | fails |
| Backup freshness | `backup status --json` through each wrapper | none recorded, unreadable, or older than `--backup-max-age` (default 48h; 0 disables) |
| Reachability | ssh | unreachable is reported for that machine only; the rest are still checked. It still counts as drift |

The **live host** is `--live-host host:port`, else the store-host.env of the
machine running the doctor. It is also dialled; a dead live host is drift.

This reproduces the 2026-10-18 manual audit: a machine with no `store-host.env`
(`store-host`), mini0's `task` wrapper pinned to mini1 although its
`store-host.env` named itself (`pin`), the retired
`~/.config/brain-remote/mini1.env` shim (`pin`, source `env-file`), per-machine
pin counts (`pin_count`), and mini2 unreachable (`unreachable`, never touched).

## Write probe footprint

Per store, through the store's own wrapper (so a host pin that misroutes the
wrapper is caught), using `sql` statements:

1. `CREATE TABLE IF NOT EXISTS brain_fleet_probe_<nonce> (id, ts)`: a table of
   its own per probe, so machines probing the same store at once never collide
2. `INSERT` one row keyed by a random nonce
3. `SELECT` that row back
4. `DELETE` it
5. `DROP TABLE brain_fleet_probe_<nonce>`

No issue, dependency, label or other bead table is read or written. On success
the store is left exactly as found apart from what Dolt's auto-commit may record
for the create/drop. If a step fails the delete and drop are still attempted and
a leftover table is named in the result. Embedded stores (no SQL server) are
`skipped`. Disable with `--no-write-probe`; read probes still run.

## `--repair`

Default is report-only. `--repair` runs the existing `brain stores
rewrite-wrappers` on each **reachable** machine that has wrapper-pin drift and a
valid `store-host.env` that agrees with the expected store host (the live host,
or the machine's roster `store_host`), then re-audits it. A machine whose
`store-host.env` is missing, unusable, or names another host is refused with
the reason: rewriting its pins would follow the wrong host. It never installs binaries, edits
`store-host.env`, touches Dolt servers, edits env files (those pins are reported
with a manual fix), or acts on an unreachable machine.

## Orphan beads

The doctor compares the bead ids (`issues.id`) of every database on each
non-live Dolt server with the live host, per database: `--old-host host:port`
(repeatable) plus any `dolt sql-server` the machines report running locally
(stray servers). An id present there but not live is an orphan; a database the
live host lacks entirely is listed with `live_database_missing`. Any orphan is
drift. An `--old-host` that cannot be scanned (refused login, timeout, down) is
drift too, with the cause; a stray server that cannot be scanned is reported
but not fatal. Credentials: `BEADS_DOLT_SERVER_USER` (default root) and
`BEADS_DOLT_PASSWORD`. Replay is only **offered**: the JSON lists a review-first
export/import command per database; nothing is replayed automatically.

## Machines that reach Dolt through a forwarder

A machine whose `store-host.env` legitimately holds a loopback address (a local
forwarder to the live host) declares it in `fleet.yaml` so it is not reported as
drift:

```yaml
machines:
  - name: mini3
    store_host: 127.0.0.1:3307
```

Hosts running an older `brain` without `stores host-audit` are reported as
`audit-unavailable` (not unreachable). Backup drift is grouped to one item per
distinct reason, and `pin_count` counts distinct wrapper/env files.
