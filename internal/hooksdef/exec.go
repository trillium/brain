package hooksdef

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// EnvInsideHook is set in every hook process's environment. A bd process that
// finds it refuses to open the store for writing: a hook may observe and may
// refuse, but a write is never the work of a hook — the store stays the only
// writer. The mechanism strips every variable that points at a store, runs the
// hook in an empty directory, and then marks the process, so a hook that
// shells out to `bd create` is refused by name rather than by accident.
const EnvInsideHook = "BD_INSIDE_HOOK"

// InsideHook reports whether this process was started by a hook.
func InsideHook() bool { return os.Getenv(EnvInsideHook) != "" }

// strippedEnvPrefixes are the environment families a hook process never
// inherits: they carry database credentials, server ports or store paths.
var strippedEnvPrefixes = []string{"BEADS_", "BRAIN_", "BD_", "DOLT_", "MYSQL_"}

// HookEnv builds the environment a hook process runs in: the caller's
// environment minus every store-facing family, plus the EnvInsideHook marker.
func HookEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		stripped := false
		for _, p := range strippedEnvPrefixes {
			if strings.HasPrefix(key, p) {
				stripped = true
				break
			}
		}
		if !stripped {
			out = append(out, kv)
		}
	}
	return append(out, EnvInsideHook+"=1")
}

// RunResult is what one hook execution produced.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	// TimedOut is set when the run was killed by its own deadline.
	TimedOut bool
	// StartErr carries the error that prevented running at all (the shell
	// could not be started, the payload could not be encoded). ExitCode is
	// meaningless when it is set.
	StartErr error
}

// Run executes one hook against one payload, with the definition's timeout, in
// a store-blind environment and an empty working directory. It reports every
// outcome as a RunResult; deciding what it means is Decide's job.
func Run(ctx context.Context, def Definition, payload Payload) RunResult {
	raw, err := json.Marshal(payload)
	if err != nil {
		return RunResult{StartErr: fmt.Errorf("encoding hook payload: %w", err)}
	}
	dir, err := os.MkdirTemp("", "bd-hook-*")
	if err != nil {
		return RunResult{StartErr: fmt.Errorf("creating hook working directory: %w", err)}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	release, err := markRunning(def)
	if err != nil {
		return RunResult{StartErr: err}
	}
	defer release()

	cctx, cancel := context.WithTimeout(ctx, def.Timeout)
	defer cancel()

	//nolint:gosec // the command is operator-authored in hooks.d; running it is the declared contract.
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", def.Run)
	cmd.Dir = dir
	cmd.Env = HookEnv(os.Environ())
	cmd.Stdin = bytes.NewReader(raw)
	cmd.WaitDelay = 2 * time.Second // a killed shell must not hang on a grandchild's pipe
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	res := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr == nil {
		return res
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		return res
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() > 0 {
		res.ExitCode = exitErr.ExitCode()
		return res
	}
	// Killed by a signal, or never started: the hook did not finish deciding.
	res.StartErr = fmt.Errorf("hook did not run to an exit status: %v", runErr)
	return res
}

// Decision is what one finished hook means for the mutation it was run
// against. At most one of Refusal and Warning is set; both nil is a pass.
type Decision struct {
	Refusal *Refusal
	Warning *Warning
}

// Decide is the single place the three failure policies are implemented.
//
//   - guard: exit 0 passes; exit RefusalExit refuses (CauseRefused); every
//     other outcome — other exit, timeout, could not start — refuses too,
//     because a guard that could not finish deciding fails closed.
//   - observer: exit 0 passes; every other outcome is a warning and the write
//     proceeds (fails open).
//   - unset: never blocks, never passes silently — a warning is produced on
//     every run, success or failure, saying the hook declared no policy.
func Decide(def Definition, event string, res RunResult) Decision {
	failure, failed := describeFailure(def, res)

	if def.IsGuard() {
		switch {
		case !failed:
			return Decision{}
		case !res.TimedOut && res.StartErr == nil && res.ExitCode == def.RefusalExit:
			return Decision{Refusal: &Refusal{Hook: def.Name, Path: def.Path, Event: event,
				Detail: condense(firstNonEmpty(res.Stderr, res.Stdout)), Cause: CauseRefused}}
		default:
			cause := CauseFailed
			if res.TimedOut {
				cause = CauseTimeout
			} else if res.StartErr != nil {
				cause = CauseLoad
			}
			return Decision{Refusal: &Refusal{Hook: def.Name, Path: def.Path, Event: event,
				Detail: failure + "; a guard that cannot finish deciding refuses", Cause: cause}}
		}
	}

	warn := func(reason, detail string) Decision {
		return Decision{Warning: &Warning{Hook: def.Name, Path: def.Path, Event: event,
			Reason: reason, Detail: detail, Status: StatusOpen}}
	}
	if !def.Declared() {
		if failed {
			return warn(ReasonFailure, fmt.Sprintf("%s, and the hook declares no failure policy; the write proceeded — declare policy = \"guard\" or \"observer\"", failure))
		}
		return warn(ReasonUnsetPolicy, "the hook ran and passed, but declares no failure policy, so a failure of it would neither block nor be silent; declare policy = \"guard\" or \"observer\" to settle that and stop this warning")
	}
	if failed {
		return warn(ReasonFailure, failure+"; the write proceeded (observer fails open)")
	}
	return Decision{}
}

// describeFailure says in one phrase how a run went wrong, or reports that it
// did not.
func describeFailure(def Definition, res RunResult) (string, bool) {
	switch {
	case res.StartErr != nil:
		return "hook could not be run: " + res.StartErr.Error(), true
	case res.TimedOut:
		return fmt.Sprintf("hook ran past its %s timeout and was killed", def.Timeout), true
	case res.ExitCode == 0:
		return "", false
	}
	msg := fmt.Sprintf("hook exited %d", res.ExitCode)
	if d := condense(firstNonEmpty(res.Stderr, res.Stdout)); d != "" {
		msg += ": " + d
	}
	if def.IsGuard() {
		msg += fmt.Sprintf(" (its refusal exit is %d)", def.RefusalExit)
	}
	return msg, true
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// maxDetail bounds how much of a hook's output is carried into a refusal or a
// stored warning.
const maxDetail = 600

// condense folds a hook's output to one bounded line: non-empty lines joined
// with " / ". A hook that explains itself over several lines is quoted whole
// rather than cut to its first line, which is often the least informative.
func condense(s string) string {
	var parts []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	out := strings.Join(parts, " / ")
	if len(out) > maxDetail {
		out = out[:maxDetail] + "…"
	}
	return out
}
