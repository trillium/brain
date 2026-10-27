# Running brain on another machine

The brain stores live in one Dolt database on one computer, the **brain host**.
Any other machine reaches them from a **client** setup, so `task create …`,
`inbox list`, `brain search …` and every other store command run against the
host with no database or server on the client.

There are two transports. Use the first; the second is the fallback.

| | Direct (default) | ssh client mode (fallback) |
|---|---|---|
| path | client `bd` → MySQL to `host:3307` | client `brain` → ssh → host `sh` → host wrapper → host `bd` → Dolt |
| needs | the Dolt port reachable, a `brain_app` password | ssh key login to the host |
| per-call cost | one TCP dial plus the query | an ssh round trip plus a full `bd` process on the host |
| unreachable host | fails in about 500 ms (`Dolt server unreachable at host:port`) | ssh connect timeout (5 s default) |
| set up with | `store-host.env` + credentials + `brain registry sync` | `brain remote set` + `brain remote install` |

Decision **decision-4l0** (2026-10-27) made direct the transport for every
tailnet machine: clients connect straight to the host's Dolt server as
`brain_app`, and ssh client mode stays only for a machine that cannot reach the
Dolt port. This replaces the earlier rule that "only ssh crosses the network",
whose stated reason (the host's Dolt accepted passwordless logins on all
interfaces) no longer describes the deployment: the host's server now requires
the `brain_app` password.

## Direct transport

```
 client                                        brain host
 ──────                                        ──────────
 task create "ship it"
   └─ ~/.local/bin/task  (server-kind wrapper)
        BRAIN_NO_EXFIL=1, BEADS_DOLT_SERVER_HOST=<host>
        └─ bd ── MySQL, user brain_app ──────▶ dolt sql-server :3307
```

Each client holds three pieces of local configuration:

1. `~/.config/brain/store-host.env`, sourced by every wrapper below its own pins:

   ```sh
   export BEADS_DOLT_SERVER_HOST=100.84.99.99
   export BEADS_DOLT_SERVER_PORT=3307
   export BEADS_DOLT_SERVER_USER=brain_app
   ```

2. The password, in `~/.config/beads/credentials` (mode 600), in a section keyed
   by the endpoint. It is never in `store-host.env`, a wrapper, a log or a
   commit:

   ```ini
   [100.84.99.99:3307]
   password=…
   ```

   Copy the section from a machine that already has it. `BEADS_DOLT_PASSWORD`
   overrides it for one call.

3. Wrappers from the registry: `brain remote unset` (if the machine was an ssh
   client), then `brain registry sync`. A machine with no `remote.yaml` is a
   direct machine, and `sync` writes a server-kind wrapper per store.

### Who writes the markdown

The markdown under `~/data/<store>/` is a derived view of the database, and only
the host writes it. `brain registry sync` notices that the machine's Dolt host is
another machine (the `BEADS_DOLT_SERVER_HOST` in `store-host.env` is not
loopback, this machine's hostname or one of its own addresses) and adds
`export BRAIN_NO_EXFIL=1` to every server-kind wrapper, so a client never grows a
second copy. The line follows the host: if the machine becomes the host, the next
sync removes it.

The host keeps the markdown current with `brain host render`, which runs
`<store> render-all` through each wrapper in `stores.yaml` (a failing store is
reported and the rest still render). It refuses to run on a machine whose Dolt
host is elsewhere. A launchd job runs it every five minutes:

```sh
brain host render-job install     # com.brain.host-render; also declares it part of the host role
brain host render-job print       # the plist, nothing written
brain host render-job uninstall
```

Because `install` declares the job with `brain host service add`, it moves with
the host role on `brain host promote`. `brain host promote` also installs and
loads the job on the new host itself if the old host did not have it, and
declares it in `host.yaml`; running promote again changes nothing further.

### Failing fast

`bd` dials the server with a 500 ms TCP probe before any MySQL traffic. A direct
client therefore fails in half a second when the host is down or off the
tailnet (`internal/storage/dolt`: `TestNew_UnreachableRemoteHostFailsFast`),
instead of waiting on an ssh connect timeout.

### Not yet done

Follow-ups, not part of this transport change: a `brain stores doctor` check
that every machine's Dolt host equals the one machine claiming `role: host`; retiring the
satellite-config wrapper generators in favour of `brain registry sync`; the local
spool and single shipper that stop detached hook processes from blocking during
an outage.

# ssh client mode (`brain remote`)

Use this only where the Dolt port is not reachable. The rest of this page
describes it.

```
 client                                           brain host
 ──────                                           ──────────
 task create "ship it"                              ~/.local/bin/task  (the host's own wrapper)
   └─ wrapper: BRAIN_REMOTE_STORE=task brain …        └─ bd → Dolt on 127.0.0.1:3307
        └─ brain binary sees remote.yaml
             └─ ssh host  exec /bin/sh -c '…' task create 'ship it'
```

In this mode only ssh crosses the network: the Dolt port is not used by the
client at all.

## What is preserved

| | |
|---|---|
| arguments | passed as positional parameters to a fixed `sh -c` script; each is single-quoted, so spaces, apostrophes, `$`, `*`, newlines and empty arguments arrive byte-for-byte. Nothing is re-parsed by the host's shell. |
| stderr | relayed, minus the `ControlSocket … already exists, disabling multiplexing` and `mux_client_*` lines ssh logs when concurrent calls race for the shared connection; those calls still succeed, so no grep filter is needed. Real ssh errors (host key, auth, name resolution) still come through. |
| stdin | relayed, so `-f -`, `--body-file -` and pipes work |
| TTY | a pseudo-terminal is requested only when both stdin and stdout are terminals; pipes and redirects stay clean |
| exit code | the host command's status; a signal-killed ssh becomes `128+signal` |
| host behaviour | the command runs through the host's own wrapper, so store pins, create-emit hooks and routing apply exactly as if you were typing at the host |
| latency | one multiplexed ssh connection (`ControlMaster=auto`, `ControlPersist=10m`) is shared by every call, so a call costs one round trip plus the command, not a new handshake |

Not preserved: **paths are host paths**, except for the file flags below. Any
other path argument is resolved on the host, so pipe the file in with `-f -`.

The client reads a local file itself for these flags and sends its contents on
stdin (a missing local file is reported on the client):

- `--body-file` and `--description-file`, on any command.
- `--file` and `-f`, on `comment`, `note` and `comments add`. For `comments add`
  the file text replaces any typed text and is sent as the comment body.

`create --file` (markdown batch) and `dep --file` (edge list) still take host
paths. Forwarding two local file flags in one command is refused before anything
is sent.

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
- `BRAIN_REMOTE_DEADLINE=<seconds>` caps one forwarded call; on expiry the
  ssh process group gets SIGTERM, then SIGKILL after 2s. Unset means no limit.
  SIGTERM, SIGINT and SIGHUP sent to the client are relayed to that group.
- An exported `BEADS_DIR` that names a local beads project (a directory with
  project files) wins over `remote.yaml`: the command runs against that store
  here and is not forwarded. An unset or non-project `BEADS_DIR` still forwards
  reads and store commands.
- A bare `brain create|update|close|delete|import` (or an alias such as `new`
  or `done`) with `BEADS_DIR` unset and a `.beads` workspace found from the
  working directory runs locally against that workspace and is not forwarded.
  It is refused otherwise, when `BEADS_DIR` is set but is not a local project or
  when no workspace is found: `brain: no beads workspace here; set BEADS_DIR or
  BRAIN_REMOTE=off`, exit 2, nothing sent to the host. Nothing here names a store
  the host can honor, and the host would otherwise answer through its own
  `brain` wrapper, so the write would land in that wrapper's default store. Reads
  (`brain list`, `brain show …`) still forward from anywhere, and store commands
  (`task create`) forward every verb.
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
- File paths that are not in the list under "What is preserved" are read from
  the host. Interactive editors (`bd edit`) work when a TTY is allocated.
- The host's wrapper directory must be reachable by absolute path; the forward
  prepends it to `PATH` for the command, so wrappers that `exec bd` resolve even
  though a non-interactive ssh session has a minimal `PATH`.
