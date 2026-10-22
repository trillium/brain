package main

// brain rollout: the single procedure for "create a store, put the binary that
// knows about it on every machine, prove every machine has it". It replaces a
// written runbook: each step has a precondition, an action and a verification,
// the run stops at the first one that fails and says what to do, and what it
// established is recorded in the shared database. The text a person reads
// (--help, the plan) is generated from the same step values the engine runs.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/storeregistry"
)

var (
	rolloutApply    bool
	rolloutBinary   string
	rolloutStore    string
	rolloutPrevious string
	rolloutRoster   string
	rolloutMachines []string
)

// stepOutcome is one line of the report.
type stepOutcome struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

type rolloutMachineReport struct {
	Name    string        `json:"name"`
	SSH     string        `json:"ssh"`
	Role    string        `json:"role,omitempty"`
	Verdict string        `json:"verdict"` // verified | pending | refused | unreachable | failed | not-evaluated
	Steps   []stepOutcome `json:"steps"`
}

type rolloutStop struct {
	Machine string `json:"machine,omitempty"`
	Step    string `json:"step"`
	Reason  string `json:"reason"`
	Fix     string `json:"fix,omitempty"`
}

type rolloutReport struct {
	Mode       string                 `json:"mode"` // plan | apply | rollback-plan | rollback
	Candidate  *rolloutCandidate      `json:"candidate,omitempty"`
	Epoch      int64                  `json:"epoch"`
	Fleet      []stepOutcome          `json:"fleet"`
	Machines   []rolloutMachineReport `json:"machines"`
	Stopped    *rolloutStop           `json:"stopped,omitempty"`
	Failures   []rolloutStop          `json:"failures,omitempty"`
	NotCovered []string               `json:"not_covered,omitempty"`
	Complete   bool                   `json:"complete"` // every roster machine verified by itself
	Summary    string                 `json:"summary"`
}

func (r *rolloutReport) failed() bool { return r.Stopped != nil || len(r.Failures) > 0 }

func rolloutBlocked(on string) stepOutcome {
	return stepOutcome{Status: statBlocked, Detail: "not evaluated: " + on + " did not pass"}
}

// execStep runs one step: precondition, then (with --apply, when a change is
// needed) the action, then the verification. In a plan, the action never runs
// and a verification runs only when there is nothing to change, so a plan of a
// converged fleet is real evidence and a plan of an unconverged one is honest
// about what it has not checked.
func (r *rolloutRun) execStep(ctx context.Context, st rolloutStep, mr *machineRun) stepOutcome {
	out := stepOutcome{Step: st.ID}
	res := st.check(ctx, r, mr)
	out.Detail, out.Fix = res.Detail, res.Fix
	switch res.State {
	case stFail:
		out.Status = statFail
		return out
	case stSkip:
		out.Status = statSkip
		if strings.HasPrefix(res.Detail, "pending") {
			out.Status = statPending
		}
		return out
	case stNeed:
		if !r.Apply {
			out.Status = statTodo
			if mr != nil && !st.bookkeeping {
				mr.Pending = true
			}
			return out
		}
		if err := st.act(ctx, r, mr); err != nil {
			out.Status = statFail
			out.Detail = fmt.Sprintf("%s failed: %s", st.ID, firstErrLine(err))
			out.Fix = "read the message above, fix its cause, and re-run; steps are idempotent"
			return out
		}
		out.Status = statDone
	default:
		out.Status = statOK
	}
	if st.verify == nil || res.State == stHave && !st.verifyHave {
		return out
	}
	v := st.verify(ctx, r, mr)
	if v.State == stFail {
		out.Status = statFail
		out.Detail, out.Fix = v.Detail, v.Fix
		if st.undo != nil && res.State == stNeed {
			if uerr := st.undo(ctx, r, mr); uerr != nil {
				out.Detail += "; AND the automatic restore failed: " + firstErrLine(uerr)
			} else {
				out.Detail += "; the previous binary was put back"
			}
		}
		return out
	}
	if v.Detail != "" {
		out.Detail = v.Detail
	}
	return out
}

func machineVerdict(steps []stepOutcome, pending bool) string {
	for _, s := range steps {
		if s.Status != statFail {
			continue
		}
		switch s.Step {
		case "reach":
			return "unreachable"
		case "pin":
			return "refused"
		}
		return "failed"
	}
	if pending {
		return "pending"
	}
	return "verified"
}

// runRollout runs every fleet step, then every machine's steps in roster order.
// With --apply it stops at the first failure; in a plan it carries on to the
// next machine (each machine's checks are independent and read-only) and
// reports the first failure as where --apply would stop.
func runRollout(ctx context.Context, r *rolloutRun, roster []fleetMember, steps []rolloutStep) *rolloutReport {
	rep := &rolloutReport{Candidate: r.Cand}
	switch {
	case r.Rollback && r.Apply:
		rep.Mode = "rollback"
	case r.Rollback:
		rep.Mode = "rollback-plan"
	case r.Apply:
		rep.Mode = "apply"
	default:
		rep.Mode = "plan"
	}
	stop := func(machine string, o stepOutcome) {
		s := rolloutStop{Machine: machine, Step: o.Step, Reason: o.Detail, Fix: o.Fix}
		rep.Failures = append(rep.Failures, s)
		if r.Apply && rep.Stopped == nil {
			rep.Stopped = &s
		}
	}

	var fleetFail *stepOutcome
	for _, st := range steps {
		if st.Scope != "fleet" {
			continue
		}
		if fleetFail != nil {
			o := rolloutBlocked(fleetFail.Step)
			o.Step = st.ID
			rep.Fleet = append(rep.Fleet, o)
			continue
		}
		o := r.execStep(ctx, st, nil)
		rep.Fleet = append(rep.Fleet, o)
		if o.Status == statFail {
			f := o
			fleetFail = &f
			stop("", o)
		}
	}
	rep.Epoch = r.Epoch
	if fleetFail != nil {
		for _, m := range roster {
			mrep := rolloutMachineReport{Name: m.Name, SSH: m.SSH, Verdict: "not-evaluated"}
			for _, st := range steps {
				if st.Scope == "machine" {
					o := rolloutBlocked(fleetFail.Step)
					o.Step = st.ID
					mrep.Steps = append(mrep.Steps, o)
				}
			}
			rep.Machines = append(rep.Machines, mrep)
		}
		rep.Summary = "NOT STARTED: " + fleetFail.Detail
		return rep
	}

	halted := false
	verified := 0
	for _, m := range roster {
		mr := &machineRun{M: m}
		mrep := rolloutMachineReport{Name: m.Name, SSH: m.SSH}
		if halted {
			mrep.Verdict = "not-evaluated"
			for _, st := range steps {
				if st.Scope == "machine" {
					o := rolloutBlocked("an earlier machine failed")
					o.Step = st.ID
					mrep.Steps = append(mrep.Steps, o)
				}
			}
			rep.Machines = append(rep.Machines, mrep)
			continue
		}
		var failed *stepOutcome
		for _, st := range steps {
			if st.Scope != "machine" {
				continue
			}
			if failed != nil {
				o := rolloutBlocked(failed.Step)
				o.Step = st.ID
				mrep.Steps = append(mrep.Steps, o)
				continue
			}
			o := r.execStep(ctx, st, mr)
			if o.Status == statFail && (st.ID == "converge" || st.ID == "smoke" || st.ID == "prove") {
				if hint := mr.restoreHint(); hint != "" {
					o.Fix = strings.TrimSpace(o.Fix + " | " + hint)
				}
			}
			mrep.Steps = append(mrep.Steps, o)
			if o.Status == statFail {
				f := o
				failed = &f
				stop(m.Name, o)
			}
		}
		mrep.Role = mr.Role
		mrep.Verdict = machineVerdict(mrep.Steps, mr.Pending)
		if failed != nil {
			r.recordFailure(ctx, mr, mrep.Verdict, *failed)
			if r.Apply {
				halted = true
			}
		} else if mrep.Verdict == "verified" && !r.Rollback {
			verified++
		}
		rep.Machines = append(rep.Machines, mrep)
	}

	for _, m := range rep.Machines {
		if m.Verdict == "not-evaluated" {
			rep.NotCovered = append(rep.NotCovered, m.Name)
		}
	}
	rep.Complete = !rep.failed() && len(rep.Machines) > 0 && verified == len(rep.Machines)
	rep.Summary = rolloutSummary(rep, r)
	return rep
}

// recordFailure writes a refused / failed / unreachable machine to the record
// (apply only), so the next run and 'brain host clients' show why it is behind.
func (r *rolloutRun) recordFailure(ctx context.Context, mr *machineRun, verdict string, o stepOutcome) {
	if !r.Apply || r.Records == nil || !r.recsRead {
		return
	}
	outcome := storeregistry.RolloutFailed
	switch verdict {
	case "refused":
		outcome = storeregistry.RolloutRefused
	case "unreachable":
		outcome = storeregistry.RolloutUnreachable
	}
	if mr.Role == "" {
		mr.Role = "unknown"
	}
	detail := o.Detail
	if mr.RestoreCmd != "" && (o.Step == "converge" || o.Step == "smoke" || o.Step == "prove") {
		// Put the restore command first: the record's detail is truncated from the end.
		detail = "RESTORE: " + mr.RestoreCmd + " | " + detail
	}
	_ = r.Records.Write(ctx, r.recordFor(mr, outcome, o.Step, detail))
}

func rolloutSummary(rep *rolloutReport, r *rolloutRun) string {
	var unreachable, refused, failed, pending, ok []string
	for _, m := range rep.Machines {
		switch m.Verdict {
		case "unreachable":
			unreachable = append(unreachable, m.Name)
		case "refused":
			refused = append(refused, m.Name)
		case "failed":
			failed = append(failed, m.Name)
		case "pending":
			pending = append(pending, m.Name)
		case "verified":
			ok = append(ok, m.Name)
		}
	}
	var parts []string
	add := func(label string, names []string) {
		if len(names) > 0 {
			parts = append(parts, label+" "+strings.Join(names, " "))
		}
	}
	add("verified", ok)
	add("changes needed", pending)
	add("REFUSED (pin)", refused)
	add("UNREACHABLE", unreachable)
	add("FAILED", failed)
	add("not evaluated", rep.NotCovered)
	body := strings.Join(parts, "; ")
	switch {
	case rep.Complete && r.Apply:
		return fmt.Sprintf("CONVERGED epoch %d: every one of %d machine(s) reported it itself. %s", rep.Epoch, len(rep.Machines), body)
	case rep.Complete:
		return fmt.Sprintf("PLAN: nothing to change; every one of %d machine(s) already proves epoch %d. %s", len(rep.Machines), rep.Epoch, body)
	case rep.Stopped != nil:
		return fmt.Sprintf("STOPPED at %s/%s: %s. %s", orDash(rep.Stopped.Machine), rep.Stopped.Step, rep.Stopped.Reason, body)
	case rep.failed():
		f := rep.Failures[0]
		return fmt.Sprintf("PLAN: --apply would stop at %s/%s. NOT converged. %s", orDash(f.Machine), f.Step, body)
	}
	return "PLAN: " + body + ". Nothing was changed; --apply would run the 'todo' steps."
}

// ---------------------------------------------------------------------------
// output
// ---------------------------------------------------------------------------

// rolloutCatalogue renders the step definitions: the text of --help and of the
// top of every plan.
func rolloutCatalogue(steps []rolloutStep) string {
	var b strings.Builder
	for _, scope := range []string{"fleet", "machine"} {
		if scope == "fleet" {
			b.WriteString("Once per run:\n")
		} else {
			b.WriteString("\nFor each machine, in roster order:\n")
		}
		for _, st := range steps {
			if st.Scope != scope {
				continue
			}
			fmt.Fprintf(&b, "  %-9s %s\n", st.ID, st.Title)
			fmt.Fprintf(&b, "      needs:  %s\n      does:   %s\n      proves: %s\n      undo:   %s\n", st.Pre, st.Do, st.Verify, st.Undo)
		}
	}
	return b.String()
}

func printRolloutReport(w io.Writer, rep *rolloutReport, r *rolloutRun, steps []rolloutStep) {
	switch rep.Mode {
	case "plan":
		fmt.Fprintln(w, "brain rollout: PLAN. Nothing is changed. Add --apply to run the 'todo' steps.")
	case "rollback-plan":
		fmt.Fprintln(w, "brain rollout rollback: PLAN. Nothing is changed. Add --apply to run the 'todo' steps.")
	case "apply":
		fmt.Fprintln(w, "brain rollout: APPLY. Stops at the first step that fails.")
	case "rollback":
		fmt.Fprintln(w, "brain rollout rollback: APPLY.")
	}
	if rep.Candidate != nil && !r.Rollback {
		fmt.Fprintf(w, "candidate  %s\n           '%s'  sha256 %s\n", rep.Candidate.Path, rep.Candidate.Version, shortHash(rep.Candidate.SHA256))
	}
	fmt.Fprintf(w, "\nSteps (the definitions the engine runs):\n%s\n", rolloutCatalogue(steps))
	fmt.Fprintln(w, "Result:")
	for _, o := range rep.Fleet {
		printOutcome(w, "fleet", o)
	}
	for _, m := range rep.Machines {
		role := ""
		if m.Role != "" {
			role = " (" + m.Role + ")"
		}
		fmt.Fprintf(w, "\n%s%s: %s\n", m.Name, role, strings.ToUpper(m.Verdict))
		for _, o := range m.Steps {
			printOutcome(w, "  ", o)
		}
	}
	fmt.Fprintf(w, "\n%s\n", rep.Summary)
	if len(rep.Failures) > 0 {
		fmt.Fprintln(w, "\nTo do first:")
		for _, f := range rep.Failures {
			where := f.Step
			if f.Machine != "" {
				where = f.Machine + "/" + f.Step
			}
			fmt.Fprintf(w, "  %s: %s\n", where, f.Reason)
			if f.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", f.Fix)
			}
		}
	}
}

func printOutcome(w io.Writer, indent string, o stepOutcome) {
	if indent == "fleet" {
		indent = ""
	}
	fmt.Fprintf(w, "%s%-9s %-8s %s\n", indent, o.Step, o.Status, o.Detail)
	if o.Fix != "" {
		fmt.Fprintf(w, "%s%-9s %-8s fix: %s\n", indent, "", "", o.Fix)
	}
}

// ---------------------------------------------------------------------------
// real wiring
// ---------------------------------------------------------------------------

// sqlRollouts keeps the rollout record in the shared database, reached the way
// the registry is: through this process's own connection.
type sqlRollouts struct{ db *sql.DB }

func (s sqlRollouts) Read(ctx context.Context) ([]storeregistry.Rollout, error) {
	return storeregistry.ReadRollouts(ctx, s.db)
}

func (s sqlRollouts) Write(ctx context.Context, rec storeregistry.Rollout) error {
	return storeregistry.ReportRollout(ctx, s.db, rec, time.Now().UTC())
}

// rolloutRecordsForCommand is nil on a client machine: it has no database
// connection of its own.
func rolloutRecordsForCommand() rolloutRecords {
	if _, client := clientSourceIfConfigured(); client {
		return nil
	}
	db, err := registryDBFunc()
	if err != nil || db == nil {
		return nil
	}
	return sqlRollouts{db: db}
}

// createStoreViaSelf runs this binary's own 'brain stores create NAME', so the
// store is made by the same code and the same database login as by hand.
func createStoreViaSelf(ctx context.Context, name string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, self, "stores", "create", name) //nolint:gosec // name validated by the step
	c.Env = append(os.Environ(), envRemoteOff+"=off")
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("'brain stores create %s': %w: %s", name, err, rolloutFirstLine(string(out)))
	}
	return nil
}

// rolloutCommandCanRunWithoutStore: on a client machine there is no local
// database to open (the registry is read through the relay), so the store must
// not be initialised; on a direct machine the connected store is the database.
func rolloutCommandCanRunWithoutStore(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c == brainRolloutCmd {
			_, client := clientSourceIfConfigured()
			return client
		}
	}
	return false
}

func rolloutActor() string {
	u := os.Getenv("USER")
	if u == "" {
		u = "unknown"
	}
	return "rollout:" + u
}

func buildRolloutRun(ctx context.Context, apply, rollback bool) (*rolloutRun, []fleetMember, error) {
	fleetRosterExplicit = rolloutRoster != ""
	roster, err := loadFleetRoster(rolloutRoster, rolloutMachines)
	if err != nil {
		return nil, nil, err
	}
	if !rollback && apply && rolloutBinary == "" {
		return nil, nil, errors.New("--apply needs --binary PATH: brain will not guess which file to put on the other machines")
	}
	src, err := registrySourceForCommand()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read the registry: %w", err)
	}
	self := machineName()
	r := &rolloutRun{
		Apply: apply, Rollback: rollback, Store: rolloutStore, Previous: rolloutPrevious,
		Exec: sshRolloutExec{self: self}, Registry: src, Records: rolloutRecordsForCommand(),
		CreateStore: createStoreViaSelf, Self: self, Actor: rolloutActor(),
	}
	if !rollback {
		path := rolloutBinary
		if path == "" {
			if path, err = os.Executable(); err != nil {
				return nil, nil, err
			}
		}
		if r.Cand, err = loadRolloutCandidate(ctx, path); err != nil {
			return nil, nil, fmt.Errorf("candidate binary: %w", err)
		}
	}
	return r, roster, nil
}

func runBrainRollout(cmd *cobra.Command, rollback bool) error {
	ctx := rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	r, roster, err := buildRolloutRun(ctx, rolloutApply, rollback)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	steps := installSteps()
	if rollback {
		steps = rollbackSteps()
	}
	rep := runRollout(ctx, r, roster, steps)
	if jsonOutput {
		_ = outputJSON(rep)
	} else {
		printRolloutReport(cmd.OutOrStdout(), rep, r, steps)
	}
	if rep.failed() {
		os.Exit(1)
	}
	return nil
}

var brainRolloutCmd = &cobra.Command{
	Use:   "rollout",
	Short: "Roll a binary and the store registry out to every machine, step by step, and prove it",
	Long: `The one procedure for: create a store on the brain host, put a brain binary that
knows about it on the other machines, and prove every machine has it. It
replaces a written runbook: it enforces the rules and records what it found.

  brain rollout                      PLAN: every step and check against every
                                     machine; changes nothing (the default)
  brain rollout --apply --binary P   run the steps; stops at the first failure
  brain rollout --store NAME ...     also create the store first (on the host)
  brain rollout rollback [--apply]   put the kept previous binary back
  brain rollout status               what the last runs recorded per machine

A plan evaluates every machine and says where --apply would stop. --apply stops
at the first precondition or verification that fails, says what is wrong and
what to do, and records the machine as refused, failed or unreachable.
A machine is only called converged when the machine ITSELF reports the target
registry epoch and the candidate build and its newest store reads by name; a
machine that cannot be reached is reported unreachable, never converged.

It never edits credentials, server users, grants or a wrapper pin. The pin
(~/.local/bin/.bd-pin.sh, BD_WANT) is owned by satellite-config; a binary the
pin would reject is refused with the exact instruction. A binary it replaces is
kept as bd.pre-<sha> beside it. Machines come from the fleet roster
(~/.config/brain/fleet.yaml, as for 'brain stores doctor --fleet'); a client
machine is checked through its own binary with BRAIN_REMOTE=off.

The steps:

` + rolloutCatalogue(installSteps()),
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         func(cmd *cobra.Command, _ []string) error { return runBrainRollout(cmd, false) },
}

var brainRolloutRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Put the previous binary back on the selected machines (plan unless --apply)",
	Long: `Restore the binary a rollout kept as bd.pre-<sha>, keeping the one it replaces as
bd.post-<sha>. The kept binary comes from the rollout record, or --previous PATH
(a file on the machine). Refuses when the machine's pin does not accept the kept
binary: restore the old pin and binary together with the converge backup's
restore.sh first. The store registry is additive and is left as it is.

The steps:

` + rolloutCatalogue(rollbackSteps()),
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         func(cmd *cobra.Command, _ []string) error { return runBrainRollout(cmd, true) },
}

var brainRolloutStatusCmd = &cobra.Command{
	Use:          "status",
	Short:        "Show the recorded binary, pin state and verification time of each machine",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		recs := rolloutRecordsForCommand()
		if recs == nil {
			return errors.New("this machine has no direct database access (client mode); run it on the brain host")
		}
		ctx := rootCtx
		if ctx == nil {
			ctx = context.Background()
		}
		rows, err := recs.Read(ctx)
		if err != nil {
			return err
		}
		if jsonOutput {
			return outputJSON(rows)
		}
		printRolloutRecords(cmd.OutOrStdout(), rows)
		return nil
	},
}

func printRolloutRecords(w io.Writer, rows []storeregistry.Rollout) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no rollout has been recorded yet")
		return
	}
	for _, x := range rows {
		verified := "never verified"
		if x.VerifiedAt != "" {
			verified = "verified " + x.VerifiedAt
		}
		fmt.Fprintf(w, "%-10s %-11s bd '%s'  pin %s  epoch %d  %s", x.Machine, x.Outcome, orDash(x.BdVersion), orDash(x.PinState), x.Epoch, verified)
		if x.PreviousBinary != "" {
			fmt.Fprintf(w, "  kept %s", x.PreviousBinary)
		}
		fmt.Fprintln(w)
		if x.Outcome != storeregistry.RolloutVerified && x.Detail != "" {
			fmt.Fprintf(w, "%-10s %s: %s\n", "", x.Step, x.Detail)
		}
	}
}

func init() {
	f := brainRolloutCmd.PersistentFlags()
	f.BoolVar(&rolloutApply, "apply", false, "Run the steps (default: plan only, nothing is changed)")
	f.StringVar(&rolloutRoster, "roster", "", "Roster file (default ~/.config/brain/fleet.yaml, else mini0, mini1, macbook)")
	f.StringSliceVar(&rolloutMachines, "machines", nil, "Only these roster machines (leaving one out is reported, never hidden)")
	brainRolloutCmd.Flags().StringVar(&rolloutBinary, "binary", "", "The bd binary to roll out (default for a plan: the running binary; required with --apply)")
	brainRolloutCmd.Flags().StringVar(&rolloutStore, "store", "", "Create this store first (run on the brain host) and prove it by name on every machine")
	brainRolloutRollbackCmd.Flags().StringVar(&rolloutPrevious, "previous", "", "A kept binary on the machine to restore (default: the one in the rollout record)")
	brainRolloutCmd.AddCommand(brainRolloutRollbackCmd, brainRolloutStatusCmd)
	brainCmd.AddCommand(brainRolloutCmd)
	rootCmd.AddCommand(brainRolloutCmd)
}

// hostClientsRollouts reads the rollout record for 'brain host clients list'.
// 'brain host' commands open no store, so when this process has no database
// connection the record is read by asking this same binary for 'rollout
// status', which connects the way every other command does. Best effort: no
// record means nothing is printed, never an error.
var hostClientsRollouts = readRolloutRecordsBestEffort

func readRolloutRecordsBestEffort(ctx context.Context) []storeregistry.Rollout {
	if testing.Testing() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if recs := rolloutRecordsForCommand(); recs != nil {
		if rows, err := recs.Read(ctx); err == nil {
			return rows
		}
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, self, "rollout", "status", "--json") //nolint:gosec // our own binary
	c.Env = append(os.Environ(), envRemoteOff+"=off")
	out, err := c.Output()
	if err != nil {
		return nil
	}
	var rows []storeregistry.Rollout
	if json.Unmarshal(out, &rows) != nil {
		return nil
	}
	return rows
}
