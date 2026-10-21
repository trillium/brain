# Deploying the store registry (plan; nothing here has been executed)

This is the per-machine plan for putting `brain stores sync`, the ride-along, the
fleet doctor and the 5-minute poller on the three brain machines. It is a plan:
the change ships with **no** machine configured, and nothing in the build or the
tests installs the poller or touches a real store. Read
[`STORE_REGISTRY.md`](STORE_REGISTRY.md) first.

Order: **mini0 → mini1 → macbook**, with `brain stores doctor --fleet` between
machines. Each machine has a gate (what must be true before the next step) and a
rollback.

## 0. Before any machine

1. The PR is merged and a binary is built from the merged commit:
   `CGO_ENABLED=1 go build -tags gms_pure_go -o /tmp/bd-registry ./cmd/bd` and
   `/tmp/bd-registry --version` prints the expected version/commit.
2. **The version pin.** Every store wrapper sources `~/.local/bin/.bd-pin.sh`
   (`BD_BIN`, `BD_WANT`) and refuses to run a bd whose version string differs.
   Deploy the binary **and** the new `BD_WANT` together, through satellite-config's
   `converge-stores.sh` (it backs up and writes a `restore.sh`), or
   `bead-doctor` will go red at 06:15 and every wrapper will refuse to run.
   Keep the previous binary (`bd.pre-<sha>`, the convention already used).
3. Land PRs #41 / #43 first if they are still open: `sync` uses the wrapper
   conventions they establish (the store-host record). If PR #43 lands first,
   rebase this branch on it (the conflict surface is `cmd/bd/brain_stores.go`).
   This branch is already rebased on the PAI removal (#52): the legacy
   `stores.yaml` fallback is gone, so a machine whose list still lives only at
   the retired legacy path must be given `~/.config/brain/stores.yaml` (`sync`
   writes it from the registry; see mini1 below).
4. **Hold the other machines on `notify` until each has had a reviewed dry-run.**
   Once the registry is adopted (mini0 step 5) any machine running the new binary
   syncs itself on its next command. That is the point, but the first sync of a
   machine deserves a look: on mini1 and the macbook, right after installing the
   binary and **before** mini0 adopts, run `echo notify >
   ~/.config/brain/registry.policy`. Commands then only print `store list changed
   (epoch N -> M): run 'brain stores sync'`. After the reviewed `sync --dry-run`
   and `sync` on that machine, delete the file (policy `auto`).
5. Back up the small mutable state of each machine (no secrets involved):
   `tar czf ~/brain-config-pre-registry.tgz ~/.config/brain ~/.local/bin ~/.pi/agent/bin`
   (a few MB; the wrappers are text; `~/.pi/agent/bin` holds a few large binaries,
   exclude them if the archive is too big).
6. The database login. The first adoption creates four tables and every
   lifecycle verb writes rows through the caller's connection (`brain_app`). Prove
   the privileges on mini0 before the first real run:
   `brain stores reconcile` (read-only) works with any login; for the write, run
   `brain stores reconcile --apply` once and, if it fails with a privilege error,
   grant `CREATE, INSERT, UPDATE, DELETE` on `brain_unified.brain_registry_*` and
   `brain_stores`, `brain_store_prefixes`, `brain_store_prefix_events`,
   `brain_store_aliases`, `brain_unified_config`, `brain_unified_metadata` to
   `brain_app` (the admin login is needed for that grant; it is not needed again).

## 1. mini0 (database host, bridge, firstmate home)

| # | step | command | gate | rollback |
|---|---|---|---|---|
| 1 | install the binary and pin | satellite-config `converge-stores.sh` with the new `BD_WANT`; or copy to `~/.local/bin/` as `bd.real` per the existing layout | `brain --version` prints the new version; `bead-doctor` PASS | restore `bd.pre-<sha>` and the old `.bd-pin.sh` (`restore.sh` in the converge backup dir) |
| 2 | baseline | `brain stores doctor` | `55/55 stores answered a read` | n/a (read-only) |
| 3 | report | `brain stores reconcile` | prints `55 store(s) and N alias(es) are certain; 8 name(s) need a decision`; writes `~/.config/brain/reconcile-report.md` and `reconcile-decisions.yaml` | nothing was changed |
| 4 | decide | edit `~/.config/brain/reconcile-decisions.yaml`: for each of `beads_global commitments feedtack gcnotes guard retro rt TinyKeyboard` set `action: store` (and `name:`) or `action: not-a-store`. Leave any you are unsure of empty. | the file has no guess in it | n/a |
| 5 | adopt | `brain stores reconcile --apply` | `adopted … epoch 1; no machine has reported yet`; `brain stores registry export --json \| jq '.state.epoch, (.entries\|length)'` shows `1` and 55+ | the tables are additive: `BRAIN_REGISTRY=off` disables every code path; nothing is deleted. To start over: `DELETE` the four `brain_registry_*` tables' rows (the tables themselves are harmless) |
| 6 | read the plan | `brain stores sync --dry-run` | only `CREATE link` lines (the 10 missing agent links) and no `UPDATE wrapper`: every existing wrapper is unmanaged (satellite-config's) and is reported `present, not generated by brain: left alone` | n/a |
| 7 | apply | `brain stores sync` | `converged at epoch 1 (55 stores)`; backup dir printed only if something was replaced | `sh <backup>/restore.sh` (links created are just symlinks; `rm` them) |
| 8 | prove | `brain stores verify --probe 3` | `converged` | |
| 9 | roster | write `~/.config/brain/fleet.yaml` (`machines: [{name: mini0}, {name: mini1}, {name: macbook}]`, `ssh:` targets as `~/.ssh/config` knows them) | `brain stores doctor --fleet --machines mini0` converged | delete the file |
| 10 | poller | `brain stores poller print` (read it), `brain stores poller install --dry-run`, then `brain stores poller install` | `brain stores poller status` → `installed=true loaded=true`; after 5 minutes `~/Library/Logs/brain/stores-sync.log` shows no error and `brain stores epoch` shows the applied epoch current | `brain stores poller uninstall` (sync stays available by hand and on every command) |
| 11 | bridge | apply the bridge patch (see the PR) and `launchctl kickstart -k gui/$UID/<bridge label>`; optionally `~/.config/brain/sync.d/10-bridge` containing that kickstart line, `chmod +x` | `bridge_info` shows the stores and `stores_epoch` | restore the bridge's `config.ts` copy |
| 12 | shell hook (optional) | add `eval "$(brain stores shell-hook zsh)"` to `~/.zshrc` | typing a registry store with no wrapper runs it | remove the line |

**The 8 database-only names.** Nothing about them is decided by code. Until a
human sets `store`, they stay unregistered and `doctor --fleet` lists them under
"registered in the database but not in the registry" only if `brain_stores` marks
them `registered=1` (the build marks them `0`).

## 2. mini1 (satellite; the macbook's current relay)

1. Steps 1–2 of mini0 (binary + pin, `brain stores doctor` baseline).
2. **Do not run `reconcile --apply` here.** The registry already exists (mini0).
   `~/.config/brain/registry.policy` should still say `notify` (section 0, step 4).
3. `brain stores sync --dry-run`. Expected: `stores.yaml differs from the registry`
   (mini1's copy carries `/Users/trilliumsmith/...` paths, copied from the
   macbook; sync rewrites it to mini1's own home), the 12 missing agent links, and
   nothing else. mini1's list lives at a retired legacy path with
   `~/.config/brain` empty, and the new binary no longer reads that path: the
   first sync writes `~/.config/brain/stores.yaml` and `stores.env` from the
   registry (the old file is left where it is, untouched).
4. `brain stores sync`, then `brain stores verify --probe 3` → `converged`; then `rm -f
   ~/.config/brain/registry.policy` (back to `auto`).
5. Poller: same as mini0 step 10.
6. Retire the stray bridge process on mini1 (`bun src/server.ts`, started 17 Oct,
   plist disabled): `kill` it. Decide separately whether mini1 keeps a bridge.
7. Gate: from mini0, `brain stores doctor --fleet --machines mini0,mini1` →
   `CONVERGED epoch N … on 2/2 machines`.
8. Rollback: as mini0 (backup dir `restore.sh`; `poller uninstall`).

## 3. macbook (control machine; a client today)

Today the macbook is a **client**: `~/.config/brain/remote.yaml` says
`host: mini1@100.102.238.78`, its wrappers are forwarding wrappers, and its data
path is macbook → ssh → mini1 → SQL → mini0, with mini1's store list a stale copy.
`brain stores sync` works on a client already (it reads the registry **through the
relay host**, which must therefore run the new binary and have the registry), but
the route decision is the captain's. Both options:

### Option A: re-point the client at mini0 (recommended)

One hop, the relay is the database host, and `sync` reads the real registry.

1. Binary and pin (steps 1–2 of mini0).
2. Back up the route: `cp -f ~/.config/brain/remote.yaml ~/.config/brain/remote.yaml.pre-registry`.
3. `brain remote set <user>@100.84.99.99` (mini0; it verifies the host's wrappers).
4. `brain stores sync --dry-run`: the registry is read from mini0; expect
   `unchanged` for the 55 forwarding wrappers and `CREATE` for any store mini0 has
   that the macbook lacks, plus the 4 missing agent links.
5. `brain stores sync`, `brain stores verify` → `converged`; `rm -f ~/.config/brain/registry.policy`.
6. Poller: same as mini0 step 10, using the macbook's `brain` wrapper path.
7. Gate: `brain stores doctor --fleet` from the macbook (local + ssh to the others) →
   `CONVERGED … on 3/3 machines`.
8. Rollback: `mv ~/.config/brain/remote.yaml.pre-registry ~/.config/brain/remote.yaml`,
   `brain stores poller uninstall`.

### Option B: make the macbook a direct machine

No ssh hop at runtime; it talks SQL to mini0:3307 like the minis.

1. Needs on the macbook: `~/.config/beads/credentials` for `100.84.99.99:3307`
   (the `brain_app` password, copied by whoever holds it; never in this repo or a
   ticket), and `~/.config/brain/store-host.env` (host/port/user only).
2. Move `remote.yaml` aside (client mode off). The macbook is now a **direct**
   machine: `brain stores sync --dry-run` plans an `UPDATE` of all 55 forwarding
   wrappers to server-mode wrappers (they carry the remote marker, so they are
   brain's to replace; each is backed up) and a `BEADS_DIR` skeleton for each store
   (project ids come from the database).
3. `brain stores sync`; `brain stores verify --probe 3`; `rm -f ~/.config/brain/registry.policy`.
4. Larger change than A (credentials on a laptop, 55 skeletons, the satellite
   `bead-doctor` wrapper-form check must accept the generated form); do it only if
   the macbook must work when mini1 is down.
5. Rollback: restore `remote.yaml` and `sh <backup>/restore.sh` (puts the
   forwarding wrappers back).

## 4. After all three

* `brain stores doctor --fleet` → `CONVERGED`; wire `brain stores doctor --fleet
  --json` into `bead-doctor` (replace its use of each machine's own registry as
  the truth), through satellite-config.
* Drill, with the captain's approval and a scratch name: `brain stores create
  zz-scratch`, watch it appear on the others without any command, `brain stores
  remove zz-scratch`, confirm the wrappers disappear and `~/data/zz-scratch`
  stays.
* Merge satellite-config's converge scripts to a repo with a remote (open
  decision 6 of the scout report) so the poller's presence is also checked there.

## Things deliberately not done

* No machine, plist or database was touched by this change.
* mini2 / mini3 are retired and not in the default roster; add them with
  `--machines` or the roster file if they ever return.
* lnx, hass, vps01 and the phone were not inspected; if any runs brain, add it to
  `fleet.yaml`.
