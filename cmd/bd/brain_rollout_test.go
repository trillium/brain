package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storeregistry"
)

// ---------------------------------------------------------------------------
// fakes: machines, registry and record. No ssh, no database, no real binary.
// ---------------------------------------------------------------------------

const (
	oldVer = "1.1.0-rc.1 (972952bf7)"
	newVer = "1.1.0-rc.1 (abc1234ef)"
)

type fakeMachine struct {
	unreachable bool
	uname       string
	pinPresent  bool
	pinWant     string
	pinOwner    string
	binKind     string // binary | script
	version     string // what the installed binary prints
	brainVer    string // what 'brain --version' prints (BRAIN_REMOTE=off)
	hasSync     bool
	relay       string // client mode when set
	applied     int64  // epoch this machine has applied
	registryAt  int64  // epoch it sees
	reportName  string // machine name in its own report ("" = its roster name)
	reportVer   string // build in its report ("" = version)
	cache       bool
	missing     map[string][]string
	storeBroken bool
	noWrapper   bool
	lastScripts map[string]string // kind -> the script text last run
	previous    string            // kept binary after an install
	uploads     int
	syncRuns    int
	restores    int
	scripts     []string // marker of every script run
	envOff      map[string]bool
}

type fakeFleet struct {
	mu       sync.Mutex
	machines map[string]*fakeMachine
	epoch    int64
}

func (f *fakeFleet) add(name string, m *fakeMachine) *fakeMachine {
	if m.uname == "" {
		m.uname = "Darwin arm64"
	}
	if m.binKind == "" {
		m.binKind = "binary"
	}
	m.hasSync = true
	m.envOff = map[string]bool{}
	m.reportVer = ""
	if f.machines == nil {
		f.machines = map[string]*fakeMachine{}
	}
	f.machines[name] = m
	return m
}

func marker(script string) string {
	first := strings.SplitN(script, "\n", 2)[0]
	return strings.TrimPrefix(first, "# rollout:")
}

func (f *fakeFleet) Exec(_ context.Context, mem fleetMember, script string, stdin io.Reader) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.machines[mem.Name]
	if m == nil || m.unreachable {
		return "", 255, fmt.Errorf("ssh to %s failed: Permission denied (publickey)", mem.SSH)
	}
	kind := marker(script)
	m.scripts = append(m.scripts, kind)
	if m.lastScripts == nil {
		m.lastScripts = map[string]string{}
	}
	m.lastScripts[kind] = script
	m.envOff[kind] = strings.Contains(script, "BRAIN_REMOTE=off")
	switch kind {
	case "probe":
		var b strings.Builder
		fmt.Fprintf(&b, "probe=1\nname=%s\nhome=/home/%s\nuname=%s\n", mem.Name, mem.Name, m.uname)
		if m.pinPresent {
			b.WriteString("pin=present\n")
			if m.pinOwner != "" {
				fmt.Fprintf(&b, "pin_owner=%s\n", m.pinOwner)
			}
			fmt.Fprintf(&b, "pin_bin=/home/%s/.local/bin/bd\npin_want=%s\n", mem.Name, m.pinWant)
		} else {
			b.WriteString("pin=absent\n")
		}
		fmt.Fprintf(&b, "bin=/home/%s/.local/bin/bd\nbin_kind=%s\n", mem.Name, m.binKind)
		if m.binKind == "script" {
			fmt.Fprintf(&b, "bd_version=%s\nbin_real=/home/%s/.local/bin/bd.real\nreal_version=%s\n", m.version, mem.Name, m.version)
		} else {
			fmt.Fprintf(&b, "bd_version=%s\n", m.version)
		}
		bv := m.brainVer
		if bv == "" {
			bv = m.version
		}
		fmt.Fprintf(&b, "brain_version=%s\n", bv)
		if m.hasSync {
			b.WriteString("has_sync=1\n")
		}
		if m.relay != "" {
			fmt.Fprintf(&b, "relay=%s\n", m.relay)
		}
		if m.previous != "" {
			fmt.Fprintf(&b, "pre=%s\n", m.previous)
		}
		return b.String(), 0, nil
	case "pincheck":
		if m.pinPresent && m.pinWant != m.version {
			return "bead: bd is '" + m.version + "', wanted '" + m.pinWant + "'; refusing\n", 78, nil
		}
		if !m.pinPresent {
			return "no-pin\n", 0, nil
		}
		return "pin-ok\n", 0, nil
	case "install":
		data, _ := io.ReadAll(stdin)
		if len(data) == 0 {
			return "", 1, nil
		}
		m.uploads++
		sha := shaOfVersion(m.version)
		m.previous = "/home/" + mem.Name + "/.local/bin/bd.pre-" + sha
		m.version = newVer
		return "previous=" + m.previous + "\n", 0, nil
	case "restore":
		m.restores++
		if v := regexpVersion(script); v != "" {
			m.version = v
		}
		return "", 0, nil
	case "version":
		return "version=" + oldVer + "\n", 0, nil
	case "sync":
		m.syncRuns++
		m.applied = f.epoch
		m.registryAt = f.epoch
		return "converged\n", 0, nil
	case "report":
		name := m.reportName
		if name == "" {
			name = mem.Name
		}
		ver := m.reportVer
		if ver == "" {
			ver = versionToken(m.version) // a real report carries the version token only
		}
		src := "database"
		if m.cache {
			src = "cache"
		}
		rep := verifyReport{Machine: name, Role: "direct", AppliedEpoch: m.applied, RegistryEpoch: m.registryAt,
			AppliedDigest: "d", RegistryHash: "d", Source: src, Active: 3, WrappersOK: 3, BdVersion: ver, Missing: m.missing}
		if rep.Missing == nil {
			rep.Missing = map[string][]string{}
		}
		if m.relay != "" {
			rep.Role = "client"
			if !m.cache {
				rep.Source = "relay"
			}
		}
		b, _ := json.Marshal(rep)
		return string(b) + "\n", 0, nil
	case "wrapper":
		if m.noWrapper {
			return "no wrapper named x on the PATH\n", 44, nil
		}
		return "", 0, nil
	case "storeread":
		if m.storeBroken {
			return "Error: no beads database found\n", 1, nil
		}
		return "", 0, nil
	}
	return "", 127, fmt.Errorf("fake: unknown script %q", kind)
}

func regexpVersion(script string) string {
	const key = "want='"
	i := strings.Index(script, key)
	if i < 0 {
		return ""
	}
	rest := script[i+len(key):]
	j := strings.Index(rest, "'")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (f *fakeFleet) mutations() int {
	n := 0
	for _, m := range f.machines {
		n += m.uploads + m.syncRuns + m.restores
	}
	return n
}

type fakeRegistry struct {
	epoch int64
	store []storeregistry.Entry
	role  string
}

func (g *fakeRegistry) Snapshot(context.Context) (storeregistry.Snapshot, bool, error) {
	return storeregistry.Snapshot{State: storeregistry.State{Epoch: g.epoch, Digest: "d"}, Entries: g.store}, true, nil
}
func (g *fakeRegistry) ProjectID(context.Context, string) (string, error) { return "", nil }
func (g *fakeRegistry) UnifiedDB(context.Context) string                  { return "" }
func (g *fakeRegistry) Report(context.Context, storeregistry.Machine) error {
	return nil
}
func (g *fakeRegistry) Machines(context.Context) ([]storeregistry.Machine, error) { return nil, nil }
func (g *fakeRegistry) Role() (string, string) {
	if g.role == "client" {
		return "client", "mini1"
	}
	return "direct", ""
}

type fakeRecords struct {
	mu     sync.Mutex
	rows   map[string]storeregistry.Rollout
	writes []storeregistry.Rollout
}

func (f *fakeRecords) Read(context.Context) ([]storeregistry.Rollout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storeregistry.Rollout
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRecords) Write(_ context.Context, r storeregistry.Rollout) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]storeregistry.Rollout{}
	}
	if prev, ok := f.rows[r.Machine]; ok && r.VerifiedAt == "" {
		r.VerifiedAt = prev.VerifiedAt
	}
	f.rows[r.Machine] = r
	f.writes = append(f.writes, r)
	return nil
}

type rolloutHarness struct {
	t       *testing.T
	fleet   *fakeFleet
	reg     *fakeRegistry
	records *fakeRecords
	run     *rolloutRun
	created []string
}

func newHarness(t *testing.T) *rolloutHarness {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bd-new")
	if err := os.WriteFile(bin, []byte("binary-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := &rolloutHarness{t: t, fleet: &fakeFleet{epoch: 7}, reg: &fakeRegistry{epoch: 7}, records: &fakeRecords{}}
	h.run = &rolloutRun{
		Cand:     &rolloutCandidate{Path: bin, Version: newVer, SHA256: strings.Repeat("a", 64), Size: 12, GOOS: "darwin", GOARCH: "arm64"},
		Exec:     h.fleet,
		Registry: h.reg,
		Records:  h.records,
		Self:     "mini0",
		Actor:    "rollout:test",
		Now:      func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) },
		CreateStore: func(_ context.Context, name string) error {
			h.created = append(h.created, name)
			h.reg.epoch++
			h.fleet.epoch = h.reg.epoch
			h.reg.store = append(h.reg.store, storeregistry.Entry{Name: name, Kind: storeregistry.KindStore, Store: name, State: storeregistry.StateActive,
				RelPath: "data/" + name, WrapperKind: storeregistry.WrapperServer, ChangedEpoch: h.reg.epoch})
			return nil
		},
	}
	return h
}

func roster(names ...string) []fleetMember {
	var out []fleetMember
	for _, n := range names {
		out = append(out, fleetMember{Name: n, SSH: n})
	}
	return out
}

func (h *rolloutHarness) do(apply bool, names ...string) *rolloutReport {
	h.t.Helper()
	h.run.Apply = apply
	return runRollout(context.Background(), h.run, roster(names...), installSteps())
}

// converged adds a machine that already runs the candidate and the registry.
func (h *rolloutHarness) converged(name string) *fakeMachine {
	return h.fleet.add(name, &fakeMachine{pinPresent: true, pinWant: newVer, pinOwner: "satellite-config", version: newVer, applied: 7, registryAt: 7})
}

func stepStatus(rep *rolloutReport, machine, step string) string {
	for _, m := range rep.Machines {
		if m.Name == machine {
			for _, s := range m.Steps {
				if s.Step == step {
					return s.Status
				}
			}
		}
	}
	return "<missing>"
}

func machineReport(rep *rolloutReport, name string) rolloutMachineReport {
	for _, m := range rep.Machines {
		if m.Name == name {
			return m
		}
	}
	return rolloutMachineReport{}
}

// ---------------------------------------------------------------------------
// plan
// ---------------------------------------------------------------------------

func TestRolloutPlan_ShowsEveryStepAndMachineAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.run.Store = "newstore"
	h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: newVer, version: oldVer, applied: 7, registryAt: 7})
	h.fleet.add("mini1", &fakeMachine{pinPresent: true, pinWant: oldVer, pinOwner: "satellite-config", version: oldVer, applied: 7, registryAt: 7})
	h.fleet.add("mini2", &fakeMachine{unreachable: true})

	rep := h.do(false, "mini0", "mini1", "mini2")

	if h.fleet.mutations() != 0 || len(h.created) != 0 || len(h.records.writes) != 0 {
		t.Fatalf("a plan changed something: mutations=%d created=%v writes=%d", h.fleet.mutations(), h.created, len(h.records.writes))
	}
	var out bytes.Buffer
	printRolloutReport(&out, rep, h.run, installSteps())
	text := out.String()
	for _, st := range installSteps() {
		if !strings.Contains(text, st.ID) || !strings.Contains(text, st.Pre) || !strings.Contains(text, st.Do) {
			t.Errorf("plan output lacks step %q or its definition text", st.ID)
		}
	}
	for _, name := range []string{"mini0", "mini1", "mini2"} {
		if machineReport(rep, name).Name == "" {
			t.Errorf("machine %s missing from the plan", name)
		}
	}
	if !strings.Contains(text, "PLAN") || !strings.Contains(text, "Nothing is changed") {
		t.Errorf("plan banner missing:\n%s", text)
	}
	// every machine was evaluated even though mini1 and mini2 fail: a plan shows all of them.
	if got := machineReport(rep, "mini0").Verdict; got != "pending" {
		t.Errorf("mini0 verdict = %q, want pending (the binary needs installing)", got)
	}
	if got := machineReport(rep, "mini1").Verdict; got != "refused" {
		t.Errorf("mini1 verdict = %q, want refused (pin)", got)
	}
	if got := machineReport(rep, "mini2").Verdict; got != "unreachable" {
		t.Errorf("mini2 verdict = %q, want unreachable", got)
	}
	if rep.Complete || strings.Contains(rep.Summary, "CONVERGED") {
		t.Errorf("a plan with failures must not claim convergence: %s", rep.Summary)
	}
	if !strings.Contains(rep.Summary, "UNREACHABLE mini2") {
		t.Errorf("summary should name the unreachable machine: %s", rep.Summary)
	}
}

func TestRolloutHelp_IsGeneratedFromTheStepDefinitions(t *testing.T) {
	for _, st := range append(installSteps(), rollbackSteps()...) {
		long := brainRolloutCmd.Long
		if st.Scope == "machine" && strings.Contains(st.ID, "restore") {
			long = brainRolloutRollbackCmd.Long
		}
		if !strings.Contains(long, st.ID) && !strings.Contains(brainRolloutRollbackCmd.Long, st.ID) {
			t.Errorf("--help does not mention step %q", st.ID)
		}
		if !strings.Contains(brainRolloutCmd.Long+brainRolloutRollbackCmd.Long, st.Pre) {
			t.Errorf("--help does not carry the precondition text of step %q", st.ID)
		}
	}
	if _, err := os.Stat("../../docs/brain/RUNBOOK_STORES_AND_DEPLOY.md"); err == nil {
		t.Error("the written runbook must not exist: the command is the single source of truth")
	}
}

func TestRolloutPlan_ConvergedFleetIsRealEvidence(t *testing.T) {
	h := newHarness(t)
	h.converged("mini0")
	h.converged("mini1")
	rep := h.do(false, "mini0", "mini1")
	if !rep.Complete || !strings.Contains(rep.Summary, "nothing to change") {
		t.Fatalf("converged fleet plan: complete=%v %s", rep.Complete, rep.Summary)
	}
	if h.fleet.mutations() != 0 {
		t.Fatal("a plan must not run anything that changes a machine")
	}
}

// ---------------------------------------------------------------------------
// stop at the first failure
// ---------------------------------------------------------------------------

func TestRolloutApply_StopsAtFirstFailureAndTouchesNothingAfterIt(t *testing.T) {
	h := newHarness(t)
	h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: newVer, version: oldVer, applied: 6, registryAt: 7})
	h.fleet.add("mini1", &fakeMachine{unreachable: true})
	mac := h.fleet.add("macbook", &fakeMachine{pinPresent: true, pinWant: newVer, version: oldVer})

	rep := h.do(true, "mini0", "mini1", "macbook")

	if rep.Stopped == nil || rep.Stopped.Machine != "mini1" || rep.Stopped.Step != "reach" {
		t.Fatalf("want a stop at mini1/reach, got %+v", rep.Stopped)
	}
	if len(mac.scripts) != 0 {
		t.Fatalf("macbook was touched after the failure: %v", mac.scripts)
	}
	if got := machineReport(rep, "macbook").Verdict; got != "not-evaluated" {
		t.Fatalf("macbook verdict = %q, want not-evaluated", got)
	}
	if !sliceHas(rep.NotCovered, "macbook") {
		t.Fatalf("macbook must be listed as not covered: %v", rep.NotCovered)
	}
	if rep.Complete || strings.Contains(rep.Summary, "CONVERGED") {
		t.Fatalf("a stopped run must not claim convergence: %s", rep.Summary)
	}
	if !strings.Contains(rep.Stopped.Fix, "ssh -o BatchMode=yes mini1 true") {
		t.Errorf("the stop must say what to do: %q", rep.Stopped.Fix)
	}
	// mini0 was fully processed before the stop, mini1 is recorded as unreachable.
	if got := h.records.rows["mini0"].Outcome; got != storeregistry.RolloutVerified {
		t.Errorf("mini0 outcome = %q, want verified", got)
	}
	if got := h.records.rows["mini1"].Outcome; got != storeregistry.RolloutUnreachable {
		t.Errorf("mini1 outcome = %q, want unreachable", got)
	}
	if _, ok := h.records.rows["macbook"]; ok {
		t.Error("macbook must not be recorded: nothing was checked on it")
	}
}

func TestRolloutApply_StopsWithinAMachineAtTheFailedStep(t *testing.T) {
	h := newHarness(t)
	m := h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: newVer, version: oldVer, applied: 7, registryAt: 7})
	m.storeBroken = true
	h.run.Store = ""
	h.reg.store = []storeregistry.Entry{{Name: "task", Kind: storeregistry.KindStore, Store: "task", State: storeregistry.StateActive, RelPath: "data/task", WrapperKind: storeregistry.WrapperServer, ChangedEpoch: 7}}
	rep := h.do(true, "mini0")
	if rep.Stopped == nil || rep.Stopped.Step != "prove" {
		t.Fatalf("want a stop at prove, got %+v", rep.Stopped)
	}
	if !strings.Contains(rep.Stopped.Reason, "task") || !strings.Contains(rep.Stopped.Reason, "by name") {
		t.Errorf("reason should name the store that does not read by name: %q", rep.Stopped.Reason)
	}
	if stepStatus(rep, "mini0", "record") != statBlocked {
		t.Errorf("record must be blocked after a failed prove, got %q", stepStatus(rep, "mini0", "record"))
	}
	if got := h.records.rows["mini0"].Outcome; got != storeregistry.RolloutFailed {
		t.Errorf("a machine that failed proof is recorded failed, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// the pin
// ---------------------------------------------------------------------------

func TestRolloutPin_MismatchIsRefusedWithTheExactInstruction(t *testing.T) {
	h := newHarness(t)
	m := h.fleet.add("mini1", &fakeMachine{pinPresent: true, pinWant: oldVer, pinOwner: "satellite-config", version: oldVer, applied: 7, registryAt: 7})

	rep := h.do(true, "mini1")

	if m.uploads != 0 {
		t.Fatal("the binary was installed behind a pin that would reject it")
	}
	if m.version != oldVer {
		t.Fatalf("installed version changed to %q", m.version)
	}
	if rep.Stopped == nil || rep.Stopped.Step != "pin" {
		t.Fatalf("want a stop at pin, got %+v", rep.Stopped)
	}
	for _, want := range []string{"converge-stores.sh", "BD_WANT", newVer, "mini1"} {
		if !strings.Contains(rep.Stopped.Fix, want) {
			t.Errorf("fix %q lacks %q", rep.Stopped.Fix, want)
		}
	}
	if !strings.Contains(rep.Stopped.Reason, oldVer) || !strings.Contains(rep.Stopped.Reason, newVer) {
		t.Errorf("reason should name both versions: %q", rep.Stopped.Reason)
	}
	if got := h.records.rows["mini1"]; got.Outcome != storeregistry.RolloutRefused || got.PinState != "stale" || got.PinWant != oldVer {
		t.Errorf("record = %+v, want refused / stale / %s", got, oldVer)
	}
	for _, s := range m.scripts {
		if s == "install" || s == "restore" {
			t.Errorf("script %q ran against a machine whose pin refuses the binary", s)
		}
	}
}

func TestRolloutPin_UnownedPinGetsAGenericInstruction(t *testing.T) {
	h := newHarness(t)
	h.fleet.add("mini1", &fakeMachine{pinPresent: true, pinWant: oldVer, version: oldVer, applied: 7, registryAt: 7})
	rep := h.do(false, "mini1")
	if rep.Failures[0].Step != "pin" || strings.Contains(rep.Failures[0].Fix, "satellite-config") {
		t.Fatalf("unexpected: %+v", rep.Failures)
	}
}

func TestRolloutPin_AbsentOrMatchingPinAllowsTheInstallAndKeepsThePreviousBinary(t *testing.T) {
	for name, pin := range map[string]bool{"absent": false, "matching": true} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			m := h.fleet.add("mini0", &fakeMachine{pinPresent: pin, pinWant: newVer, version: oldVer, applied: 6, registryAt: 7})
			rep := h.do(true, "mini0")
			if rep.Stopped != nil {
				t.Fatalf("stopped: %+v", rep.Stopped)
			}
			if m.uploads != 1 || m.version != newVer {
				t.Fatalf("not installed: uploads=%d version=%q", m.uploads, m.version)
			}
			if !strings.Contains(m.previous, "bd.pre-972952bf7") {
				t.Fatalf("the previous binary was not kept as bd.pre-<sha>: %q", m.previous)
			}
			if got := h.records.rows["mini0"].PreviousBinary; got != m.previous {
				t.Fatalf("the record must carry the kept binary for rollback: %q vs %q", got, m.previous)
			}
			if !rep.Complete {
				t.Fatalf("expected a verified machine: %s", rep.Summary)
			}
		})
	}
}

func TestRolloutBinary_FailedPinAfterInstallPutsThePreviousBinaryBack(t *testing.T) {
	h := newHarness(t)
	// The pin file is rewritten under us between check and verify: it now wants a third version.
	m := h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: newVer, version: oldVer, applied: 6, registryAt: 7})
	m.pinWant = newVer
	steps := installSteps()
	for i := range steps {
		if steps[i].ID == "binary" {
			orig := steps[i].act
			steps[i].act = func(ctx context.Context, r *rolloutRun, mr *machineRun) error {
				err := orig(ctx, r, mr)
				m.pinWant = "something else"
				return err
			}
		}
	}
	h.run.Apply = true
	rep := runRollout(context.Background(), h.run, roster("mini0"), steps)
	if rep.Stopped == nil || rep.Stopped.Step != "binary" {
		t.Fatalf("want a stop at binary, got %+v", rep.Stopped)
	}
	if m.restores != 1 || !strings.Contains(rep.Stopped.Reason, "put back") {
		t.Fatalf("the previous binary must be restored automatically: restores=%d reason=%q", m.restores, rep.Stopped.Reason)
	}
}

func TestRolloutBinary_PlatformMismatchIsRefused(t *testing.T) {
	h := newHarness(t)
	h.fleet.add("lnx", &fakeMachine{uname: "Linux x86_64", pinPresent: false, version: oldVer})
	rep := h.do(false, "lnx")
	if len(rep.Failures) != 1 || rep.Failures[0].Step != "binary" || !strings.Contains(rep.Failures[0].Reason, "darwin/arm64") {
		t.Fatalf("want a platform refusal, got %+v", rep.Failures)
	}
}

func TestRolloutBinary_ShimWithoutRealBinaryIsNotGuessed(t *testing.T) {
	f := &machineFacts{Bin: "/h/.local/bin/bd", BinKind: "script"}
	if _, _, err := f.installTarget(); err == nil {
		t.Fatal("a shim script with no bd.real must be refused")
	}
	f.BinReal, f.RealVersion = "/h/.local/bin/bd.real", oldVer
	path, ver, err := f.installTarget()
	if err != nil || path != "/h/.local/bin/bd.real" || ver != oldVer {
		t.Fatalf("shim layout: %v %q %q", err, path, ver)
	}
}

// ---------------------------------------------------------------------------
// client mode
// ---------------------------------------------------------------------------

func TestRolloutClient_IsCheckedThroughItsOwnBinaryNotItsHost(t *testing.T) {
	h := newHarness(t)
	// The MacBook is a client of mini1 and still runs the old build. Its host runs the new one.
	mac := h.fleet.add("macbook", &fakeMachine{pinPresent: false, version: oldVer, relay: "mini1@100.102.238.78", applied: 7, registryAt: 7})
	mac.reportVer = versionToken(newVer) // what its host would say if the question were forwarded

	rep := h.do(false, "macbook")

	if machineReport(rep, "macbook").Role != "client" {
		t.Fatalf("client mode not detected: %+v", machineReport(rep, "macbook"))
	}
	if !mac.envOff["probe"] {
		t.Error("the probe on a client must run its own binary with BRAIN_REMOTE=off")
	}
	if rep.Complete {
		t.Fatal("a client whose own binary is old must not be called converged because its host is new")
	}
	if got := stepStatus(rep, "macbook", "binary"); got != statTodo {
		t.Fatalf("binary step = %q, want todo (its own binary is %s)", got, oldVer)
	}
	for _, o := range machineReport(rep, "macbook").Steps {
		if o.Step == "mode" && !strings.Contains(o.Detail, "BRAIN_REMOTE=off") {
			t.Errorf("mode step should say how the client is checked: %q", o.Detail)
		}
	}
}

func TestRolloutClient_AnAnswerFromTheHostIsNotTheClientsAnswer(t *testing.T) {
	h := newHarness(t)
	mac := h.converged("macbook")
	mac.relay = "mini1@100.102.238.78"
	mac.reportName = "mini1" // the report came back from the host
	rep := h.do(false, "macbook")
	if len(rep.Failures) != 1 || rep.Failures[0].Step != "converge" && rep.Failures[0].Step != "prove" {
		t.Fatalf("want a failure at converge/prove, got %+v", rep.Failures)
	}
	if rep.Complete || !strings.Contains(rep.Failures[0].Reason, "not from macbook") {
		t.Fatalf("the host's answer must not count: %+v", rep.Failures)
	}
}

func TestRolloutClient_LocalBinaryOldMeansNotProvedEvenIfReportClaimsNew(t *testing.T) {
	h := newHarness(t)
	mac := h.converged("macbook")
	mac.relay = "mini1@100.102.238.78"
	mac.version = oldVer
	mac.pinWant = oldVer
	mac.reportVer = versionToken(newVer)
	rep := h.do(false, "macbook")
	if rep.Complete {
		t.Fatal("claimed converged with an old local binary")
	}
}

func clientWithNewestStore(h *rolloutHarness) *fakeMachine {
	h.reg.store = []storeregistry.Entry{{Name: "feature", Kind: storeregistry.KindStore, Store: "feature", State: storeregistry.StateActive, RelPath: "data/feature", WrapperKind: storeregistry.WrapperServer, ChangedEpoch: 7}}
	mac := h.converged("macbook")
	mac.relay = "mini1@100.102.238.78"
	return mac
}

func TestRolloutClient_IsProvedWithItsOwnPiecesAndTheRecordSaysSo(t *testing.T) {
	h := newHarness(t)
	mac := clientWithNewestStore(h)
	rep := h.do(true, "macbook")
	if !rep.Complete {
		t.Fatalf("a client with the build, the wrapper and the epoch must be proved: %+v %+v", rep.Failures, rep.Stopped)
	}
	// Its own sync and verify run through its own binary, never the forwarding 'brain' wrapper.
	for _, kind := range []string{"report"} {
		sc := mac.lastScripts[kind]
		if !strings.Contains(sc, "unset BRAIN_REMOTE_STORE") || strings.Contains(sc, "\nbrain ") || strings.Contains(sc, "BRAIN_REMOTE=off") {
			t.Errorf("%s script on a client must run bd directly and still read through the relay:\n%s", kind, sc)
		}
	}
	for _, kind := range []string{"wrapper", "storeread"} {
		if mac.lastScripts[kind] == "" {
			t.Errorf("client proof must run %s", kind)
		}
	}
	rec := h.records.rows["macbook"]
	if rec.Role != "client" || rec.Outcome != storeregistry.RolloutVerified || !strings.Contains(rec.Detail, "client of mini1@100.102.238.78") || !strings.Contains(rec.Detail, "'feature'") {
		t.Fatalf("record must say it is a client and what was proved: %+v", rec)
	}
}

func TestRolloutClient_ConvergeRunsTheMachinesOwnSyncNotTheHosts(t *testing.T) {
	h := newHarness(t)
	mac := clientWithNewestStore(h)
	mac.applied = 5
	h.do(true, "macbook")
	if mac.syncRuns != 1 {
		t.Fatalf("the client's own sync must run once, ran %d", mac.syncRuns)
	}
	sc := mac.lastScripts["sync"]
	if !strings.Contains(sc, "unset BRAIN_REMOTE_STORE") || !strings.Contains(sc, "stores sync") || strings.Contains(sc, "\nbrain stores") {
		t.Errorf("client sync must run through its own bd binary:\n%s", sc)
	}
	// What the host answered ('already applied epoch 7') is not accepted for the client.
	h2 := newHarness(t)
	m2 := clientWithNewestStore(h2)
	m2.reportName = "mini1"
	rep := h2.do(false, "macbook")
	if rep.Complete || len(rep.Failures) == 0 || rep.Failures[0].Step != "converge" || !strings.Contains(rep.Failures[0].Reason, "not from macbook") {
		t.Fatalf("converge must reject the host's report: %+v", rep.Failures)
	}
}

func TestRolloutClient_HostAnswerIsRejectedAtProve(t *testing.T) {
	h := newHarness(t)
	mac := clientWithNewestStore(h)
	mac.reportName = "mini1"
	rep := h.do(false, "macbook")
	if rep.Complete || !strings.Contains(rep.Failures[0].Reason, "not from macbook") {
		t.Fatalf("wrong-name answer must be rejected: %+v", rep.Failures)
	}
}

func TestRolloutClient_MissingWrapperFailsTheProof(t *testing.T) {
	h := newHarness(t)
	mac := clientWithNewestStore(h)
	mac.noWrapper = true
	rep := h.do(true, "macbook")
	if rep.Complete || rep.Stopped == nil || rep.Stopped.Step != "prove" || !strings.Contains(rep.Stopped.Reason, "wrapper for store 'feature'") {
		t.Fatalf("a missing wrapper must fail the proof: %+v %+v", rep.Stopped, rep.Failures)
	}
	if rec := h.records.rows["macbook"]; rec.Outcome == storeregistry.RolloutVerified {
		t.Fatalf("must not be recorded verified: %+v", rec)
	}
}

func TestRolloutClient_StoreThatDoesNotAnswerThroughTheHostFailsTheProof(t *testing.T) {
	h := newHarness(t)
	mac := clientWithNewestStore(h)
	mac.storeBroken = true
	if rep := h.do(false, "macbook"); rep.Complete || !strings.Contains(rep.Failures[0].Reason, "by name") {
		t.Fatalf("%+v", rep.Failures)
	}
}

func TestRolloutClient_ReportNotReadThroughTheRelayIsRefused(t *testing.T) {
	h := newHarness(t)
	mac := clientWithNewestStore(h)
	mac.cache = true // read from its cache, not the relay
	if rep := h.do(false, "macbook"); rep.Complete {
		t.Fatal("a cached answer proves nothing")
	}
}

func TestRolloutProve_EachClaimIsCheckedOnTheMachineItself(t *testing.T) {
	cases := map[string]struct {
		edit func(m *fakeMachine)
		want string
	}{
		"registry cache":    {func(m *fakeMachine) { m.cache = true }, "cache"},
		"report from other": {func(m *fakeMachine) { m.reportName = "elsewhere" }, "not from"},
		"report old build":  {func(m *fakeMachine) { m.reportVer = "0.9.0" }, "version"},
		"store unreadable":  {func(m *fakeMachine) { m.storeBroken = true }, "by name"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reg.store = []storeregistry.Entry{{Name: "task", Kind: storeregistry.KindStore, Store: "task", State: storeregistry.StateActive, RelPath: "data/task", WrapperKind: storeregistry.WrapperServer, ChangedEpoch: 7}}
			c.edit(h.converged("mini0"))
			rep := h.do(false, "mini0")
			if rep.Complete || len(rep.Failures) == 0 {
				t.Fatalf("must not be converged: %s", rep.Summary)
			}
			text := rep.Failures[0].Reason + " " + rep.Failures[0].Step
			if !strings.Contains(text, c.want) {
				t.Fatalf("reason %q should mention %q", text, c.want)
			}
		})
	}
}

// A machine that is behind is not "converged": the plan says a sync is needed, and
// an apply whose sync does not fix it fails at converge instead of claiming success.
func TestRolloutConverge_BehindMachineNeedsASyncAndIsNeverClaimedConverged(t *testing.T) {
	h := newHarness(t)
	m := h.converged("mini0")
	m.applied = 5
	m.missing = map[string][]string{"wrapper": {"newstore"}}
	rep := h.do(false, "mini0")
	if rep.Complete || stepStatus(rep, "mini0", "converge") != statTodo || stepStatus(rep, "mini0", "prove") != statPending {
		t.Fatalf("plan: complete=%v converge=%s prove=%s", rep.Complete, stepStatus(rep, "mini0", "converge"), stepStatus(rep, "mini0", "prove"))
	}
	// the machine's sync does not actually fix it (still missing a wrapper)
	rep = h.do(true, "mini0")
	if rep.Complete || rep.Stopped == nil || rep.Stopped.Step != "converge" || !strings.Contains(rep.Stopped.Reason, "newstore") {
		t.Fatalf("apply must stop at converge naming what is missing: %+v", rep.Stopped)
	}
}

// ---------------------------------------------------------------------------
// store step, records
// ---------------------------------------------------------------------------

func TestRolloutStore_CreatedOnceThenTheNewEpochIsTheTarget(t *testing.T) {
	h := newHarness(t)
	h.run.Store = "newstore"
	m := h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: newVer, version: newVer, applied: 7, registryAt: 7})
	rep := h.do(true, "mini0")
	if rep.Stopped != nil {
		t.Fatalf("stopped: %+v", rep.Stopped)
	}
	if len(h.created) != 1 || h.created[0] != "newstore" {
		t.Fatalf("created = %v", h.created)
	}
	if rep.Epoch != 8 || m.syncRuns != 1 || m.applied != 8 {
		t.Fatalf("target epoch = %d, syncs=%d applied=%d; want 8,1,8", rep.Epoch, m.syncRuns, m.applied)
	}
	if got := h.records.rows["mini0"]; got.Epoch != 8 || got.Outcome != storeregistry.RolloutVerified || got.VerifiedAt == "" {
		t.Fatalf("record = %+v", got)
	}
	// a second run finds the store registered and does not create it again
	h2 := h
	h2.created = nil
	_ = h2.do(true, "mini0")
	if len(h2.created) != 0 {
		t.Fatal("create ran twice")
	}
}

func TestRolloutStore_RefusedOnAClientAndOnABadName(t *testing.T) {
	h := newHarness(t)
	h.reg.role = "client"
	h.run.Store = "newstore"
	h.converged("mini0")
	rep := h.do(false, "mini0")
	if rep.Failures[0].Step != "store" || !strings.Contains(rep.Failures[0].Reason, "client") {
		t.Fatalf("want a client refusal, got %+v", rep.Failures)
	}
	h2 := newHarness(t)
	h2.run.Store = "Bad Name"
	rep = h2.do(false, "mini0")
	if rep.Failures[0].Step != "store" {
		t.Fatalf("want a name refusal, got %+v", rep.Failures)
	}
	if len(h.created)+len(h2.created) != 0 {
		t.Fatal("nothing may be created on a refusal")
	}
}

func TestRolloutRecord_CarriesVersionPinEpochAndTime(t *testing.T) {
	h := newHarness(t)
	h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: newVer, pinOwner: "satellite-config", version: oldVer, applied: 6, registryAt: 7})
	h.do(true, "mini0")
	got := h.records.rows["mini0"]
	want := storeregistry.Rollout{
		Machine: "mini0", Role: "direct", BdVersion: newVer, PinState: "ok", PinWant: newVer, Epoch: 7,
		Outcome: storeregistry.RolloutVerified, Step: "prove", Actor: "rollout:test", VerifiedAt: "2026-10-22 12:00:00",
	}
	for _, c := range []struct{ name, got, want string }{
		{"machine", got.Machine, want.Machine}, {"role", got.Role, want.Role}, {"version", got.BdVersion, want.BdVersion},
		{"pin state", got.PinState, want.PinState}, {"pin want", got.PinWant, want.PinWant}, {"outcome", got.Outcome, want.Outcome},
		{"step", got.Step, want.Step}, {"actor", got.Actor, want.Actor}, {"verified_at", got.VerifiedAt, want.VerifiedAt},
	} {
		if c.got != c.want {
			t.Errorf("record %s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if got.Epoch != 7 {
		t.Errorf("record epoch = %d", got.Epoch)
	}
	var out bytes.Buffer
	printRolloutRecords(&out, []storeregistry.Rollout{got})
	if !strings.Contains(out.String(), "mini0") || !strings.Contains(out.String(), "verified 2026-10-22") {
		t.Errorf("status view: %s", out.String())
	}
}

func TestRolloutRecord_AFailedRunKeepsTheEarlierVerifiedTime(t *testing.T) {
	h := newHarness(t)
	h.records.rows = map[string]storeregistry.Rollout{"mini1": {Machine: "mini1", Outcome: storeregistry.RolloutVerified, VerifiedAt: "2026-10-20 09:00:00", PreviousBinary: "/h/bd.pre-1"}}
	h.fleet.add("mini1", &fakeMachine{unreachable: true})
	h.do(true, "mini1")
	got := h.records.rows["mini1"]
	if got.Outcome != storeregistry.RolloutUnreachable || got.VerifiedAt != "2026-10-20 09:00:00" || got.PreviousBinary != "/h/bd.pre-1" {
		t.Fatalf("record = %+v", got)
	}
}

func TestRolloutRecords_UnavailableOnAClientStopsBeforeAnythingElse(t *testing.T) {
	h := newHarness(t)
	h.run.Records = nil
	m := h.converged("mini0")
	rep := h.do(true, "mini0")
	if rep.Stopped == nil || rep.Stopped.Step != "records" || len(m.scripts) != 0 {
		t.Fatalf("want an early stop at records with no machine touched: %+v %v", rep.Stopped, m.scripts)
	}
}

func TestRolloutApply_NeedsAnExplicitBinary(t *testing.T) {
	rolloutBinary = ""
	defer func() { rolloutBinary = "" }()
	if _, _, err := buildRolloutRun(context.Background(), true, false); err == nil || !strings.Contains(err.Error(), "--binary") {
		t.Fatalf("want an error naming --binary, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// rollback
// ---------------------------------------------------------------------------

func TestRolloutRollback_RestoresTheKeptBinaryOnlyWhenThePinAcceptsIt(t *testing.T) {
	t.Run("pin still wants the new version", func(t *testing.T) {
		h := newHarness(t)
		h.run.Rollback = true
		h.records.rows = map[string]storeregistry.Rollout{"mini0": {Machine: "mini0", PreviousBinary: "/home/mini0/.local/bin/bd.pre-972952bf7"}}
		m := h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: newVer, pinOwner: "satellite-config", version: newVer})
		h.run.Apply = true
		rep := runRollout(context.Background(), h.run, roster("mini0"), rollbackSteps())
		if m.restores != 0 || rep.Stopped == nil || rep.Stopped.Step != "pin" {
			t.Fatalf("rollback behind a pin that wants the new version must be refused: restores=%d %+v", m.restores, rep.Stopped)
		}
		if !strings.Contains(rep.Stopped.Fix, "restore.sh") {
			t.Errorf("fix should point at the converge backup: %q", rep.Stopped.Fix)
		}
	})
	t.Run("pin accepts it", func(t *testing.T) {
		h := newHarness(t)
		h.run.Rollback = true
		h.records.rows = map[string]storeregistry.Rollout{"mini0": {Machine: "mini0", PreviousBinary: "/home/mini0/.local/bin/bd.pre-972952bf7", VerifiedAt: "2026-10-21 00:00:00"}}
		m := h.fleet.add("mini0", &fakeMachine{pinPresent: true, pinWant: oldVer, version: newVer})
		h.run.Apply = true
		rep := runRollout(context.Background(), h.run, roster("mini0"), rollbackSteps())
		if rep.Stopped != nil || m.restores != 1 || m.version != oldVer {
			t.Fatalf("rollback: stopped=%+v restores=%d version=%q", rep.Stopped, m.restores, m.version)
		}
		if got := h.records.rows["mini0"]; got.Outcome != storeregistry.RolloutRolledBack || got.BdVersion != oldVer {
			t.Fatalf("record = %+v", got)
		}
	})
	t.Run("nothing was kept", func(t *testing.T) {
		h := newHarness(t)
		h.run.Rollback = true
		h.fleet.add("mini0", &fakeMachine{version: newVer})
		rep := runRollout(context.Background(), h.run, roster("mini0"), rollbackSteps())
		if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0].Fix, "--previous") {
			t.Fatalf("want an instruction naming --previous, got %+v", rep.Failures)
		}
	})
}

// ---------------------------------------------------------------------------
// the shell scripts, run for real against a temporary HOME
// ---------------------------------------------------------------------------

func fakeBdScript(version string) string {
	return "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'bd version " + version + "'; exit 0; fi\nexit 0\n"
}

func TestRolloutScripts_RealShellProbeInstallPinAndRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BRAIN_REMOTE", "")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	// the layout of mini0: bd is a shim, bd.real is the binary, the pin names bd
	write(filepath.Join(bin, "bd.real"), fakeBdScript(oldVer), 0o755)
	write(filepath.Join(bin, "bd"), "#!/bin/sh\nexec \"$HOME/.local/bin/bd.real\" \"$@\"\n", 0o755)
	pin := "# bd-pin: installed by satellite-config converge-stores.sh - edit the repo copy\n" +
		"BD_BIN=\"$HOME/.local/bin/bd\"\nBD_WANT='" + oldVer + "'\n" +
		"_bd_got=$(\"$BD_BIN\" --version 2>/dev/null | sed -n '1s/^bd version //p')\n" +
		"if [ \"$_bd_got\" != \"$BD_WANT\" ]; then echo \"bead: bd is '$_bd_got', wanted '$BD_WANT'\" >&2; exit 78; fi\nunset _bd_got\n"
	write(filepath.Join(bin, ".bd-pin.sh"), pin, 0o644)

	x := sshRolloutExec{self: "here"}
	m := fleetMember{Name: "here", SSH: "here"}
	ctx := context.Background()

	out, exit, err := x.Exec(ctx, m, scriptProbe, nil)
	if err != nil || exit != 0 {
		t.Fatalf("probe: %v exit %d: %s", err, exit, out)
	}
	f, err := parseMachineFacts(out)
	if err != nil {
		t.Fatal(err)
	}
	if !f.PinPresent || f.PinWant != oldVer || f.PinOwner != "satellite-config" || f.BinKind != "script" || f.BdVersion != oldVer || f.RealVersion != oldVer {
		t.Fatalf("facts = %+v", f)
	}
	target, _, err := f.installTarget()
	if err != nil || target != filepath.Join(bin, "bd.real") {
		t.Fatalf("install target = %q %v", target, err)
	}
	if out, _, _ := x.Exec(ctx, m, scriptPinCheck, nil); !strings.Contains(out, "pin-ok") {
		t.Fatalf("pin check before: %s", out)
	}

	// install: the new binary replaces bd.real, the old one is kept as bd.pre-<sha>
	newBody := fakeBdScript(newVer)
	script := scriptInstall(target, newVer, shaOfVersion(oldVer), int64(len(newBody)))
	out, exit, err = x.Exec(ctx, m, script, strings.NewReader(newBody))
	if err != nil || exit != 0 {
		t.Fatalf("install: %v exit %d: %s", err, exit, out)
	}
	kept := filepath.Join(bin, "bd.pre-972952bf7")
	if got, _ := os.ReadFile(kept); string(got) != fakeBdScript(oldVer) {
		t.Fatalf("previous binary not kept at %s", kept)
	}
	if got, _ := os.ReadFile(target); string(got) != newBody {
		t.Fatal("new binary not in place")
	}
	if !strings.Contains(out, "previous="+kept) {
		t.Fatalf("install did not report the kept file: %s", out)
	}
	// the pin still wants the old version: the wrappers would now refuse. This is what the pin step prevents.
	if out, _, _ := x.Exec(ctx, m, scriptPinCheck, nil); strings.Contains(out, "pin-ok") || !strings.Contains(out, "wanted") {
		t.Fatalf("pin check after a swap behind the pin must fail, got %q", out)
	}

	// a different file already named bd.pre-<sha> is never overwritten
	write(kept, "someone else's backup", 0o755)
	body3 := fakeBdScript("3.0 (ccc0003)")
	_, exit, _ = x.Exec(ctx, m, scriptInstall(target, "3.0 (ccc0003)", shaOfVersion(newVer), int64(len(body3))), strings.NewReader(body3))
	if exit != 0 {
		t.Fatal("second install failed")
	}
	if got, _ := os.ReadFile(kept); string(got) != "someone else's backup" {
		t.Fatal("an existing backup was overwritten")
	}

	// an upload that is the wrong size is rejected and leaves the target alone
	before, _ := os.ReadFile(target)
	_, exit, _ = x.Exec(ctx, m, scriptInstall(target, newVer, "x", 999), strings.NewReader(newBody))
	if exit == 0 {
		t.Fatal("a truncated upload must be rejected")
	}
	if after, _ := os.ReadFile(target); string(after) != string(before) {
		t.Fatal("target changed by a rejected upload")
	}
	// an upload that reports the wrong version is rejected
	_, exit, _ = x.Exec(ctx, m, scriptInstall(target, "9.9 (zzz)", "x", int64(len(newBody))), strings.NewReader(newBody))
	if exit == 0 {
		t.Fatal("a binary reporting another version must be rejected")
	}
	entries, _ := os.ReadDir(bin)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bd.new") {
			t.Errorf("temporary upload left behind: %s", e.Name())
		}
	}

	// restore the first kept binary
	pre := filepath.Join(bin, "bd.pre-972952bf7")
	write(pre, fakeBdScript(oldVer), 0o755)
	out, exit, err = x.Exec(ctx, m, scriptRestore(target, pre, oldVer, "ccc0003"), nil)
	if err != nil || exit != 0 {
		t.Fatalf("restore: %v %d %s", err, exit, out)
	}
	if out, _, _ := x.Exec(ctx, m, scriptPinCheck, nil); !strings.Contains(out, "pin-ok") {
		t.Fatalf("after restore the pin must accept the binary again: %s", out)
	}
	if _, err := os.Stat(filepath.Join(bin, "bd.post-ccc0003")); err != nil {
		t.Fatalf("the binary a restore replaces must be kept: %v", err)
	}
}

func TestRolloutScripts_ProbeOnAClientAndWithoutAPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, ".local", "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.MkdirAll(filepath.Join(home, ".config", "brain"), 0o755)
	_ = os.WriteFile(filepath.Join(bin, "bd"), []byte(fakeBdScriptBinary(oldVer)), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".config", "brain", "remote.yaml"), []byte("host: mini1@100.102.238.78\n"), 0o644)
	out, exit, err := sshRolloutExec{self: "here"}.Exec(context.Background(), fleetMember{Name: "here", SSH: "here"}, scriptProbe, nil)
	if err != nil || exit != 0 {
		t.Fatalf("probe: %v %d %s", err, exit, out)
	}
	f, err := parseMachineFacts(out)
	if err != nil {
		t.Fatal(err)
	}
	if f.PinPresent || f.Relay != "mini1@100.102.238.78" || f.BdVersion != oldVer {
		t.Fatalf("facts = %+v", f)
	}
	if _, err := parseMachineFacts("garbage"); err == nil {
		t.Fatal("garbage must not parse as facts")
	}
}

// fakeBdScriptBinary is a script posing as a binary: its first bytes are not "#!"-detectable here,
// so the probe treats it as a script; the test only needs the version.
func fakeBdScriptBinary(v string) string { return fakeBdScript(v) }

func TestSSHRolloutExec_TransportFailureIsNotAScriptFailure(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'ssh: connect to host x port 22: Connection refused' >&2\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRemoteSSH, fake)
	_, exit, err := sshRolloutExec{self: "elsewhere"}.Exec(context.Background(), fleetMember{Name: "x", SSH: "x"}, "true", nil)
	if err == nil || exit != 255 || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("want a transport error carrying ssh's message, got exit=%d err=%v", exit, err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("unexpected")
	}
}

func TestRolloutCommand_RunsLocallyOnAClientAndSkipsTheRideAlong(t *testing.T) {
	for _, args := range [][]string{{"rollout"}, {"brain", "rollout"}, {"rollout", "rollback", "--apply"}, {"rollout", "status"}} {
		if !registryLocalVerb(args) {
			t.Errorf("%v must always run on this machine, never be forwarded to the host", args)
		}
	}
	for _, sub := range []*cobra.Command{brainRolloutCmd, brainRolloutRollbackCmd, brainRolloutStatusCmd} {
		if !registryRideAlongSkipped(sub) {
			t.Errorf("%s must not trigger the registry ride-along while it rolls the registry out", sub.Name())
		}
		if !hostGateAllows(sub, roleMoved) {
			t.Errorf("%s must work on a machine that is no longer the host", sub.Name())
		}
	}
	found, _, err := rootCmd.Find([]string{"brain", "rollout", "rollback"})
	if err != nil || found != brainRolloutRollbackCmd {
		t.Fatalf("brain rollout rollback not registered: %v", err)
	}
	if c, _, _ := rootCmd.Find([]string{"brain", "runbook"}); c != nil && c.Name() == "runbook" {
		t.Error("brain runbook must be gone: the command is the single source of truth")
	}
}

func TestHostClientsList_ShowsTheRolloutRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostClientsRollouts = func(context.Context) []storeregistry.Rollout {
		return []storeregistry.Rollout{{Machine: "macbook", Outcome: storeregistry.RolloutRefused, BdVersion: oldVer, PinState: "stale", Epoch: 7, Step: "pin", Detail: "pin wants old"}}
	}
	defer func() { hostClientsRollouts = readRolloutRecordsBestEffort }()
	out := rolloutCaptureStdout(t, func() {
		if err := brainHostClientsListCmd.RunE(brainHostClientsListCmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"macbook", "refused", oldVer, "pin stale", "pin wants old"} {
		if !strings.Contains(out, want) {
			t.Errorf("clients list lacks %q:\n%s", want, out)
		}
	}
}

func rolloutCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	done := make(chan string)
	go func() { b, _ := io.ReadAll(rd); done <- string(b) }()
	fn()
	_ = wr.Close()
	os.Stdout = old
	return <-done
}
