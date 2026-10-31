package main

// 'brain stores doctor --fleet': proof that the fleet converged. For every
// machine in the roster it reads the registry epoch and the generated
// artefacts (read-only, over ssh) and reports converged / lagging / drifted,
// with the exact fix command. It exits non-zero unless the fleet converged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/steveyegge/beads/internal/storeregistry"
)

const fleetRosterPath = ".config/brain/fleet.yaml"

// fleetMember is one machine of the roster.
type fleetMember struct {
	Name string `yaml:"name"`
	// SSH is the ssh target (default: the name, resolved by ~/.ssh/config).
	SSH string `yaml:"ssh,omitempty"`
	// Command is how to run brain there (default puts ~/.local/bin on PATH).
	Command string `yaml:"command,omitempty"`
	// StoreHost is the host:port this machine's store-host.env is expected to
	// hold when that is not the live host itself: a machine that reaches the
	// live Dolt server through a local forwarder declares 127.0.0.1:<port>.
	StoreHost string `yaml:"store_host,omitempty"`
}

type fleetRoster struct {
	Machines []fleetMember `yaml:"machines"`
}

var defaultFleetRoster = []fleetMember{{Name: "mini0"}, {Name: "mini1"}, {Name: "macbook"}}

// loadFleetRoster reads the roster file (default ~/.config/brain/fleet.yaml,
// else mini0, mini1, macbook), optionally narrowed by --machines.
func loadFleetRoster(path string, only []string) ([]fleetMember, error) {
	members := defaultFleetRoster
	if path == "" {
		path = homeFile(fleetRosterPath)
	}
	if data, err := os.ReadFile(path); err == nil {
		var r fleetRoster
		if err := yaml.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("parsing roster %s: %w", path, err)
		}
		if len(r.Machines) > 0 {
			members = r.Machines
		}
	} else if !errors.Is(err, os.ErrNotExist) || fleetRosterExplicit {
		return nil, fmt.Errorf("reading roster %s: %w", path, err)
	}
	var out []fleetMember
	for _, m := range members {
		if !storeregistry.ValidName(m.Name) {
			return nil, fmt.Errorf("roster machine name %q is not valid", m.Name)
		}
		if m.SSH == "" {
			m.SSH = m.Name
		}
		if strings.HasPrefix(m.SSH, "-") || strings.ContainsAny(m.SSH, " \t\n;&|$`'\"") {
			return nil, fmt.Errorf("roster machine %s: invalid ssh target %q", m.Name, m.SSH)
		}
		if m.StoreHost != "" {
			if _, err := parseHostAddr(m.StoreHost); err != nil {
				return nil, fmt.Errorf("roster machine %s: store_host: %w", m.Name, err)
			}
		}
		if len(only) > 0 && !sliceHas(only, m.Name) {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("the roster selects no machines")
	}
	return out, nil
}

func sliceHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

const defaultFleetCommand = `PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:$PATH" brain`

// fleetRunner runs `brain <args>` on a roster machine and returns its stdout.
// Tests replace it with an in-process fake.
type fleetRunner func(ctx context.Context, m fleetMember, args ...string) ([]byte, error)

func sshFleetRunner(ctx context.Context, m fleetMember, args ...string) ([]byte, error) {
	command := m.Command
	if command == "" {
		command = defaultFleetCommand
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = remoteQuote(a)
	}
	script := command + " " + strings.Join(quoted, " ")
	c := exec.CommandContext(ctx, sshBinary(), "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", //nolint:gosec // roster is the operator's file, validated above
		"-o", "ControlMaster=no", "-T", m.SSH, "--", script)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	c.WaitDelay = 2 * time.Second
	err := c.Run()
	if err != nil {
		var ee *exec.ExitError
		// Exit 1 from 'verify' means "not converged": the JSON on stdout is the answer.
		if errors.As(err, &ee) && ee.ExitCode() != sshConnectFailed && stdout.Len() > 0 {
			return stdout.Bytes(), nil
		}
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Machine verdicts.
const (
	verdictConverged   = "converged"
	verdictAsleepOK    = "asleep-ok"
	verdictLagging     = "lagging"
	verdictDrifted     = "drifted"
	verdictUnreachable = "unreachable"
)

type fleetMachineResult struct {
	Name    string        `json:"name"`
	SSH     string        `json:"ssh"`
	Verdict string        `json:"verdict"`
	Detail  string        `json:"detail"`
	Fix     string        `json:"fix,omitempty"`
	Report  *verifyReport `json:"report,omitempty"`
	LastAck string        `json:"last_ack,omitempty"`
}

type fleetReport struct {
	Epoch      int64                `json:"epoch"`
	Digest     string               `json:"digest"`
	Machines   []fleetMachineResult `json:"machines"`
	Consistent []string             `json:"registry_problems,omitempty"`
	Retired    []string             `json:"retired,omitempty"`
	Converged  bool                 `json:"converged"`
	Summary    string               `json:"summary"`
	Failing    []string             `json:"failing_machines,omitempty"`
	// Audit is the host/pin/probe/orphan half of the fleet view (nil with --no-audit).
	Audit *fleetAuditReport `json:"audit,omitempty"`
}

// asleepWindow: an unreachable machine whose last acknowledgement is at the
// current epoch missed nothing, however long ago it spoke.
func classifyFleetMachine(m fleetMember, snap storeregistry.Snapshot, ack *storeregistry.Machine, rep *verifyReport, runErr error) fleetMachineResult {
	r := fleetMachineResult{Name: m.Name, SSH: m.SSH, Report: rep}
	if ack != nil {
		r.LastAck = fmt.Sprintf("%s at epoch %d", ack.CheckedAt, ack.AppliedEpoch)
	}
	fix := "brain stores sync --machine " + m.Name
	if runErr != nil || rep == nil {
		msg := "unreachable"
		if runErr != nil {
			msg += ": " + firstErrLine(runErr)
		}
		if ack != nil && ack.AppliedEpoch == snap.State.Epoch && ack.AppliedDigest == snap.State.Digest {
			r.Verdict = verdictAsleepOK
			r.Detail = msg + "; its last report (" + r.LastAck + ") is current, so it missed no change"
			return r
		}
		r.Verdict = verdictUnreachable
		if ack != nil {
			r.Detail = msg + "; last report " + r.LastAck + ", database is at epoch " + fmt.Sprint(snap.State.Epoch)
		} else {
			r.Detail = msg + "; it has never reported"
		}
		r.Fix = "wake or reach " + m.Name + ", then: " + fix
		return r
	}
	switch {
	case rep.Error != "" && rep.AppliedEpoch < snap.State.Epoch:
		r.Verdict = verdictLagging
		r.Detail = fmt.Sprintf("applied epoch %d, database is at %d (%s)", rep.AppliedEpoch, snap.State.Epoch, rep.Error)
		r.Fix = fix
	case rep.AppliedEpoch < snap.State.Epoch || (rep.AppliedEpoch == snap.State.Epoch && rep.AppliedDigest != snap.State.Digest):
		r.Verdict = verdictLagging
		r.Detail = fmt.Sprintf("applied epoch %d, database is at %d", rep.AppliedEpoch, snap.State.Epoch)
		if names := flattenMissing(rep.Missing); len(names) > 0 {
			r.Detail += "; missing " + strings.Join(names, " ")
		}
		r.Fix = fix
	case !rep.Converged():
		r.Verdict = verdictDrifted
		var parts []string
		if names := flattenMissing(rep.Missing); len(names) > 0 {
			parts = append(parts, "missing "+strings.Join(names, " "))
		}
		for _, p := range rep.Probes {
			if p.Status == "fail" {
				parts = append(parts, fmt.Sprintf("%s does not read (%s)", p.Name, p.Reason))
			}
		}
		if rep.Error != "" {
			parts = append(parts, rep.Error)
		}
		r.Detail = fmt.Sprintf("at epoch %d but %s", rep.AppliedEpoch, strings.Join(parts, "; "))
		r.Fix = fix
	default:
		r.Verdict = verdictConverged
		r.Detail = fmt.Sprintf("epoch %d, %d/%d wrappers", rep.AppliedEpoch, rep.WrappersOK, rep.Active)
		if len(rep.Orphans) > 0 {
			r.Detail += "; note: unmanaged wrappers of removed stores left alone: " + strings.Join(rep.Orphans, " ")
		}
	}
	return r
}

// registryConsistencyProblems cross-checks the registry with the namespace
// record in the same database. A mismatch is a registry bug and is reported first.
func registryConsistencyProblems(ctx context.Context, q storeregistry.DBTX, entries []storeregistry.Entry) []string {
	var problems []string
	known := map[string]bool{} // canonical store -> any entry (any state) names it
	for _, e := range entries {
		known[e.Store] = true
	}
	rows, err := q.QueryContext(ctx, "SELECT `store`, registered FROM brain_stores")
	if err != nil {
		return nil // legacy database: no namespace record to compare with
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var s string
		var reg int
		if rows.Scan(&s, &reg) != nil {
			continue
		}
		have[s] = true
		if reg == 1 && !known[s] {
			problems = append(problems, fmt.Sprintf("store %q is registered in the database but not in the registry (run 'brain stores reconcile')", s))
		}
	}
	for _, e := range storeregistry.Active(entries) {
		if e.Kind == storeregistry.KindStore && !have[e.Store] {
			problems = append(problems, fmt.Sprintf("registry store %q has no brain_stores row", e.Store))
		}
	}
	sort.Strings(problems)
	return problems
}

// runFleetDoctor builds the fleet report. src supplies the database view;
// run reaches the machines (the local one directly via local).
func runFleetDoctor(ctx context.Context, src registrySource, db storeregistry.DBTX, roster []fleetMember,
	local func(ctx context.Context) verifyReport, run fleetRunner, timeout time.Duration) (*fleetReport, error) {
	snap, found, err := src.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("the registry is unreadable: %w", err)
	}
	if !found {
		return nil, errors.New("the registry is not adopted in this database (run 'brain stores reconcile')")
	}
	acks, _ := src.Machines(ctx)
	ackBy := map[string]*storeregistry.Machine{}
	rep := &fleetReport{Epoch: snap.State.Epoch, Digest: snap.State.Digest}
	for i := range acks {
		if acks[i].Retired {
			rep.Retired = append(rep.Retired, acks[i].Machine)
			continue
		}
		ackBy[acks[i].Machine] = &acks[i]
	}
	if db != nil {
		rep.Consistent = registryConsistencyProblems(ctx, db, snap.Entries)
	}

	self := machineName()
	results := make([]fleetMachineResult, len(roster))
	var wg sync.WaitGroup
	for i, m := range roster {
		wg.Add(1)
		go func(i int, m fleetMember) {
			defer wg.Done()
			mctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			var v *verifyReport
			var rerr error
			if m.Name == self && local != nil {
				lv := local(mctx)
				v = &lv
			} else {
				var out []byte
				out, rerr = run(mctx, m, "stores", "verify", "--json", "--probe", "3")
				if rerr == nil || len(out) > 0 {
					start := bytes.IndexByte(out, '{')
					if start >= 0 {
						var pv verifyReport
						if jerr := json.Unmarshal(out[start:], &pv); jerr == nil {
							v, rerr = &pv, nil
						} else if rerr == nil {
							rerr = jerr
						}
					} else if rerr == nil {
						rerr = fmt.Errorf("no JSON from %s: %q", m.Name, strings.TrimSpace(string(out)))
					}
				}
			}
			results[i] = classifyFleetMachine(m, snap, ackBy[m.Name], v, rerr)
		}(i, m)
	}
	wg.Wait()
	rep.Machines = results

	ok := len(rep.Consistent) == 0
	var bad []string
	for _, r := range results {
		switch r.Verdict {
		case verdictConverged, verdictAsleepOK:
		default:
			ok = false
			bad = append(bad, r.Name)
		}
	}
	rep.Converged = ok
	rep.Failing = bad
	conv := 0
	for _, r := range results {
		if r.Verdict == verdictConverged || r.Verdict == verdictAsleepOK {
			conv++
		}
	}
	short := rep.Digest
	if len(short) > 12 {
		short = short[:12]
	}
	if ok {
		rep.Summary = fmt.Sprintf("CONVERGED epoch %d digest %s… on %d/%d machines", rep.Epoch, short, conv, len(results))
	} else {
		var why []string
		for _, r := range results {
			if r.Verdict != verdictConverged && r.Verdict != verdictAsleepOK {
				why = append(why, fmt.Sprintf("%s %s (%s)", r.Name, r.Verdict, r.Detail))
			}
		}
		for _, p := range rep.Consistent {
			why = append(why, "registry: "+p)
		}
		rep.Summary = fmt.Sprintf("NOT CONVERGED at epoch %d: %s", rep.Epoch, strings.Join(why, " ; "))
	}
	return rep, nil
}

var (
	doctorFleet         bool
	fleetRoster_        string
	fleetMachines       []string
	fleetTimeout        time.Duration
	fleetRosterExplicit bool

	fleetNoAudit      bool
	fleetNoWriteProbe bool
	fleetRepair       bool
	fleetLiveHost     string
	fleetOldHosts     []string
	fleetBackupMax    time.Duration
)

// fleetJSONRequested reports whether the fleet report should be JSON. The
// doctor command's local --json flag shadows the global one, so honor both.
func fleetJSONRequested() bool { return jsonOutput || storesDoctorJSON }

func runBrainStoresDoctorFleet(cmd *cobra.Command) {
	ctx := rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	fleetRosterExplicit = fleetRoster_ != ""
	roster, err := loadFleetRoster(fleetRoster_, fleetMachines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	src, err := registrySourceForCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot read the registry: %v\nFAILING MACHINES: (database unreadable)\n", err)
		os.Exit(2)
	}
	var db storeregistry.DBTX
	if ds, ok := src.(dbSource); ok {
		db = ds.db
	}
	local := func(c context.Context) verifyReport { return buildVerifyReport(c, src, 3) }
	rep, err := runFleetDoctor(ctx, src, db, roster, local, sshFleetRunner, fleetTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\nFAILING MACHINES: (database unreadable)\n", err)
		os.Exit(2)
	}
	if !fleetNoAudit {
		ao, aerr := fleetAuditOptionsFromFlags(ctx)
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", aerr)
			os.Exit(2)
		}
		rep.Audit = runFleetAudit(ctx, roster, sshFleetRunner, ao, fleetTimeout)
	}
	if fleetJSONRequested() {
		_ = outputJSON(rep)
	} else {
		printFleetReport(os.Stdout, rep)
		if rep.Audit != nil {
			printFleetAudit(os.Stdout, rep.Audit)
		}
	}
	auditClean := rep.Audit == nil || rep.Audit.Clean
	if !rep.Converged || !auditClean {
		if len(rep.Failing) > 0 {
			fmt.Fprintf(os.Stderr, "FAILING MACHINES: %s\n", strings.Join(rep.Failing, " "))
		} else if !rep.Converged {
			fmt.Fprintln(os.Stderr, "FAILING MACHINES: (registry inconsistent)")
		}
		if !auditClean {
			fmt.Fprintf(os.Stderr, "FLEET DRIFT: %d item(s) on %s\n", len(rep.Audit.Drift), strings.Join(driftMachines(rep.Audit.Drift), " "))
		}
		os.Exit(1)
	}
}

func printFleetReport(w interface{ Write([]byte) (int, error) }, rep *fleetReport) {
	for _, p := range rep.Consistent {
		fmt.Fprintf(w, "REGISTRY  %s\n", p)
	}
	for _, r := range rep.Machines {
		fmt.Fprintf(w, "%-12s %-11s %s\n", r.Name, strings.ToUpper(r.Verdict), r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(w, "%-12s %-11s fix: %s\n", "", "", r.Fix)
		}
	}
	if len(rep.Retired) > 0 {
		fmt.Fprintf(w, "retired (not checked): %s\n", strings.Join(rep.Retired, " "))
	}
	fmt.Fprintln(w, rep.Summary)
}

// runFleetMachineSync runs `brain stores sync` on a roster machine over ssh
// ('brain stores sync --machine NAME').
func runFleetMachineSync(ctx context.Context, name string, dryRun bool) int {
	roster, err := loadFleetRoster("", []string{name})
	if err != nil {
		fmt.Fprintf(os.Stderr, "brain stores sync --machine %s: %v\n", name, err)
		return 2
	}
	args := []string{"stores", "sync"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	tctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := sshFleetRunner(tctx, roster[0], args...)
	_, _ = os.Stdout.Write(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "brain stores sync --machine %s: %v\n", name, err)
		return 1
	}
	return 0
}

func init() {
	brainStoresDoctorCmd.Flags().BoolVar(&doctorFleet, "fleet", false,
		"Prove the whole fleet converged: read the registry epoch and generated artefacts on every roster machine (read-only, over ssh)")
	brainStoresDoctorCmd.Flags().StringVar(&fleetRoster_, "roster", "", "Roster file (default ~/.config/brain/fleet.yaml, else mini0, mini1, macbook)")
	brainStoresDoctorCmd.Flags().StringSliceVar(&fleetMachines, "machines", nil, "Only these roster machines")
	brainStoresDoctorCmd.Flags().BoolVar(&fleetNoAudit, "no-audit", false, "With --fleet: skip the host audit (stamp, store-host, pins, probes, backups, orphans) and run only the convergence check")
	brainStoresDoctorCmd.Flags().BoolVar(&fleetNoWriteProbe, "no-write-probe", false, "With --fleet: skip the per-store write probe (read probes still run)")
	brainStoresDoctorCmd.Flags().BoolVar(&fleetRepair, "repair", false, "With --fleet: rewrite drifted wrapper host pins on reachable machines through 'stores rewrite-wrappers' (never installs binaries, edits store-host.env, touches Dolt servers or acts on an unreachable machine)")
	brainStoresDoctorCmd.Flags().StringVar(&fleetLiveHost, "live-host", "", "With --fleet: the live store host as host:port (default: this machine's store-host.env)")
	brainStoresDoctorCmd.Flags().StringSliceVar(&fleetOldHosts, "old-host", nil, "With --fleet: an old/other Dolt server (host:port) to check for orphan beads missing on the live host; repeatable")
	brainStoresDoctorCmd.Flags().DurationVar(&fleetBackupMax, "backup-max-age", 48*time.Hour, "With --fleet: a store whose last backup is older than this is drift (0 disables)")
	brainStoresDoctorCmd.Flags().DurationVar(&fleetTimeout, "fleet-timeout", 30*time.Second, "Per-machine time limit for --fleet")
}
