# Brain de-PAI migration (v0.5.0)

Captain decision: PAI naming is dropped. Layer 1 (daemon rename) is done;
this is layer 2: full de-PAI migration in the `trillium/brain` fork,
shipped as fork version `v0.5.0`
(combined token `<upstreamTag>+brain.0.5.0`).

## What changed

| Before | After (canonical) | Compat |
|---|---|---|
| `~/.config/pai/stores.yaml` | `~/.config/brain/stores.yaml` | legacy path kept as symlink; loads union-merge both files (canonical wins) so diverged copies lose nothing |
| `~/.config/pai/stores.env` | `~/.config/brain/stores.env` | legacy path kept as symlink; file exports both `BRAIN_*` and deprecated `PAI_*` aliases |
| `PAI_STORE_<NAME>` | `BRAIN_STORE_<NAME>` | old names still exported with deprecation note |
| `PAI_STORES_LIST` | `BRAIN_STORES_LIST` | old name still exported with deprecation note |
| `~/.claude/PAI/MEMORY/WORK` (ISA exfil default) | `~/.claude/brain/MEMORY/WORK` | `BRAIN_ISA_EXFIL_ROOT` override unchanged; when canonical is absent but legacy exists, legacy is used |
| `PAI federation` / `PAI shared-server` wording in help | `brain federation` / `brain shared-server` | scheduler `FAILING STORES:` contract and `__registry__` sentinel unchanged |
| ISA format reference `PAI/DOCUMENTATION/IsaFormat.md` | `IsaFormat v2.7` (format originated as PAI Algorithm v6.4+ ISAs) | no behavior change |

## Transition guarantees

- **Non-destructive.** Saving the registry or env writes the canonical file
  first, then converges the legacy path to a symlink only when safe (legacy
  missing, or legacy regular file migrated after its contents were read).
  Existing regular files are never deleted without their contents already
  living at the canonical path.
- **Old reads keep working.** Registry loads union-merge
  `~/.config/brain/stores.yaml` and `~/.config/pai/stores.yaml`
  (canonical wins on conflict), so even a diverged legacy copy loses no
  entries — the next save persists the union and converges the legacy path
  to a symlink. Transfer-verb loads merge the same way. ISA exfil prefers
  the canonical root but falls back to the legacy directory when it exists
  and the new one does not.
- **Old env names keep working.** `brain stores env` (and every
  `brain stores create/rename`) emits both `BRAIN_STORE_*` /
  `BRAIN_STORES_LIST` and deprecated `PAI_STORE_*` / `PAI_STORES_LIST`.
  New code should use the `BRAIN_*` names; the `PAI_*` aliases will be
  removed in a later release.

## Operator actions

1. Run `brain stores env` once after upgrading to regenerate
   `~/.config/brain/stores.env` and converge the legacy symlink.
2. Update shell wrappers / scheduler env to source the canonical file and
   use `BRAIN_STORE_*` / `BRAIN_STORES_LIST`. The deprecated names still
   work during transition.
3. Optional: `mv ~/.claude/PAI/MEMORY/WORK/* ~/.claude/brain/MEMORY/WORK/`
   once you have verified renders land at the new root, or pin
   `BRAIN_ISA_EXFIL_ROOT` explicitly.
4. Verify: `brain stores doctor` should report the same stores as before.

## Version

Fork semver rule: `<upstreamTag>+brain.<forkVersion>`.
This migration ships as `brain/v0.5.0` (e.g. `1.1.0-rc.1+brain.0.5.0`).

```sh
make brain-release BUMP=minor   # tags brain/v0.5.0 locally
make build && cp bd ~/.local/bin/bd
git push origin brain/v0.5.0
```
