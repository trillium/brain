# Brain stores across machines: a store's command runs where its database lives

**Status:** design only. Nothing here is implemented, cut over, re-pointed,
rebound, or edited on any machine. Every fact about the serving machine in this
document was read over ssh or probed as a read-only TCP/SQL handshake; the one
SQL-query probe was a read-only `select` against `*.*` and changed nothing.
**Direction:** the captain-approved vision, `VISION.md`, three sections:
§"One federation, one surface" ("*The federation converges on one database on
one machine; a subordinate copy is a future capability, not a current one.*"),
§"A silent failure is the defect class" ("*A database that cannot be reached
is a loud failure, never an empty result.*"), and §"A programmatic surface,
deliberately opened". The "one database, one machine, everywhere" direction
is H-8 of the vision review, decided in the captain's own words:
*"Yes - this is the intent. There should be a way to make a subordinate copy
too, but that can be a future feature."*
Predecessor designs: [brain-single-database.md](brain-single-database.md)
(the unified database and the namespace model),
[brain-cutover-runbook.md](brain-cutover-runbook.md) (one store re-pointed,
end to end, on copies) and `WHAT_BRAIN_ADDS.md` §2.

## The requirement, in the captain's words

A store's command on one machine runs the actual command on the machine that
hosts the store. His example: *"machine A holds the dolt store, config on
machine B, and `task create` / `task list` come from machine A."* Config-only
machines are real: this design landed on one. The machine it was written on
(`mini0`) carries a `~/.config/brain/stores.yaml` registry full of store
entries pointing at scratch probe directories — the store *directories*
themselves, the ones the federation runs on, live on the serving machine.
A machine that described stores could never run one of them against the
database that owns them.

**The one rule everything below serves:** a store is *reached*, not *copied*.
There is no replica, no sync fallback, no stale local view the command may
quietly drop to. When the host cannot be reached the command is a loud refusal
that names the host and the store; it never returns an empty result, never
reads anything else, never writes down that it "served" anything.

## What is on the ground (verified, not assumed)

These facts are load-bearing. Each was read or probed on 2026-10-09 and the
evidence is named inline, because two claims in current tooling are stale.

| Fact | Evidence |
|---|---|
| The serving machine's Dolt SQL server **already** answers on every interface: `-H 0.0.0.0 -P 3307`. | `ps` on the serving machine; launchd `com.pai.dolt-server.plist` |
| The routing shim's comment ("SQL :3307 binds 127.0.0.1 only and root has no password") is **stale**. The rebind it calls a prerequisite happened on or around 2026-10-05 — the prior plist (`com.pai.dolt-server.plist.bak-20261005-015609`) shows the `127.0.0.1` bind it replaced. | serving machine's `~/Library/LaunchAgents/` |
| SQL port 3307 on the serving machine is TCP-*reachable from this client machine* over the tailnet. | `nc -z 100.74.138.74 3307` from mini0: connect succeeds |
| A password-less `root` from a network client is **already refused** (`Error 1045 Access denied for user 'root'`). `mysql.user` shows only `root@'localhost'`, `__dolt_local_user__@'localhost'`, plus `brainsync@'%'`. | `dolt sql` probe from mini0, read-only |
| `brainsync@'%'` carries the SQL privilege kitchen sink — `SHUTDOWN`, `FILE`, `CREATE USER`, `DROP`, `SUPER` — on `*.*`. Its holder and password were **not** determined by this design. | `SHOW GRANTS` read-only over ssh |
| The serving machine's Dolt is 1.83.1; its server does not support TLS (a client's default TLS handshake is refused with "server does not support TLS"). | `dolt sql` probe from mini0 |
| The remotesapi port 3310 serves the tailnet. | serving machine's `netstat`; shims on both machines |
| Every store wrapper, on serving and client machines alike, sources `~/.local/bin/.beads-route.sh`, which probes the remotesapi port and exports `BEADS_ROUTE=external\|local\|disabled`, with `external-first` the standing mode. | shim text, identical byte-for-byte on both machines |
| The shim's fallback is the opposite of the vision: remote unreachable → `BEADS_ROUTE=local`, `exec bd` proceeds against whatever the local machines still holds, printing `using local (reconcile later)` as if that were the normal path. | shim text |
| Sync today is a per-store Dolt remote (`macbook`) plus `beads-reconcile.sh` (`--pull`/`--push`): chunk transport, eventual consistency, manual. | `beads-reconcile.sh` text; store `.beads/` remotes |
| A store's connection is already addressable: `.beads/metadata.json` carries `dolt_server_host`, `dolt_server_port`, `dolt_database`, and the fork reads `BEADS_DOLT_SERVER_HOST`, `BEADS_DOLT_PASSWORD` and the server TLS flag from env and from an INI-style credentials file keyed by `[host:port]` (`internal/storage/dolt/open.go`, `store.go`, `internal/configfile/credentials.go`). | source at head |
| Store identity and location are already deliberately recorded: `~/.config/brain/stores.yaml` (managed by `brain stores add/remove`, single write path for wrapper regeneration). | registry text on both machines |
| The unified database's namespace model — narrow reads, storeless-mint refusal, prefix-ownership refusal — is all SQL-level and knows nothing about machines. | `internal/storage/issueops/unified_namespaces.go` |

One sentence on why the stale comment matters: the design work the shim
deferred ("until rebind + auth exist") is *half done already* — the rebind
happened, and the remaining half is auth. Every transport option below is
priced against that, not against a future.

## The three transport shapes

Each shape answers: what it means, what it gets you, what it costs, and what
the host must give it.

### Option A — ssh command forwarding: the command runs on the host

**What it means.** The client's store wrapper no longer `exec bd` locally. It
runs the *host's own wrapper* on the host:

```sh
exec ssh serving-host ~/.local/bin/task "$@"
```

stdout, stderr and exit status come back; the machine that hosts the database
executes every verb, on its own binary, against its own server, exactly as a
sitting-at-the-host session would.

**What it gets you.**

- Zero new network surface: transport is the ssh that already carries admin
  traffic; session keys, reachability and identity come free.
- Zero SQL-side change: no users, no credentials, no credentials files, no TLS
  story. `root@'localhost'` keeps answering local connections only.
- Zero fork change: this is purely a wrapper emission change
  (`brain stores`), and the wrapper already exists on the host.
- The client's `bd` version can never disagree with the host's: the host runs
  its binary. No schema-compatibility window to police.
- Failure is `ssh`'s own failure mode — "connection refused by that host", or
  "host down" — in the machine-readable text ssh already emits.

**What it costs.**

- Every invocation pays a fresh ssh session. Even with ControlMaster reuse
  (which turns this into a client-side config, not a daemon) there is
  reconnection latency on cold paths, and hooks/scripts that iterate stores
  pay it per store.
- **Locality breaks.** Everything executes in the host's environment: markdown
  exfiltration renders into the *host's* tree, not the machine that typed the
  command, so a machine asking `task list` gets rows but never keeps a
  readable view of its own. Hooks run on the host; an interactive editor, a
  pager, colour and completion live where the server does until forwarded.
- Every verb's exact environment (`BEADS_KNOWLEDGE_ROOT`, pin exports,
  `--wide`, rerun flags) has to survive ssh's argv/env boundary; shell-level
  env a captain sets on the client does not ride along.
- One ssh identity is "the client machine" at best. Naming *which store, which
  invocation* failed is the wrapper's job, not any transport primitive.

**What the host must give it.** Nothing new — the wrappers exist, ssh exists.
If identity discipline is wanted, one dedicated non-login ssh account per
client machine on the host, but that is optional refinement, not a
requirement.

### Option B — ssh tunnel: the client speaks SQL to a forwarded local port

**What it means.** The client's `bd` keeps running locally and speaks the
MySQL protocol, but through an ssh forward instead of a socket:

```sh
# per-command (cold), or under a supervised launchd KeepAlive (steady)
ssh -N -L 13307:127.0.0.1:3307 serving-host &
BEADS_DOLT_SERVER_HOST=127.0.0.1 BEADS_DOLT_SERVER_PORT=13307 bd "$@"
```

The server on the host sees a connection from `127.0.0.1` — the loopback it
already accepts.

**What it gets you.**

- The host's server can stay exactly as configured — nothing on the host
  changes if you ride loopback.
- The client runs its own binary: markdown renders into the client's own
  tree, hooks run locally, `--wide` behaves as a local read across the SQL
  layer, and version skew stays a normal rules-of-the-window concern like a
  fork upgrade.
- Failure is at the SQL layer, which the loud-refusal text can name properly
  (store, host, database behind the tunnel) — *when the tunnel is healthy*.

**What it costs.**

- **Something must hold the tunnel up.** Per-command is a fresh ssh session
  per invocation (Option A's cost, paid again); steady-state is a supervised
  launchd job per client machine — a process, a teardown, a reconnect
  discipline, and a per-host local port assignment that grows with the
  federation.
- **Ambiguous failure.** "Tunnel down locally" reads as local
  `ECONNREFUSED` while the truth may be the intact tunneled server refusing,
  or the intact tunneled host unreachable — three different causes under one
  unexplainable errno unless the wrapper adds another layer of
  discrimination. The loud-refusal contract cannot be met by an errno that
  points at the wrong machine.
- **Authentication is the wrong shape.** From the server's point of view the
  connection is `root@'localhost'` — so either every client machine shares
  the server's root (refused by design below), or the host still needs
  per-consumer SQL users, which erases Option B's "nothing changes on the
  host" advantage. Whichever you pick, you have Option C's auth cost plus a
  tunnel-supervision cost.
- All stores on one host share one tunnel; a wedged connection wedges the
  federation on that host with the worst error of the three options.

**What the host must give it.** Nothing if the shared-root shape is accepted
(it is not — see §Security), or otherwise the same per-consumer SQL accounts
Option C requires. Supervision is a client-side launchd job.

### Option C — direct SQL over the private network: rebind already, then authenticate

**What it means.** The client's `bd` connects straight to the serving
machine's Dolt SQL server over the tailnet. The bind is *already* `0.0.0.0`
(see the ground table): the remaining work is identity — named per-consumer
SQL accounts on the server, credentials per client machine, and an accepted
position on plain MySQL-protocol over WireGuard-encrypted tailnet transport
(§Security). The wrapper pins the connection points exactly as the shim's own
NOTE anticipated when it deferred SQL routing; the fork's connection-picking
layer already reads `BEADS_DOLT_SERVER_HOST`/`PORT`/password/TLS.

**What it gets you.**

- No extra process anywhere: no per-command session, no launchd tunnel, no
  supervision. One TCP connection inside `bd`, pooled like every local
  connection today.
- **Failure names the host, honestly.** "refused" is the server refusing
  (host up, port closed); "no route to host" or timeout is the host or the
  network down; auth-denied names the SQL account. Three distinct, directly
  nameable causes — the only shape of the three where the errno already says
  the right thing.
- Attribution: one SQL account per consumer machine, so logs and refusals can
  name *which machine* asked, and a future audit story can too.
- The client runs its own binary — same locality win as Option B.
- Small diff to land: wrapper emission + shim, not a new transport layer; the
  code path already exists in `internal/storage/dolt/open.go`.

**What it costs.**

- **The auth work is real and is the host's**: `CREATE USER`, per-consumer
  passwords, least privilege, and the standing account hygiene below. This is
  the price of having a network boundary at all.
- The MySQL protocol runs without TLS today (serving Dolt 1.83.1 has no TLS;
  the client's default handshake is refused). Acceptable *inside the tailnet*
  — WireGuard is the encryption layer — and only there; see §Security.
- Version skew between client and host binaries is now possible and has to
  stay within the SQL-schema window a normal fork upgrade already lives in.
- Tests and temp stores that pre-set `BEADS_DOLT_SERVER_HOST` themselves
  keep winning only if the routing block assigns *conditionally* (the
  `: "${VAR:=…}"` shape the shim already uses, and which
  `~/.config/pai/remote.env` states as the rule: pre-set env always wins
  over defaults) — while the unconditional pin exports (BEADS_DIR,
  BD_NAME) stay ordered as the shim's NOTE requires (see §Transition).

**What the host must give it.** SQL identities and a credentials story. No
rebind — it is already listening to the tailnet. (If `0.0.0.0`-on-a-Mac is
ever judged too broad of a hint about what is listening, the alternative
binding — the tailnet interface's specific IP — is a one-line plist edit, and
this design takes no position on it without the captain.)

### The marked recommendation: Option C

**Recommendation: Option C — direct SQL over the tailnet, authenticated
per-consumer.** Standing captain preference from `captain.md`: present
options with their consequences, then give a recommendation and say why.

Why C and not the others:

1. **Failure semantics decide it.** The vision's loudest rule is about
   unreachable: the command must refuse loudly, naming the host. Only Option
   C's transport leaves the cause distinguishing to the network (refused /
   no route / auth fail) rather than burying it under an extra layer. Option
   B cannot meet this rule without inventing a second discrimination layer
   on top of an errno that points at the wrong machine; Option A's ssh
   failure text does not speak in "this store's database is unreachable"
   terms — it speaks in "ssh" terms, which the wrapper would then translate,
   i.e., a second discrimination layer by another name.
2. **Nothing to build on the client.** The connection knobs are already in
   the fork (host/port/password/TLS, read from env and the credentials
   file). Option A also has a tiny client diff, but it pays for that with
   locality: markdown and hooks land on a machine the operator may not even
   be sitting at — the one property the federation's "markdown is a reader's
   view" promise lives on.
3. **Attribution.** Per-consumer SQL accounts give the refusal text and the
   server log the same answer to "who asked"; Option A gives ssh's identity
   only if a per-client ssh account exists, and Option B, resting on
   loopback, gives the server no way to tell clients apart at all without
   the same Option-C-shaped accounts anyway.
4. **The two other shapes are not wasted — they stay in defined roles.**
   Wrapper-forwarding (A) is the **bootstrap and admin channel**: the account
   that a brand-new client machine asks for its SQL identity has to exist
   before any SQL connection is accepted, and asking happens over the ssh
   that already works (§Transition, step 2 — where the first identity is
   minted). Tunneling (B) remains the
   **off-tailnet exception**: a client machine outside the tailnet has no
   direct route, and ssh-tunneled SQL is the documented shape *only* for that
   machine, not the fleet path. Recommending C for the tailnet does not
   authorize B outside a named exception; it just doesn't make B the road.

## Where the database lives, and how a wrapper learns a store is remote

**One database, on one machine, everywhere.** The design's answer to *where*
resolves to what the captain already confirmed: the serving machine hosts one
Dolt server (`com.pai.dolt-server`), that server hosts the merged
(`brain_unified`) database, and every other store's own database on that same
server is a pre-unification transitional fact, not a second shape to support.
No subordinate copy exists in this design; the capability is the vision's
future feature, not a knob added tonight. So the merged database lives on one
host and every other machine is a client of it, with no copy of its own.

**Registry, not probes.** A store's identity is already deliberately
recorded; where its database lives is the same kind of deliberate record.
`~/.config/brain/stores.yaml` gains a machine field per store, in the
registry's existing shape (path + about + … ):

- `location: local` — the default. Store's database is on the machine running
  the command; today's behaviour, byte for byte. Every existing store reads
  as `local` with no edit at all; the field is additive.
- `location: remote` + `host: <tailnet-name-or-ip>` — the machine that hosts
  this store's database. Exactly one host per store; the design names no
  failover, no quorum, no replica — a subordinate copy is a future feature.
- Wrappers learn it through the existing single write path:
  `brain stores` regenerates `stores.env` and the store wrappers from this
  same file (`WHAT_BRAIN_ADDS.md` features A-2 and A-3), so the machine
  field rides the read the wrappers already make. A wrapper never
  discovers a host by probing: the probe's job (§Refusal behaviour) is to
  answer *is it up*, never *where is it*.
- The store's own `.beads/metadata.json` remains the concrete connection
  record (`dolt_server_host`, `_port`, `_database`) as it is today. When a
  store moves, the move is recorded in the registry and then stamped into
  metadata.json by the same step (§Transition), so the two records never
  disagree. The registry is the *identity* of the machine; metadata.json is
  the *coordinates*, and the regen step is the single place that writes both.

This ordering matters: the vision says stores are added deliberately, never
noticed. Location is the same class of decision as identity — a registry
write, not a discovery mechanism. Nothing sniffs the tailnet for "a Dolt
server and mint it as mine".

## Refusal behaviour: unreachable is loud, and never a fallback

Every store command, on every machine, resolves a destination (see §Where the
database lives) and then must be able to reach it. If it cannot, the command
is a loud refusal. Exact text, matching `stores doctor`'s existing
conventions (`exit 1`, `FAILING STORES:` precedent) and the
`RefuseStorelessMint` refusal style (reason, then the way out):

```
task: database host unreachable — refusing rather than serving a stale or empty view
  store:    task                 (registry: ~/.config/brain/stores.yaml)
  host:     <serving-machine>:3307   (the store's serving machine, per the registry)
  detail:   connect: no route to host
  way out:  confirm the serving machine is up (`nc -z <serving-machine> 3307`),
            or update the registry entry if the store moved.
  what did not happen: nothing was read, nothing was written, no local
  directory was consulted, no data was returned.
```

**What the command never does:**

- never returns an empty result ("no beads found") when the real answer was
  "the host is down" — those are different outcomes and the vision refuses to
  let them look alike;
- never falls back to a local directory, a cached copy, a macbook-remote
  pull, or a replica — the federation has no replica in this design and the
  pull is *transition-only* (§Transition) and is never an automatic response
  to unreachability;
- never proceeds with `BEADS_ROUTE=local` on an unreachable remote: today's
  shim behaviour is explicitly the *transitional* shape (§Transition) and the
  steady-state rule is the opposite;
- never prints a warning instead of refusing — the vision's rule is that a
  warning is quieter than a refusal and both are failures here;
- never returns exit 0.

Reachability and authentication are two different loud failures; both refuse.
The probe that finds them differs by layer. The shim's nc probe (it already
knows how) distinguishes "connection refused" (host up, port closed) from
"no route"/timeout (host or network down) and refuses with the failure text
above, naming which — it never sets `BEADS_ROUTE=local` and proceeds. Auth
failure, though, is only knowable after a real SQL handshake, so it is named
by `bd`'s own connection path — the refusal there names the SQL account and
the host the registry holds for the store, and refusing outright is equally
obligatory (an auth failure is a reason to name the account, not a reason to
fall back).

`stores doctor` is the federation-wide shape of the same rule: it already
probe-stores and exits 1 with a `FAILING STORES:` line, which is exactly the
multi-store surface of this refusal. Nothing new is invented; the definition
of "failing" simply becomes stricter — an unreachable remote store is
failing, not skipped.

## Wide and narrow reads from a client, and all-sight during transition

Reads run on the host's database through SQL; nothing about the transport
changes what a read means:

- **Narrow read from a client** (`<store> list`, `search`, `ready`, `count`,
  `statistics`) — the wrapper pins one namespace (`BD_NAME`) and the read
  scopes to that namespace's record in whatever database the store is served
  from — its own pre-merge database during the transition, or the merged
  `brain_unified` record afterwards — exactly as
  `brain-single-database.md` specifies. The client's wrapper emits the pin;
  the server answers the query. The transport does not change the rule.
- **Wide read from a client** (`--wide`, `search --federated`) — every read
  the merger defines as wide costs *one query* on the unified database,
  which is one host. On a client that query goes over SQL to the same host;
  `--wide` on `mini0` and `--wide` on the serving machine are the same read
  to the same rows. The `--wide` flag ships with the federation today and
  needs no transport-native changes.

**All-sight during transition** is the one honest wrinkle. Until every store
is merged into `brain_unified`, a client's wide read can only see beads that
already live there; stores still local to another machine are not inside
*any* SQL read from a client, however the transport behaves. The design
answers this with two rules, not one:

1. **What is unreachable is refused** (§Refusal behaviour). A wide read that
   found no way to a store that should have been consulted is a refusal, not
   a partial answer.
2. **What is deliberately not yet included is declared.** A wide read on a
   client while some stores are still local to another machine (see
   §Transition step 3) succeeds, prints what it served, and *additionally*
   prints a loud line to stderr naming what is temporarily outside:

   ```
   brain --wide: served brain_unified only. 12 stores are still local to
   other machines (inbox, person, …) and were not read; run
   'brain stores list --wide' on the serving machine for the whole
   federation, or finish the transition.
   ```

   The exit stays 0 — this is not a refusal, it is a declared gap — but the
   rule matches the vision: a number that under-reports without saying so is
   a defect; saying so, with the exact list, is obeying the rule. When the
   transition finishes, this line disappears with the condition it guarded.

## The transition path: what today's shim and reconcile script are for

The transition has six steps. `beads-reconcile.sh`'s role is fixed in step 1;
the shim's in step 3. The shim is not the destination — it is the packaging
for the destination's reachability check.

**Step 1 — reconcile is not a cutover tool, it is a pre-cutover tool.**
Today's `beads-reconcile.sh` moves chunks between per-store `.beads`
directories using the per-store `macbook` Dolt remote. That mechanism is
*for the input side of this transition*, because the reconcile is the one
thing that can land a store's database from its current-machine copy onto
the serving machine. Its role ends when a store is remote — see step 5's
retirement.

**Step 2 — SQL identities land on the serving machine before clients do.**
One SQL account per consuming machine, least privilege, named after the
machine, provisioned by the host's admin; a client machine that has to ask
for its identity asks over the ssh bootstrap (Option A's defined role in
§The marked recommendation). This is also where `brainsync@'%'` is shrunk or
retired, before any new identity exists to inherit its breadth. The
`brain stores` registry stays the machine-of-record for locations;
identities are host-side.

**Step 3 — routing becomes a wrapper-emission decision; the shim keeps the
probe.** The wrapper generator is the piece that can read the registry, so
it — not the shim — decides the transport, per the shim's own NOTE that a
`BEADS_DOLT_SERVER_HOST` export only works placed below the store-specific
pin exports:

- a store whose registry entry stays `location: local` gets exactly today's
  wrapper, shim line first, byte for byte: probe, choose, continue.
- a store whose registry entry becomes `location: remote` gets its wrapper
  regenerated with a routing block *below the pin exports* (the position the
  shim's NOTE names) that sets `BEADS_DOLT_SERVER_HOST`, `_PORT`, the
  credential env, and the client TLS position recorded in §Security — then
  probes the store's registered SQL host:port the same way the shim probes
  the remotesapi today.
- probe succeeds → `BEADS_ROUTE=external-remotesql` and the wrapper execs
  `bd` locally against the host's database; probe fails → the refusal in
  §Refusal behaviour, not `local`.

**Step 4 — store-by-store repoint, lowest-fleet-breadth first**, following
`brain-cutover-runbook.md`'s minus-one-store pattern (one store, reversible,
rollback the two-file pin edit). The act per store is a wrapper edit plus a
registry edit, exactly as the runbook says; the runbook's discipline carries
over unchanged.

**Step 5 — a store's cut-over kills its reconcile role.** When a store's
wrapper speaks SQL to the serve-side, `macbook`'s remote on that store is
removed (the wrapper's `.beads/` no longer holds a *source* of truth — the
serving machine's database is the truth, and the `macbook` remote was the
mirror). Reconcile's store list shrinks to zero as the transition finishes;
when reconcile has no stores left, the shim's `external-first` probe is
retired and the registry's `location: remote` fields are the whole story.
The shim is a transition mechanism, not the destination.

**Step 6 — merged-database move is *not* pre-specified here.** This design
does not schedule the `brain_unified` cutover (the previous runbook owns
it); it only makes every step above zero-risk for that eventual move,
because each store reaching the serving machine is a *transport* move and
the merged-database move is a *content* move — separate, independent,
separately sequenced. Deferring this is deliberate: doing both at once makes
a rollback path where each step's recovery is entangled with the other's.

## Security: what changes before a network client is allowed

**The design refuses to expose:**

- **the serving server's `root` to the network.** It currently already is
  refused (see §What is on the ground) and must stay refused: only
  `localhost` accepts it. Wrapping the server in a tunnel to share root
  (Option B's easy shape) is refused rather than avoided-at-cost.
- **SQL outside the tailnet.** No public DNS record, no router
  port-forward, no internet-facing exposure of the serving machine's SQL
  port. The tailnet is the trust boundary, and that is recorded as a
  standing assumption in this section, not hidden.
- **one shared SQL account.** Every consumer machine has its own identity;
  sharing a password across machines collapses attribution and gives the
  refusal text nowhere to point.
- **anything that broadens what one client can say to another store's rows.**
  See §"The trust boundary the unified shape sets" below: the unified
  namespace model is a *client-side* boundary, and an SQL account on the
  unified database means rows of every store are reachable to that account
  in SQL. The boundary is the tailnet plus the wrapper discipline, not a
  row-level ACL — a fact this design names rather than silently assumes.

**What must change before any client SQL identity is expected to work:**

- named least-privilege SQL accounts per consumer, provisioned on the
  serving server, with the account's *own* machine named (see
  `brainsync@'%'` below for the shape to not repeat);
- a credentials story on the client using the fork's existing shape: the
  INI-style credentials file `~/.config/beads/credentials`, keyed by
  `[host:port]` (path overridable with `BEADS_CREDENTIALS_FILE`), or the
  `BEADS_DOLT_PASSWORD` env directly — owner-only filesystem permissions,
  and no credentials in world-readable places. On the serving machine this
  design found
  `DOLT_REMOTE_PASSWORD` recorded in plaintext in a `launchd` plist backup;
  the pattern to avoid is recorded (how the secret got there is not in this
  design's scope, and the value was not propagated further than the reading
  ssh session).
- a standing position on plaintext MySQL-protocol inside the tailnet: the
  serving server's Dolt build does not support TLS, the client does not ask
  for TLS on its tailnet, and WireGuard guards the bytes in transit. The
  design accepts this and names it: if the tailnet guarantee is ever
  rescoped, TLS stops being optional and the plan changes. Tailnet
  terminology used in this document assumes the standard
  (WireGuard-encrypted) meaning without further caveat.

**`brainsync@'%'`, found, must shrink before the first client identity.** The
account carries `SHUTDOWN`, `FILE`, `SUPER`, `CREATE USER` on `*.*` — the
opposite of least-privilege and the shape every new account would be
pretending. This design does not touch it live (no live changes) and names
shrinking/retiring it as the first host-side step of §Transition step 2,
with the owner-password question still open for the captain (see
§Questions the design leaves deliberately to the captain).

## What the design deliberately does not do

This is a design document, not an implementation:

- no server or SQL identity changed on any machine;
- no wrapper, shim, or registry edited on any machine, including the serving
  machine whose materials were only *read*;
- no store re-pointed, merged, or reconciled;
- the per-store `macbook` remotes left alone (step 5 will retire them, per
  store, later and deliberately).

## Questions the design leaves deliberately to the captain

1. **Which transport, in the end.** The three-option pricing is done and
   the recommendation is marked (§The marked recommendation) — Option C.
   The captain may pick A (cheaper to stand up today, *not* cheaper later,
   because locality's cost only grows), B for a specific off-tailnet
   machine, or override.
2. **Identity granularity.** One SQL identity per machine (recommended:
   matches the fleet, gives per-machine logging; multiple people on one
   machine share the identity, which is the right shape for a
   single-operator fleet) or one identity per store-per-machine (finer
   precision, but a provisioning step per pair, which is overkill
   pre-merge and buys nothing the single-operator fleet needs).
3. **Where the first SQL identity lands.** This design does not name the
   first client machine or the first store to move; that choice is the
   transition's, not the design's.
4. **`brainsync@'%'`'s owner and password.** Found, not determined. The
   account is the shape the design refuses to multiply; its ownership,
   password location, and whether it can simply be retired are captain-side
   facts this design does not have. (Found via a read-only `SHOW GRANTS`
   over ssh; the `DOLT_REMOTE_PASSWORD` value that sits in the serving
   machine's plist backup went no further than the reading ssh session and
   was propagated into no file this task produced.)

### The trust boundary the unified shape sets

One fact is stated plainly rather than buried: the unified database's
namespace guard (`RefuseStorelessMint`, `ValidateNamespaceOwnership`) rides
in the `bd` binary. A database account granted on `brain_unified` can
theoretically write outside the namespace model's intent, because the
boundary lives in the *client-side* code. Pre-merged, per-database SQL
grants were a real (if blunt) second boundary, because each store was a
database and SQL could only see one. Post-merged, the second boundary is the
tailnet itself.

The design accepts this for a single-operator fleet on tailnet-respecting
machines: the fleet is one person, and the boundary lives at the network and
the operating system (who can ssh, who can reach SQL at all), not inside the
row data. That is the same trade-off every machine-local config makes today;
naming it here is so the next design does not rediscover it by accident.
