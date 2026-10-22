package main

// brain rollout: the step definitions. This file is the one place that says
// what the procedure is. `brain rollout --help`, the plan output and the
// engine all read the same rolloutStep values, so the text a person reads is
// the behavior the command has.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storeregistry"
)

// Step outcomes of a check.
const (
	stNeed = "need" // the precondition holds and the action has to run
	stHave = "have" // already true; only the verification runs
	stSkip = "skip" // does not apply to this run
	stFail = "fail" // the precondition does not hold: stop, say what to do
)

// Displayed statuses.
const (
	statOK      = "ok"
	statTodo    = "todo"
	statDone    = "done"
	statFail    = "FAIL"
	statBlocked = "blocked"
	statSkip    = "skip"
	statPending = "pending"
)

type checkResult struct {
	State  string
	Detail string
	Fix    string
}

// rolloutStep is one step of the procedure. Pre, Do, Verify and Undo are the
// human text; check, act and verify are what runs. Scope "fleet" runs once,
// "machine" once per machine.
type rolloutStep struct {
	ID, Title, Scope string
	Pre, Do, Verify  string
	Undo             string
	check            func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult
	act              func(ctx context.Context, r *rolloutRun, mr *machineRun) error
	verify           func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult
	// undo runs when verify fails after act, to put the machine back.
	undo func(ctx context.Context, r *rolloutRun, mr *machineRun) error
	// bookkeeping marks a step whose only effect is the shared record: in a
	// plan it is shown as todo but does not make the machine "need changes".
	bookkeeping bool
	// verifyHave runs verify even when the check found nothing to change: the
	// check alone is not the proof (the installed binary also has to be accepted
	// by the pin).
	verifyHave bool
}

// rolloutCandidate is the binary being rolled out.
type rolloutCandidate struct {
	Path    string `json:"path"`
	Version string `json:"version"` // as the pin compares it: line 1 of --version without "bd version "
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	GOOS    string `json:"goos,omitempty"`
	GOARCH  string `json:"goarch,omitempty"`
}

func (c *rolloutCandidate) short() string { return shaOfVersion(c.Version) }

// loadRolloutCandidate reads the file, asks it for its version (the way the pin
// does, with client forwarding off) and hashes it.
func loadRolloutCandidate(ctx context.Context, path string) (*rolloutCandidate, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the binary
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("%s is not an executable file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	c := &rolloutCandidate{Path: path, SHA256: hex.EncodeToString(h.Sum(nil)), Size: st.Size()}
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, path, "--version") //nolint:gosec // the operator names the binary
	cmd.Env = append(os.Environ(), envRemoteOff+"=off")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s --version failed: %w", path, err)
	}
	line := strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0])
	c.Version = strings.TrimSpace(strings.TrimPrefix(line, "bd version "))
	if c.Version == "" || c.Version == line && !strings.HasPrefix(line, "bd version") {
		return nil, fmt.Errorf("%s --version printed %q, which is not a bd version line", path, line)
	}
	if bi, err := buildinfo.ReadFile(path); err == nil {
		for _, s := range bi.Settings {
			switch s.Key {
			case "GOOS":
				c.GOOS = s.Value
			case "GOARCH":
				c.GOARCH = s.Value
			}
		}
	}
	return c, nil
}

// rolloutRecords is the shared record of what each machine runs (the
// brain_registry_rollouts table). Tests substitute a fake.
type rolloutRecords interface {
	Read(ctx context.Context) ([]storeregistry.Rollout, error)
	Write(ctx context.Context, r storeregistry.Rollout) error
}

// rolloutRun is everything a run shares across steps and machines.
type rolloutRun struct {
	Apply    bool
	Rollback bool
	Cand     *rolloutCandidate
	Store    string
	Previous string // rollback: a kept binary on the machine, overriding the record

	Exec        rolloutExec
	Registry    registrySource
	Records     rolloutRecords
	CreateStore func(ctx context.Context, name string) error
	Self        string
	Actor       string
	Now         func() time.Time

	// Filled by the fleet steps.
	Snap     storeregistry.Snapshot
	Epoch    int64
	Digest   string
	Role     string // role of the machine running the command: direct | client
	recs     map[string]storeregistry.Rollout
	recsRead bool
}

func (r *rolloutRun) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// machineRun is one machine's state while its steps run.
type machineRun struct {
	M           fleetMember
	Facts       *machineFacts
	Role        string // direct | client
	PinState    string // ok | absent | stale | unreadable
	PinWant     string
	Target      string // the file a binary step replaces
	Previous    string // the kept previous binary
	PrevVersion string // the version the target ran before this run changed it
	Pending     bool   // an earlier step of this machine needs a change (plan)
	Proved      bool
	Report      *verifyReport
	BinCheck    string
	Version     string // what the machine runs now
}

func (r *rolloutRun) run(ctx context.Context, m fleetMember, script string, stdin io.Reader, timeout time.Duration) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r.Exec.Exec(cctx, m, script, stdin)
}

func (r *rolloutRun) probe(ctx context.Context, mr *machineRun) error {
	out, exit, err := r.run(ctx, mr.M, scriptProbe, nil, 40*time.Second)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("the probe script exited %d: %s", exit, rolloutFirstLine(out))
	}
	f, perr := parseMachineFacts(out)
	if perr != nil {
		return perr
	}
	mr.Facts = f
	mr.Version = f.BdVersion
	mr.Role = "direct"
	if f.Relay != "" {
		mr.Role = "client"
	}
	return nil
}

// ---------------------------------------------------------------------------
// the install procedure
// ---------------------------------------------------------------------------

const pinOwnerHint = "The pin is generated by satellite-config's converge-stores.sh, not by this repository, so brain does not edit it."

func pinFix(m, want, got string, owner string) string {
	if owner == "satellite-config" {
		return fmt.Sprintf("set BD_WANT to '%s' in satellite-config's repo copy of .bd-pin.sh and run converge-stores.sh on %s (it installs the pin and the binary together, with a restore.sh), then re-run 'brain rollout'", got, m)
	}
	return fmt.Sprintf("set BD_WANT to '%s' in whatever installs ~/.local/bin/.bd-pin.sh on %s (it is not generated by brain), then re-run 'brain rollout'", got, m)
}

func installSteps() []rolloutStep {
	return []rolloutStep{
		stepCandidate(), stepRegistry(), stepStore(), stepRecords(),
		stepReach(), stepMode(), stepPin(), stepBinary(), stepConverge(), stepProve(), stepRecord(),
	}
}

func rollbackSteps() []rolloutStep {
	return []rolloutStep{
		stepRegistry(), stepRecords(),
		stepReach(), stepMode(), stepRollbackPin(), stepRestore(), stepRecord(),
	}
}

func stepCandidate() rolloutStep {
	return rolloutStep{
		ID: "candidate", Title: "the binary being rolled out", Scope: "fleet",
		Pre:    "the --binary file (default: the running binary) is an executable that prints its own version with --version",
		Do:     "nothing; read-only",
		Verify: "the version string and sha256 are recorded for every later step",
		Undo:   "none",
		check: func(_ context.Context, r *rolloutRun, _ *machineRun) checkResult {
			if r.Cand == nil {
				return checkResult{State: stFail, Detail: "no candidate binary", Fix: "pass --binary PATH"}
			}
			d := fmt.Sprintf("%s reports '%s' (%s/%s, sha256 %s)", r.Cand.Path, r.Cand.Version, orDash(r.Cand.GOOS), orDash(r.Cand.GOARCH), shortHash(r.Cand.SHA256))
			return checkResult{State: stHave, Detail: d}
		},
	}
}

func stepRegistry() rolloutStep {
	return rolloutStep{
		ID: "registry", Title: "the store registry the fleet must converge on", Scope: "fleet",
		Pre:    "the registry is adopted and readable from this machine (directly, or through its relay host on a client)",
		Do:     "nothing; read-only",
		Verify: "the target epoch and digest are read once and every machine is proved against that epoch",
		Undo:   "none",
		check: func(ctx context.Context, r *rolloutRun, _ *machineRun) checkResult {
			snap, found, err := r.Registry.Snapshot(ctx)
			if err != nil {
				return checkResult{State: stFail, Detail: "the registry is unreadable: " + firstErrLine(err),
					Fix: "run this on the brain host (or a client whose host runs a build with the registry), with a working database login"}
			}
			if !found {
				return checkResult{State: stFail, Detail: "the registry is not adopted in this database",
					Fix: "run 'brain stores reconcile' (read-only) and then 'brain stores reconcile --apply' on the brain host, see docs/brain/STORE_REGISTRY_DEPLOY.md"}
			}
			r.Snap, r.Epoch, r.Digest = snap, snap.State.Epoch, snap.State.Digest
			role, relay := r.Registry.Role()
			r.Role = role
			via := "database"
			if role == "client" {
				via = "relay " + relay
			}
			return checkResult{State: stHave, Detail: fmt.Sprintf("epoch %d, digest %s, %d active names, read via %s",
				r.Epoch, shortHash(r.Digest), len(storeregistry.Active(snap.Entries)), via)}
		},
	}
}

func stepStore() rolloutStep {
	return rolloutStep{
		ID: "store", Title: "create the store (only with --store NAME)", Scope: "fleet",
		Pre:    "this machine reads the database directly (it is the brain host, not a client) and the name is a valid store name",
		Do:     "run this binary's 'brain stores create NAME' with the configured database login; credentials, server users and grants are never touched",
		Verify: "the registry has an active entry NAME and its epoch moved; that epoch becomes the target for every machine",
		Undo:   "'brain stores remove NAME' tombstones it (data directory, rows and prefix stay by design)",
		check: func(_ context.Context, r *rolloutRun, _ *machineRun) checkResult {
			if r.Store == "" {
				return checkResult{State: stSkip, Detail: "no --store given: the target is the registry as it is now"}
			}
			if !storeregistry.ValidName(r.Store) {
				return checkResult{State: stFail, Detail: fmt.Sprintf("%q is not a valid store name", r.Store),
					Fix: "use lowercase letters, digits, '-' and '_' (no spaces, dots or slashes)"}
			}
			for _, e := range storeregistry.Active(r.Snap.Entries) {
				if e.Name == r.Store && e.Kind == storeregistry.KindStore {
					return checkResult{State: stHave, Detail: fmt.Sprintf("store %q is already registered (changed at epoch %d)", r.Store, e.ChangedEpoch)}
				}
			}
			if r.Role == "client" {
				return checkResult{State: stFail, Detail: "this machine is a client: a store is created on the machine that hosts the database",
					Fix: "run 'brain rollout --store " + r.Store + "' on the brain host (see 'brain host status')"}
			}
			if r.CreateStore == nil {
				return checkResult{State: stFail, Detail: "this build cannot create a store here"}
			}
			return checkResult{State: stNeed, Detail: fmt.Sprintf("create store %q (epoch %d -> %d)", r.Store, r.Epoch, r.Epoch+1)}
		},
		act: func(ctx context.Context, r *rolloutRun, _ *machineRun) error {
			return r.CreateStore(ctx, r.Store)
		},
		verify: func(ctx context.Context, r *rolloutRun, _ *machineRun) checkResult {
			if r.Store == "" {
				return checkResult{State: stHave}
			}
			snap, found, err := r.Registry.Snapshot(ctx)
			if err != nil || !found {
				return checkResult{State: stFail, Detail: "cannot re-read the registry after create", Fix: "run 'brain stores epoch' and re-run"}
			}
			for _, e := range storeregistry.Active(snap.Entries) {
				if e.Name == r.Store {
					r.Snap, r.Epoch, r.Digest = snap, snap.State.Epoch, snap.State.Digest
					return checkResult{State: stHave, Detail: fmt.Sprintf("store %q is registered; target epoch is now %d", r.Store, r.Epoch)}
				}
			}
			return checkResult{State: stFail, Detail: fmt.Sprintf("store %q is not in the registry after create", r.Store),
				Fix: "run 'brain stores create " + r.Store + "' by hand to see the error (it resumes idempotently)"}
		},
	}
}

func stepRecords() rolloutStep {
	return rolloutStep{
		ID: "records", Title: "the shared rollout record", Scope: "fleet",
		Pre:    "this machine can read (and with --apply, write) the brain_registry_rollouts table in the shared database",
		Do:     "nothing; read-only",
		Verify: "earlier records are loaded so a rollback knows which binary each machine kept",
		Undo:   "none",
		check: func(ctx context.Context, r *rolloutRun, _ *machineRun) checkResult {
			if r.Records == nil {
				return checkResult{State: stFail, Detail: "this machine has no direct database access (client mode), so it cannot write the rollout record",
					Fix: "run 'brain rollout' on the brain host"}
			}
			recs, err := r.Records.Read(ctx)
			if err != nil {
				return checkResult{State: stFail, Detail: "cannot read the rollout record: " + firstErrLine(err), Fix: "check the database login of this machine (the table is created on the first --apply)"}
			}
			r.recs = map[string]storeregistry.Rollout{}
			for _, x := range recs {
				r.recs[x.Machine] = x
			}
			r.recsRead = true
			return checkResult{State: stHave, Detail: fmt.Sprintf("%d machine(s) have a record", len(recs))}
		},
	}
}

func stepReach() rolloutStep {
	return rolloutStep{
		ID: "reach", Title: "reach the machine and read its facts", Scope: "machine",
		Pre:    "the machine answers ssh non-interactively (BatchMode, 10s) as the deploy user and runs a POSIX sh",
		Do:     "nothing; read-only probe of the pin, the installed binary, the client config and the build's verbs",
		Verify: "the probe answered with its facts",
		Undo:   "none",
		check: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			if err := r.probe(ctx, mr); err != nil {
				return checkResult{State: stFail, Detail: "UNREACHABLE: " + firstErrLine(err),
					Fix: fmt.Sprintf("make 'ssh -o BatchMode=yes %s true' work for the deploy user (key, user name, tailnet), or leave %s out with --machines and say so; an unreachable machine is never reported converged", mr.M.SSH, mr.M.Name)}
			}
			return checkResult{State: stHave, Detail: fmt.Sprintf("answers as %s (%s), runs %s", mr.Facts.Name, mr.Facts.Uname, orDash(mr.Facts.BdVersion))}
		},
	}
}

func stepMode() rolloutStep {
	return rolloutStep{
		ID: "mode", Title: "direct machine or client", Scope: "machine",
		Pre:    "none",
		Do:     "nothing; read-only",
		Verify: "a client (~/.config/brain/remote.yaml names a host) forwards commands to its host and so would run the host's build; every version check on a client therefore runs its own binary with BRAIN_REMOTE=off, and the registry is read through the relay as the client itself reads it",
		Undo:   "none",
		check: func(_ context.Context, _ *rolloutRun, mr *machineRun) checkResult {
			if mr.Role == "client" {
				return checkResult{State: stHave, Detail: "client of " + mr.Facts.Relay + ": its own binary is checked with BRAIN_REMOTE=off, never through the host"}
			}
			return checkResult{State: stHave, Detail: "direct machine"}
		},
	}
}

func (mr *machineRun) pinInfo() (state, want string) {
	f := mr.Facts
	switch {
	case !f.PinPresent:
		return "absent", ""
	case f.PinWant == "":
		return "unreadable", ""
	}
	return "", f.PinWant
}

func stepPin() rolloutStep {
	return rolloutStep{
		ID: "pin", Title: "the wrapper pin accepts the binary", Scope: "machine",
		Pre:    "~/.local/bin/.bd-pin.sh is absent, or its BD_WANT equals the candidate's version string exactly; otherwise every pinned wrapper would exit 78 as soon as the binary is swapped. The pin is owned by satellite-config, so a mismatch is refused with the instruction, never edited",
		Do:     "nothing; read-only",
		Verify: "the pin state (ok, absent) is recorded for the machine",
		Undo:   "none",
		check: func(_ context.Context, r *rolloutRun, mr *machineRun) checkResult {
			f := mr.Facts
			st, want := mr.pinInfo()
			mr.PinWant = want
			switch st {
			case "absent":
				mr.PinState = "absent"
				return checkResult{State: stHave, Detail: "no .bd-pin.sh: wrappers do not pin a version here"}
			case "unreadable":
				mr.PinState = "unreadable"
				return checkResult{State: stFail, Detail: "~/.local/bin/.bd-pin.sh exists but BD_WANT could not be read from it",
					Fix: "read the file on " + mr.M.Name + " by hand; brain will not install a binary behind a pin it cannot read"}
			}
			if want != r.Cand.Version {
				mr.PinState = "stale"
				return checkResult{State: stFail,
					Detail: fmt.Sprintf("the pin on %s wants '%s' but the candidate is '%s': after the swap every pinned wrapper would refuse to run. %s", mr.M.Name, want, r.Cand.Version, pinOwnerHintFor(f.PinOwner)),
					Fix:    pinFix(mr.M.Name, want, r.Cand.Version, f.PinOwner)}
			}
			mr.PinState = "ok"
			return checkResult{State: stHave, Detail: fmt.Sprintf("pin wants '%s', the candidate's version", want)}
		},
	}
}

func pinOwnerHintFor(owner string) string {
	if owner == "satellite-config" {
		return pinOwnerHint
	}
	return "The pin is not generated by brain, so brain does not edit it."
}

func stepBinary() rolloutStep {
	return rolloutStep{
		ID: "binary", Title: "install the binary, keeping the previous one", Scope: "machine", verifyHave: true,
		Pre:    "the pin accepts the candidate; the candidate's platform is the machine's; the file to replace is known (BD_BIN, or the bd.real behind a BD_BIN shim)",
		Do:     "upload the candidate beside the target, run it there and check it reports the candidate's version, copy the current binary to bd.pre-<sha> (an existing, different backup is never overwritten), then rename the new file into place",
		Verify: "the machine's own binary (BRAIN_REMOTE=off) reports the candidate's version and the pin, sourced the way a wrapper does, accepts it; if not, the kept binary is put back at once",
		Undo:   "'brain rollout rollback --apply' puts the kept binary back (keeping the replaced one as bd.post-<sha>)",
		check: func(_ context.Context, r *rolloutRun, mr *machineRun) checkResult {
			f := mr.Facts
			if goos, goarch := goPlatform(f.Uname); r.Cand.GOOS != "" && (goos != r.Cand.GOOS || goarch != r.Cand.GOARCH) {
				return checkResult{State: stFail,
					Detail: fmt.Sprintf("the candidate is built for %s/%s but %s is %s", r.Cand.GOOS, r.Cand.GOARCH, mr.M.Name, f.Uname),
					Fix:    fmt.Sprintf("build one for %s/%s and pass it as --binary, or leave %s out with --machines", goos, goarch, mr.M.Name)}
			}
			target, installed, err := f.installTarget()
			if err != nil {
				return checkResult{State: stFail, Detail: err.Error(), Fix: "install the binary on " + mr.M.Name + " by the machine's own converge, then re-run to verify"}
			}
			mr.Target = target
			if f.BdVersion == r.Cand.Version && installed == r.Cand.Version {
				return checkResult{State: stHave, Detail: fmt.Sprintf("%s already runs '%s'", target, r.Cand.Version)}
			}
			return checkResult{State: stNeed, Detail: fmt.Sprintf("replace %s ('%s') with '%s'; the current binary is kept as %s/bd.pre-%s",
				target, orDash(installed), r.Cand.Version, dirOf(target), shaOfVersion(installed))}
		},
		act: func(ctx context.Context, r *rolloutRun, mr *machineRun) error {
			f, err := os.Open(r.Cand.Path) //nolint:gosec // the operator names the binary
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, installed, _ := mr.Facts.installTarget()
			mr.PrevVersion = installed
			script := scriptInstall(mr.Target, r.Cand.Version, shaOfVersion(installed), r.Cand.Size)
			out, exit, err := r.run(ctx, mr.M, script, f, 10*time.Minute)
			if err != nil {
				return err
			}
			if exit != 0 {
				return fmt.Errorf("install script exited %d: %s", exit, rolloutFirstLine(strings.TrimSpace(out)))
			}
			for _, line := range strings.Split(out, "\n") {
				if v, ok := strings.CutPrefix(line, "previous="); ok && v != "none" {
					mr.Previous = strings.TrimSpace(v)
				}
			}
			return nil
		},
		verify: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			return verifyInstalled(ctx, r, mr, r.Cand.Version)
		},
		undo: func(ctx context.Context, r *rolloutRun, mr *machineRun) error {
			if mr.Previous == "" {
				return fmt.Errorf("nothing was kept to restore")
			}
			out, exit, err := r.run(ctx, mr.M, scriptRestore(mr.Target, mr.Previous, mr.PrevVersion, r.Cand.short()), nil, 2*time.Minute)
			if err != nil || exit != 0 {
				return fmt.Errorf("restoring %s failed: %v %s", mr.Previous, err, rolloutFirstLine(out))
			}
			return nil
		},
	}
}

// versionToken is the part of a --version line a verify report carries:
// "1.1.0-rc.1" of "1.1.0-rc.1 (972952bf7)".
func versionToken(v string) string {
	if f := strings.Fields(v); len(f) > 0 {
		return f[0]
	}
	return v
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// verifyInstalled re-reads the machine and asserts the version it runs and that
// its pin accepts it.
func verifyInstalled(ctx context.Context, r *rolloutRun, mr *machineRun, want string) checkResult {
	if err := r.probe(ctx, mr); err != nil {
		return checkResult{State: stFail, Detail: "cannot re-read " + mr.M.Name + " after the change: " + firstErrLine(err)}
	}
	f := mr.Facts
	if f.BdVersion != want {
		return checkResult{State: stFail, Detail: fmt.Sprintf("%s runs '%s', expected '%s'", mr.M.Name, f.BdVersion, want),
			Fix: "run 'brain rollout rollback' if the binary was replaced"}
	}
	out, exit, err := r.run(ctx, mr.M, scriptPinCheck, nil, 40*time.Second)
	if err != nil || exit != 0 || !strings.Contains(out, "pin-ok") && !strings.Contains(out, "no-pin") {
		msg := rolloutFirstLine(out)
		if err != nil {
			msg = firstErrLine(err)
		}
		return checkResult{State: stFail, Detail: fmt.Sprintf("the wrapper pin on %s refuses the binary: %s", mr.M.Name, msg),
			Fix: "restore the previous binary (brain rollout rollback --apply) and the pin together"}
	}
	return checkResult{State: stHave, Detail: fmt.Sprintf("%s runs '%s' and the pin accepts it", mr.M.Name, want)}
}

func stepConverge() rolloutStep {
	return rolloutStep{
		ID: "converge", Title: "make the machine apply the registry", Scope: "machine",
		Pre:    "the machine's brain has 'stores sync' (the build contains the registry)",
		Do:     "run the machine's own 'brain stores sync' (idempotent; backs up what it replaces; never touches a wrapper brain did not generate)",
		Verify: "the machine's own 'brain stores verify' reports the target epoch and nothing missing",
		Undo:   "sh ~/.local/share/brain/registry-backup/<time>/restore.sh on the machine; BRAIN_REGISTRY=off switches the registry off",
		check: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			if mr.Pending {
				return checkResult{State: stNeed, Detail: "after the binary is installed: run 'brain stores sync' on " + mr.M.Name}
			}
			if !mr.Facts.hasBrain {
				return checkResult{State: stFail, Detail: "no 'brain' command on the PATH of " + mr.M.Name,
					Fix: "install the brain wrappers on " + mr.M.Name + " (brain remote install on a client, converge-stores.sh on a direct machine)"}
			}
			if !mr.Facts.HasSync {
				return checkResult{State: stFail, Detail: fmt.Sprintf("the build on %s ('%s') has no 'brain stores sync': it predates the registry", mr.M.Name, orDash(mr.Facts.BrainVersion)),
					Fix: "roll the candidate out to it first (this command does, once its pin allows)"}
			}
			rep, err := r.fetchReport(ctx, mr)
			if err != nil {
				return checkResult{State: stFail, Detail: "cannot read " + mr.M.Name + "'s own registry report: " + firstErrLine(err), Fix: reportFix(mr)}
			}
			if rep.AppliedEpoch == r.Epoch && rep.RegistryEpoch == r.Epoch && rep.Converged() {
				return checkResult{State: stHave, Detail: fmt.Sprintf("already applied epoch %d", rep.AppliedEpoch)}
			}
			return checkResult{State: stNeed, Detail: fmt.Sprintf("applied epoch %d, target %d: run 'brain stores sync' on %s", rep.AppliedEpoch, r.Epoch, mr.M.Name)}
		},
		act: func(ctx context.Context, r *rolloutRun, mr *machineRun) error {
			out, exit, err := r.run(ctx, mr.M, scriptSync, nil, 3*time.Minute)
			if err != nil {
				return err
			}
			if exit != 0 {
				return fmt.Errorf("'brain stores sync' exited %d: %s", exit, rolloutFirstLine(strings.TrimSpace(out)))
			}
			return nil
		},
		verify: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			rep, err := r.fetchReport(ctx, mr)
			if err != nil {
				return checkResult{State: stFail, Detail: "cannot read " + mr.M.Name + "'s own registry report: " + firstErrLine(err), Fix: reportFix(mr)}
			}
			if rep.AppliedEpoch != r.Epoch || !rep.Converged() {
				return checkResult{State: stFail, Detail: fmt.Sprintf("%s applied epoch %d (target %d)%s", mr.M.Name, rep.AppliedEpoch, r.Epoch, missingText(rep)),
					Fix: fmt.Sprintf("read 'brain stores sync --dry-run' on %s, then re-run", mr.M.Name)}
			}
			return checkResult{State: stHave, Detail: fmt.Sprintf("applied epoch %d", rep.AppliedEpoch)}
		},
	}
}

func reportFix(mr *machineRun) string {
	if mr.Role == "client" {
		return fmt.Sprintf("%s is a client: its relay %s must run a build that has the registry; roll the relay out first", mr.M.Name, mr.Facts.Relay)
	}
	return fmt.Sprintf("run 'brain stores verify' on %s to see why", mr.M.Name)
}

func missingText(rep *verifyReport) string {
	var parts []string
	if m := flattenMissing(rep.Missing); len(m) > 0 {
		parts = append(parts, "missing "+strings.Join(m, " "))
	}
	for _, p := range rep.Probes {
		if p.Status == "fail" {
			parts = append(parts, p.Name+" does not read ("+p.Reason+")")
		}
	}
	if rep.Error != "" {
		parts = append(parts, rep.Error)
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, "; ")
}

// fetchReport asks the machine ITSELF for its registry state and parses it.
func (r *rolloutRun) fetchReport(ctx context.Context, mr *machineRun) (*verifyReport, error) {
	out, exit, err := r.run(ctx, mr.M, scriptReport, nil, 90*time.Second)
	if err != nil {
		return nil, err
	}
	start := strings.IndexByte(out, '{')
	if start < 0 {
		return nil, fmt.Errorf("no JSON from 'brain stores verify' (exit %d): %q", exit, rolloutFirstLine(out))
	}
	var rep verifyReport
	dec := json.NewDecoder(strings.NewReader(out[start:]))
	if err := dec.Decode(&rep); err != nil {
		return nil, fmt.Errorf("unreadable report: %w", err)
	}
	mr.Report = &rep
	return &rep, nil
}

func stepProve() rolloutStep {
	return rolloutStep{
		ID: "prove", Title: "the machine itself proves it converged", Scope: "machine",
		Pre:    "all earlier steps of the machine passed",
		Do:     "nothing; read-only",
		Verify: "the report came from this machine (its own name, not its host's), the machine's own binary and its brain command run the candidate's exact version, at the target epoch, from the live registry (not a cache), converged, and the newest store reads by name through its wrapper",
		Undo:   "none",
		check: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			if mr.Pending {
				return checkResult{State: stSkip, Detail: "pending: proved after the changes above"}
			}
			return proveMachine(ctx, r, mr)
		},
	}
}

func proveMachine(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
	if err := r.probe(ctx, mr); err != nil {
		return checkResult{State: stFail, Detail: "cannot re-read " + mr.M.Name + ": " + firstErrLine(err)}
	}
	f := mr.Facts
	if f.BdVersion != r.Cand.Version {
		return checkResult{State: stFail, Detail: fmt.Sprintf("%s's own binary (BRAIN_REMOTE=off) is '%s', not '%s'", mr.M.Name, orDash(f.BdVersion), r.Cand.Version),
			Fix: "install the candidate on " + mr.M.Name}
	}
	if f.hasBrain && f.BrainVersion != r.Cand.Version {
		return checkResult{State: stFail, Detail: fmt.Sprintf("the 'brain' command on %s runs '%s', not '%s'", mr.M.Name, f.BrainVersion, r.Cand.Version),
			Fix: "the brain wrapper on " + mr.M.Name + " resolves to a different binary than bd; fix the wrapper's BD_BIN"}
	}
	rep, err := r.fetchReport(ctx, mr)
	if err != nil {
		return checkResult{State: stFail, Detail: "cannot read " + mr.M.Name + "'s own report: " + firstErrLine(err), Fix: reportFix(mr)}
	}
	switch {
	case !strings.EqualFold(rep.Machine, mr.M.Name):
		return checkResult{State: stFail, Detail: fmt.Sprintf("the report says machine '%s', not '%s': the answer is not from %s (a client forwarding to its host?)", rep.Machine, mr.M.Name, mr.M.Name),
			Fix: fmt.Sprintf("make the roster name equal the machine's own name (BRAIN_MACHINE or its short hostname), or run 'brain stores verify' on %s", mr.M.Name)}
	case rep.BdVersion != versionToken(r.Cand.Version):
		// The report carries only the version token, not the commit; the commit was
		// matched above on the same 'brain' command (brain --version).
		return checkResult{State: stFail, Detail: fmt.Sprintf("%s reports version '%s', not '%s'", mr.M.Name, rep.BdVersion, versionToken(r.Cand.Version)), Fix: "install the candidate on " + mr.M.Name}
	case rep.Source == "cache":
		return checkResult{State: stFail, Detail: mr.M.Name + " could not read the registry and answered from its cache: that proves nothing", Fix: reportFix(mr)}
	case rep.AppliedEpoch != r.Epoch || rep.RegistryEpoch != r.Epoch:
		return checkResult{State: stFail, Detail: fmt.Sprintf("%s applied epoch %d and sees registry epoch %d, target is %d", mr.M.Name, rep.AppliedEpoch, rep.RegistryEpoch, r.Epoch),
			Fix: fmt.Sprintf("run 'brain stores sync' on %s", mr.M.Name)}
	case !rep.Converged():
		return checkResult{State: stFail, Detail: fmt.Sprintf("%s is at epoch %d but not converged%s", mr.M.Name, rep.AppliedEpoch, missingText(rep)),
			Fix: fmt.Sprintf("run 'brain stores sync' on %s", mr.M.Name)}
	}
	name := r.proveStoreName()
	if name != "" {
		out, exit, err := r.run(ctx, mr.M, scriptStoreRead(name), nil, 90*time.Second)
		if err != nil {
			return checkResult{State: stFail, Detail: firstErrLine(err)}
		}
		if exit != 0 {
			return checkResult{State: stFail, Detail: fmt.Sprintf("store '%s' does not answer by name on %s (exit %d: %s)", name, mr.M.Name, exit, rolloutFirstLine(out)),
				Fix: fmt.Sprintf("run '%s list' on %s to see the error", name, mr.M.Name)}
		}
	}
	d := fmt.Sprintf("%s itself reports epoch %d, '%s', converged", mr.M.Name, rep.AppliedEpoch, rep.BdVersion)
	if name != "" {
		d += fmt.Sprintf("; '%s list' answers", name)
	}
	mr.Proved = true
	return checkResult{State: stHave, Detail: d}
}

// proveStoreName is the store whose wrapper is read on every machine: the one
// just created, else the registry's newest.
func (r *rolloutRun) proveStoreName() string {
	if r.Store != "" {
		return r.Store
	}
	act := storeregistry.Active(r.Snap.Entries)
	sort.Slice(act, func(i, j int) bool { return act[i].ChangedEpoch > act[j].ChangedEpoch })
	for _, e := range act {
		if e.Kind == storeregistry.KindStore && e.WrapperKind == storeregistry.WrapperServer {
			return e.Name
		}
	}
	return ""
}

func (r *rolloutRun) recordFor(mr *machineRun, outcome, step, detail string) storeregistry.Rollout {
	rec := storeregistry.Rollout{
		Machine: mr.M.Name, Role: mr.Role, BdVersion: mr.Version, PinState: mr.PinState, PinWant: mr.PinWant,
		BinaryPath: mr.Target, PreviousBinary: mr.Previous, Epoch: r.Epoch,
		Outcome: outcome, Step: step, Detail: truncate(detail, 900), Actor: r.Actor,
	}
	if prev, ok := r.recs[mr.M.Name]; ok && rec.PreviousBinary == "" {
		rec.PreviousBinary = prev.PreviousBinary
	}
	if outcome == storeregistry.RolloutVerified {
		rec.VerifiedAt = r.now().Format("2006-01-02 15:04:05")
	}
	return rec
}

func stepRecord() rolloutStep {
	return rolloutStep{
		ID: "record", Title: "write what was established to the shared record", Scope: "machine", bookkeeping: true,
		Pre:    "the shared database accepts the write (checked at the 'records' step)",
		Do:     "upsert the machine's row in brain_registry_rollouts: binary version, pin state, kept previous binary, epoch, outcome, time verified. A refused or failed machine is recorded too, as refused/failed/unreachable, and keeps its earlier verified time",
		Verify: "the row is read back",
		Undo:   "none (the record is history of what was found)",
		check: func(_ context.Context, r *rolloutRun, mr *machineRun) checkResult {
			if !r.recsRead {
				return checkResult{State: stFail, Detail: "the rollout record is not available", Fix: "see the 'records' step"}
			}
			return checkResult{State: stNeed, Detail: fmt.Sprintf("record %s: %s, pin %s, epoch %d", mr.M.Name, orDash(mr.Version), orDash(mr.PinState), r.Epoch)}
		},
		act: func(ctx context.Context, r *rolloutRun, mr *machineRun) error {
			outcome, detail := storeregistry.RolloutApplied, "binary and registry applied; not yet proved"
			step := "record"
			if mr.Proved {
				outcome, detail, step = storeregistry.RolloutVerified, "the machine itself reported the target epoch and the candidate build", "prove"
			}
			if r.Rollback {
				outcome, detail, step = storeregistry.RolloutRolledBack, "previous binary restored; pin accepts it", "restore"
			}
			return r.Records.Write(ctx, r.recordFor(mr, outcome, step, detail))
		},
		verify: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			recs, err := r.Records.Read(ctx)
			if err != nil {
				return checkResult{State: stFail, Detail: "cannot read the record back: " + firstErrLine(err)}
			}
			for _, x := range recs {
				if x.Machine == mr.M.Name && x.Epoch == r.Epoch && x.BdVersion == mr.Version {
					return checkResult{State: stHave, Detail: fmt.Sprintf("recorded %s for %s at epoch %d", x.Outcome, x.Machine, x.Epoch)}
				}
			}
			return checkResult{State: stFail, Detail: "the record did not read back for " + mr.M.Name}
		},
	}
}

// ---------------------------------------------------------------------------
// rollback
// ---------------------------------------------------------------------------

// previousFor is the kept binary to restore: --previous, else the record.
func (r *rolloutRun) previousFor(mr *machineRun) string {
	if r.Previous != "" {
		return r.Previous
	}
	if rec, ok := r.recs[mr.M.Name]; ok {
		return rec.PreviousBinary
	}
	return ""
}

func stepRollbackPin() rolloutStep {
	return rolloutStep{
		ID: "pin", Title: "the wrapper pin accepts the kept binary", Scope: "machine",
		Pre:    "a kept binary is known (the rollout record, or --previous PATH on the machine) and the pin is absent or wants that binary's version; restoring behind a pin that wants the new version would make every wrapper refuse",
		Do:     "nothing; read-only",
		Verify: "the kept binary's version is read on the machine",
		Undo:   "none",
		check: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			prev := r.previousFor(mr)
			if prev == "" {
				return checkResult{State: stFail, Detail: "no kept binary is recorded for " + mr.M.Name,
					Fix: fmt.Sprintf("pass --previous PATH, a file on %s (kept files: %s)", mr.M.Name, orDash(strings.Join(mr.Facts.PreBackups, " ")))}
			}
			out, exit, err := r.run(ctx, mr.M, scriptVersionOf(prev), nil, 40*time.Second)
			if err != nil {
				return checkResult{State: stFail, Detail: firstErrLine(err)}
			}
			ver := ""
			for _, l := range strings.Split(out, "\n") {
				if v, ok := strings.CutPrefix(l, "version="); ok {
					ver = strings.TrimSpace(v)
				}
			}
			if exit != 0 || ver == "" {
				return checkResult{State: stFail, Detail: fmt.Sprintf("the kept binary %s is missing or does not run on %s", prev, mr.M.Name), Fix: "pass --previous PATH to a binary that runs"}
			}
			mr.Previous = prev
			mr.BinCheck = ver
			st, want := mr.pinInfo()
			mr.PinWant = want
			switch {
			case st == "unreadable":
				mr.PinState = "unreadable"
				return checkResult{State: stFail, Detail: "the pin exists but BD_WANT could not be read", Fix: "read ~/.local/bin/.bd-pin.sh on " + mr.M.Name + " by hand"}
			case st == "absent":
				mr.PinState = "absent"
			case want != ver:
				mr.PinState = "stale"
				return checkResult{State: stFail,
					Detail: fmt.Sprintf("the pin on %s wants '%s' but the kept binary is '%s': restoring it would make every pinned wrapper refuse. %s", mr.M.Name, want, ver, pinOwnerHintFor(mr.Facts.PinOwner)),
					Fix:    pinFix(mr.M.Name, want, ver, mr.Facts.PinOwner) + " (the converge backup's restore.sh restores the old pin and binary together)"}
			default:
				mr.PinState = "ok"
			}
			return checkResult{State: stHave, Detail: fmt.Sprintf("kept binary %s is '%s'; pin %s", prev, ver, mr.PinState)}
		},
	}
}

func stepRestore() rolloutStep {
	return rolloutStep{
		ID: "restore", Title: "put the kept binary back", Scope: "machine", verifyHave: true,
		Pre:    "the pin accepts the kept binary",
		Do:     "copy the kept binary over the installed one; the binary it replaces is kept as bd.post-<sha>",
		Verify: "the machine's own binary reports the kept binary's version and the pin accepts it",
		Undo:   "run 'brain rollout --apply' again with the candidate",
		check: func(_ context.Context, _ *rolloutRun, mr *machineRun) checkResult {
			target, installed, err := mr.Facts.installTarget()
			if err != nil {
				return checkResult{State: stFail, Detail: err.Error()}
			}
			mr.Target = target
			if mr.Facts.BdVersion == mr.BinCheck {
				return checkResult{State: stHave, Detail: fmt.Sprintf("%s already runs '%s'", target, mr.BinCheck)}
			}
			return checkResult{State: stNeed, Detail: fmt.Sprintf("restore %s ('%s') over %s ('%s')", mr.Previous, mr.BinCheck, target, orDash(installed))}
		},
		act: func(ctx context.Context, r *rolloutRun, mr *machineRun) error {
			_, installed, _ := mr.Facts.installTarget()
			out, exit, err := r.run(ctx, mr.M, scriptRestore(mr.Target, mr.Previous, mr.BinCheck, shaOfVersion(installed)), nil, 2*time.Minute)
			if err != nil {
				return err
			}
			if exit != 0 {
				return fmt.Errorf("restore exited %d: %s", exit, rolloutFirstLine(strings.TrimSpace(out)))
			}
			return nil
		},
		verify: func(ctx context.Context, r *rolloutRun, mr *machineRun) checkResult {
			res := verifyInstalled(ctx, r, mr, mr.BinCheck)
			if res.State != stFail {
				mr.Version = mr.Facts.BdVersion
			}
			return res
		},
	}
}
