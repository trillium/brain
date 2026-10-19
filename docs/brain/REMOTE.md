# Client mode: run brain on another machine (`brain remote`)

The brain stores live in one Dolt database on one computer, the **brain host**.
Any other machine can be a **client**: `task create …`, `inbox list`,
`brain search …` and every other store command run against the host,
transparently, with no database or server on the client.

```
 client                                           brain host
 ──────                                           ──────────
 task create "ship it"                              ~/.local/bin/task  (the host's own wrapper)
   └─ wrapper: BRAIN_REMOTE_STORE=task brain …        └─ bd → Dolt on 127.0.0.1:3307
        └─ brain binary sees remote.yaml
             └─ ssh host  exec /bin/sh -c '…' task create 'ship it'
```

Only ssh crosses the network. The Dolt port is not exposed, bound, tunnelled or
reconfigured: the host keeps listening wherever it already did, and clients
never connect to it. (This is why client mode does not depend on, or worsen,
the open finding about the host's Dolt server accepting passwordless logins on
all interfaces.)

## What is preserved

| | |
|---|---|
| arguments | passed as positional parameters to a fixed `sh -c` script; each is single-quoted, so spaces, apostrophes, `$`, `*`, newlines and empty arguments arrive byte-for-byte. Nothing is re-parsed by the host's shell. |
| stdin | relayed, so `-f -`, `--body-file -` and pipes work |
| TTY | a pseudo-terminal is requested only when both stdin and stdout are terminals; pipes and redirects stay clean |
| exit code | the host command's status; a signal-killed ssh becomes `128+signal` |
| host behaviour | the command runs through the host's own wrapper, so store pins, create-emit hooks and routing apply exactly as if you were typing at the host |
| latency | one multiplexed ssh connection (`ControlMaster=auto`, `ControlPersist=10m`) is shared by every call, so a call costs one round trip plus the command, not a new handshake |

Not preserved: **paths are host paths**, except the local file flags below.
`--body-file` and `--description-file` on `create` and `update`, and `--file` on
`comment` and `note`, are read on the client and sent as stdin. `create --file`,
`batch --file` and `comments add --file` are rejected before anything is sent.
Other path flags are not rewritten: `task create -f body.md` reads `body.md` on
the host. Use `-f -` and pipe the file in.

## Set up a client

Prerequisites: ssh key login from the client to the host (`ssh -o
BatchMode=yes <target> true` must succeed without a prompt), and a `brain`
binary on the client. Build it with `make build` and copy `bd` to
`~/.local/bin/brain` (or any directory on `PATH`).

```sh
brain remote set me@my-host.example.ts.net   # checks the host answers, then saves
brain remote install                          # one wrapper per store in the host's registry
brain remote status                           # host, reachability, latency, versions
```

`brain remote set` writes `~/.config/brain/remote.yaml`:

```yaml
host: me@my-host.example.ts.net
bin_dir: $HOME/.local/bin   # where the host keeps its wrappers (optional)
connect_timeout: 5          # seconds (optional)
ssh_options: []             # extra ssh -o options, e.g. ["IdentityFile=~/.ssh/brain"]
```

`brain remote install` asks the host for `brain stores list --json`, keeps the
stores that have a wrapper in the host's `bin_dir`, and writes a small wrapper
for each into the directory of the `brain` binary (`--dir` to choose another).
Re-run it whenever the host gains a store; `--prune` removes wrappers for
stores the host no longer has; `--also name` adds a host wrapper that is not a
registry store; `--dry-run` shows what would change.

Before it writes anything, `brain remote install` (unless `--dry-run`) and
`brain remote set` (unless `--no-check`) run `brain stores rewrite-wrappers` on
the host. That rewrites only the `BEADS_DIR` line (and `BRAIN_KNOWLEDGE_ROOT`
if it holds an absolute home path) of each existing store and alias wrapper to
the `$HOME`-relative form, keeping every other line, and never creates a
wrapper that is missing. It then probes each store through its wrapper. If any
store cannot resolve a database, the command exits non-zero and lists them
under `FAILING STORES`; `set` does not save the host in that case.

Safety rules of the installer: it only ever replaces files it generated (they
carry a `# brain-remote-wrapper:` marker; `--force` overrides for other files),
it never writes a command called `bd`, it skips the name of the brain binary
itself, and it prints a note when a wrapper shadows, or is shadowed by, another
command on `PATH` (for example Taskwarrior's `task`). Put the wrapper directory
**after** anything you want to keep winning.

## Add another client machine

1. Make sure that machine can ssh to the host without a password prompt.
2. Install the `brain` binary there.
3. `brain remote set <ssh-target>` then `brain remote install`.

Nothing changes on the host. A client that is also the host's own account (or
the host itself) simply has no `remote.yaml` and runs locally.

## Everyday use

- `BRAIN_REMOTE=off task list` runs one command against a local database.
- `BRAIN_REMOTE_HOST=other@host brain status` points one command at another host.
- `brain remote disconnect` closes the shared connection; the next call reopens it.
- `brain remote unset` removes the configuration. Wrappers stay but have no host.
- Unreachable host: the call fails with `brain: cannot reach the brain host …`
  and ssh's exit status 255; nothing ran on the host. The connect timeout is
  `connect_timeout` seconds (default 5). There is no offline mode: clients need
  the host awake and reachable.
- `brain remote …` itself is the only client-side command; every other
  invocation, including `--help` and `--version`, is answered by the host.

## When the host moves

`brain host promote` moves the host role to another machine (see
[`HOST_PROMOTE.md`](HOST_PROMOTE.md)). A client that was offline for the move, or
never registered with the host, does not need to be told: the old host answers its
next command with where the brain went, the client checks that the new machine
really is the host the old one named, rewrites `remote.yaml`, runs the command
once more and prints one line saying it re-pointed. If it cannot verify the move
it changes nothing and says why (exit 76).

## What a call costs

Measured from a Mac mini to the host's Mac over Tailscale (the host was busy,
load average ~300, which slows every command it runs):

| | |
|---|---|
| a fresh `ssh host true` handshake | ~340 ms |
| `ssh host true` on the shared connection | ~70 ms |
| the host running `task show <id>` itself | ~430–780 ms |
| `task show <id>` from the client | ~720–890 ms |

The client adds the shared-connection round trip (~70 ms) plus ~100 ms to start
the `brain` binary; the rest is the host doing the work it would do for a local
call.

## Limits

- The login shell on the host must be sh-compatible for the outer command
  (bash, zsh, dash); everything after that runs under `/bin/sh -c`.
- A local file flag that is sent as stdin takes the place of piped input, and
  only one such file can be given per command. Other path flags read the host's
  files. Interactive editors (`bd edit`) work when a TTY is allocated.
- The host's wrapper directory must be reachable by absolute path; the forward
  prepends it to `PATH` for the command, so wrappers that `exec bd` resolve even
  though a non-interactive ssh session has a minimal `PATH`.
