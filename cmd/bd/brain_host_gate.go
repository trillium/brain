// Package main — brain_host_gate.go
//
// The host gate: what a machine does with brain commands once it is no longer
// (or is about to stop being) the one place the brain lives.
//
//	frozen  a promotion paused writes here: read-only commands run, everything
//	        else is refused before it touches the database.
//	moved   the brain lives elsewhere: commands forwarded from a client are
//	        answered with where it went (the client follows); anything else is
//	        refused rather than quietly reading or writing the old copy.
//
// The gate is deliberately a file check at process start, not a database lock:
// it costs nothing on a healthy host, works with the database server down, and
// covers every entry point that goes through a brain command. Anything that
// writes to the database by some other route is outside it, which is why a
// promotion also proves, by hash, that nothing changed behind its back.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	envHostGate      = "BRAIN_HOST_GATE"        // "off" bypasses the gate (recovery, audits of the old copy)
	envRemoteForward = "BRAIN_REMOTE_FORWARDED" // set by the forwarding script of a client call
)

func hostGateOff() bool { return strings.EqualFold(os.Getenv(envHostGate), "off") }

// hostGateEarly runs before client-mode forwarding. A call that a client
// forwarded to a machine that holds a forwarding record (it gave the host role
// away) gets the moved notice and exit 75, before any command or database
// machinery starts. The client verifies the notice and follows it.
func hostGateEarly(args []string) (handled bool, exitCode int) {
	if hostGateOff() || os.Getenv(envRemoteForward) != "1" {
		return false, 0
	}
	if len(args) > 0 && (args[0] == "host" || args[0] == "remote") {
		return false, 0
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, 0
	}
	rec := activeForward(home)
	if rec == nil {
		return false, 0
	}
	fmt.Fprintln(os.Stderr, noticeFromRecord(rec).line())
	return true, hostMovedExit
}

func shortDate(stamp string) string {
	if t, err := time.Parse(hostTimeFormat, stamp); err == nil {
		return t.Format("2006-01-02")
	}
	return stamp
}

// hostGateExempt are commands that never reach the brain database in a way the
// gate needs to stop: management of the host itself and of client mode, help,
// version, completion, and read-only queries.
var hostGateExempt = map[string]bool{
	"host": true, "remote": true, "rollout": true, "help": true, "version": true, "completion": true,
	"__complete": true, "__completeNoDesc": true, "where": true, "info": true,
}

// hostGateAllows reports whether cmd may run on a machine in the given role.
// A frozen host still answers read-only commands; a moved one answers none of
// them, because a read of the old copy would quietly show stale data.
func hostGateAllows(cmd *cobra.Command, role hostRole) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if hostGateExempt[c.Name()] || (role == roleFrozen && isReadOnlyCommand(c.Name())) {
			return true
		}
	}
	if v, _ := cmd.Flags().GetBool("version"); v {
		return true
	}
	return false
}

// hostRoleGate is called first in every command's pre-run. It returns an error
// when this machine's host role says the command must not run here.
func hostRoleGate(cmd *cobra.Command) error {
	if hostGateOff() {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	cfg, err := loadHostConfig(home)
	if err != nil {
		return nil // a damaged host.yaml must not take every command down; 'brain host status' reports it
	}
	if cfg == nil || cfg.Role == roleHost {
		return nil
	}
	if hostGateAllows(cmd, cfg.Role) {
		return nil
	}
	switch cfg.Role {
	case roleFrozen:
		return fmt.Errorf("the brain is frozen on this machine while it is promoted to another host (promotion %s, since %s): writes are paused, reads still work.\n"+
			"  It resumes by itself when the promotion finishes. To stop it now: brain host rollback.  To bypass once: %s=off", cfg.PromoteID, cfg.FrozenSince, envHostGate)
	case roleMoved:
		return errors.New("this machine is no longer the brain host: the brain moved to " + cfg.MovedTo + " on " + shortDate(cfg.MovedAt) + ".\n" +
			"  Commands run there in client mode (brain remote status). The old copy here is kept untouched, not served.\n" +
			"  To look at it anyway: " + envHostGate + "=off <command>.  To undo the move: brain host rollback")
	}
	return nil
}
