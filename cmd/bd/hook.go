package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/hooksdef"
)

// hookCmd is the parent of the declared-hook verbs. It is "hook" (singular)
// because `bd hooks` already names the git-hook installer.
var hookCmd = &cobra.Command{
	Use:     "hook",
	GroupID: "setup",
	Short:   "Declared guard/observer hooks (hooks.d)",
	Long: `Hooks declared in <beadsDir>/hooks.d/<name>.toml observe mutations and may
refuse them. A guard hook runs before a write and can refuse it (fail-closed);
an observer runs after (fail-open); a hook that declares no policy warns.

Every failure of an observer or unset-policy hook becomes a durable warning
record in the store. Hooks never write: the store is the only writer.

See docs/brain/HOOKS.md.`,
}

// hookAncestry is set when an ancestor of this process is a bd process with a
// hook running against this store (see hooksdef.AncestorRunningHook).
var hookAncestry bool

// insideHook reports whether this process is the work of a hook: either the
// hook marker is in its environment or a hook-running bd is its ancestor.
// Such a process may read the store and may never write it.
func insideHook() bool { return hooksdef.InsideHook() || hookAncestry }

// isHookCommand reports whether cmd is one of the hook verbs, which must keep
// working when hooks.d is broken (that is when they are needed).
func isHookCommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c == hookCmd {
			return true
		}
	}
	return false
}

// loadHookDefs loads hooks.d for a command about to run. Unusable definitions
// refuse every command that could write; read-only commands and the hook verbs
// themselves still run so the problem can be diagnosed.
func loadHookDefs(cmd *cobra.Command, beadsDir string, readOnly bool) ([]hooksdef.Definition, error) {
	if beadsDir == "" {
		return nil, nil
	}
	dir := hooksdef.HooksDir(beadsDir)
	defs, err := hooksdef.LoadAll(dir)
	if err == nil {
		return defs, nil
	}
	if readOnly || isHookCommand(cmd) {
		return nil, nil
	}
	return nil, fmt.Errorf("refusing to run %q: hook definitions in %s are unusable, so writes cannot be guarded: %v\n  fix or remove the named file (bd hook list shows every problem)", cmd.Name(), dir, err)
}

func hooksDirOrErr() (string, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return "", fmt.Errorf("no .beads directory found; hooks are declared per store in <beadsDir>/hooks.d")
	}
	return hooksdef.HooksDir(beadsDir), nil
}

var hookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List declared hooks and validate hooks.d",
	Long: `List every hook declared in hooks.d and report every definition that cannot be
loaded. Exits non-zero when any definition is unusable. Writes nothing.

Examples:
  bd hook list
  bd hook list --json`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := hooksDirOrErr()
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		defs, loadErr := hooksdef.LoadAll(dir)
		if jsonOutput {
			type row struct {
				Name    string `json:"name"`
				Policy  string `json:"policy"`
				When    string `json:"when"`
				Timeout string `json:"timeout"`
				Path    string `json:"path"`
				Warns   bool   `json:"warns_when_unset"`
			}
			rows := make([]row, 0, len(defs))
			for _, d := range defs {
				rows = append(rows, row{d.Name, d.Policy, d.Event(), d.Timeout.String(), d.Path, !d.Declared()})
			}
			out := map[string]interface{}{"dir": dir, "hooks": rows}
			if loadErr != nil {
				out["error"] = loadErr.Error()
			}
			if err := outputJSON(out); err != nil {
				return err
			}
			if loadErr != nil {
				return SilentExit()
			}
			return nil
		}
		if len(defs) == 0 && loadErr == nil {
			fmt.Printf("No hooks declared (%s)\n", dir)
			return nil
		}
		for _, d := range defs {
			fmt.Printf("  %s\n", d.Describe())
		}
		if loadErr != nil {
			return HandleError("unusable hook definition(s): %v", loadErr)
		}
		return nil
	},
}

var (
	hookWarnStatus string
	hookWarnAck    []string
	hookWarnAckAll bool
)

var hookWarningsCmd = &cobra.Command{
	Use:   "warnings",
	Short: "List and acknowledge hook warnings",
	Long: `List the durable warning records hooks have produced. A warning says which hook
degraded and what happened; it stays "open" until acknowledged, so it can be
routed to a person or a repair agent instead of being read and stepped over.

Records live in the store's config table under the "hookwarning." prefix, so
anything that can read the store can read them (bd hook warnings --json).

Examples:
  bd hook warnings                      # open warnings
  bd hook warnings --status all --json
  bd hook warnings --ack hw-20261008T101500Z-1a2b3c4d
  bd hook warnings --ack-all`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := ensureDirectMode("hook warnings requires direct database access"); err != nil {
			return HandleError("%v", err)
		}
		switch hookWarnStatus {
		case hooksdef.StatusOpen, hooksdef.StatusAcknowledged, "all":
		default:
			return HandleErrorRespectJSON("unknown --status %q; use open, acknowledged or all", hookWarnStatus)
		}
		ctx := rootCtx
		cfg, err := store.GetAllConfig(ctx)
		if err != nil {
			return HandleErrorRespectJSON("reading warnings: %v", err)
		}
		all, badRecords := hooksdef.WarningsFromConfig(cfg)

		if len(hookWarnAck) > 0 || hookWarnAckAll {
			CheckReadonly("hook warnings --ack")
			return acknowledgeHookWarnings(ctx, all, badRecords)
		}

		var shown []hooksdef.Warning
		for _, w := range all {
			if hookWarnStatus == "all" || w.Status == hookWarnStatus {
				shown = append(shown, w)
			}
		}
		if jsonOutput {
			out := map[string]interface{}{"warnings": shown}
			if len(badRecords) > 0 {
				msgs := make([]string, len(badRecords))
				for i, e := range badRecords {
					msgs[i] = e.Error()
				}
				out["unreadable"] = msgs
			}
			if err := outputJSON(out); err != nil {
				return err
			}
		} else {
			if len(shown) == 0 {
				fmt.Printf("No %s hook warnings\n", hookWarnStatus)
			}
			for _, w := range shown {
				subject := ""
				if w.Subject != "" {
					subject = " on " + w.Subject
				}
				fmt.Printf("%s  %s  [%s] hook %q (%s)%s\n    %s\n", w.ID, w.Status, w.Reason, w.Hook, w.Event, subject, w.Detail)
			}
		}
		// A record that cannot be read is itself a degradation: say so and fail.
		if len(badRecords) > 0 {
			for _, e := range badRecords {
				fmt.Fprintf(os.Stderr, "Error: %v\n", e)
			}
			return SilentExit()
		}
		return nil
	},
}

func acknowledgeHookWarnings(ctx context.Context, all []hooksdef.Warning, bad []error) error {
	byID := map[string]hooksdef.Warning{}
	for _, w := range all {
		byID[w.ID] = w
	}
	var targets []hooksdef.Warning
	if hookWarnAckAll {
		for _, w := range all {
			if w.Status == hooksdef.StatusOpen {
				targets = append(targets, w)
			}
		}
	}
	for _, id := range hookWarnAck {
		w, ok := byID[id]
		if !ok {
			return HandleErrorRespectJSON("no hook warning %q (bd hook warnings --status all lists them)", id)
		}
		targets = append(targets, w)
	}
	now := time.Now().UTC()
	actor := getActorWithGit()
	var acked []string
	for _, w := range targets {
		if w.Status == hooksdef.StatusAcknowledged {
			continue
		}
		w.Status, w.AckBy, w.AckAt = hooksdef.StatusAcknowledged, actor, &now
		enc, err := w.Encode()
		if err == nil {
			err = store.SetConfig(ctx, hooksdef.WarningKey(w.ID), enc)
		}
		if err != nil {
			return HandleErrorRespectJSON("acknowledging %s: %v", w.ID, err)
		}
		acked = append(acked, w.ID)
	}
	if jsonOutput {
		return outputJSON(map[string]interface{}{"acknowledged": acked, "by": actor})
	}
	fmt.Printf("Acknowledged %d hook warning(s)\n", len(acked))
	_ = bad
	return nil
}

var (
	hookTestEvent       string
	hookTestPayloadFile string
)

var hookTestCmd = &cobra.Command{
	Use:   "test <name>",
	Short: "Run one hook against a sample payload and show its decision",
	Long: `Run a declared hook exactly as the write path would — same shell, same
sanitized environment, same timeout — against a sample or supplied payload, and
print its exit status, output and what its declared policy would do with it.
Writes nothing.

Examples:
  bd hook test no-secrets --event create
  bd hook test no-secrets --payload-file payload.json`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := hooksDirOrErr()
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		def, err := hooksdef.LoadFile(dir + "/" + args[0] + ".toml")
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		payload := hooksdef.Payload{Event: hookTestEvent, Command: "hook-test",
			Issue: &hooksdef.PayloadIssue{Title: "sample bead", IssueType: "task", Priority: 2}}
		if hookTestPayloadFile != "" {
			raw, err := os.ReadFile(hookTestPayloadFile) //nolint:gosec // operator-supplied path
			if err != nil {
				return HandleErrorRespectJSON("reading payload file: %v", err)
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				return HandleErrorRespectJSON("payload file is not a hook payload: %v", err)
			}
		}
		if !def.MatchesEvent(payload.Event) {
			return HandleErrorRespectJSON("hook %q does not fire for event %q (when=%s)", def.Name, payload.Event, def.Event())
		}
		res := hooksdef.Run(rootCtx, def, payload)
		d := hooksdef.Decide(def, payload.Event, res)
		verdict := "pass"
		switch {
		case d.Refusal != nil:
			verdict = "REFUSE: " + d.Refusal.Error()
		case d.Warning != nil:
			verdict = "WARN [" + d.Warning.Reason + "]: " + d.Warning.Detail
		}
		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"hook": def.Name, "exit": res.ExitCode, "timed_out": res.TimedOut,
				"stdout": res.Stdout, "stderr": res.Stderr, "verdict": verdict,
			})
		}
		fmt.Printf("hook:    %s\nexit:    %d\nstdout:  %s\nstderr:  %s\nverdict: %s\n",
			def.Describe(), res.ExitCode, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr), verdict)
		return nil
	},
}

func init() {
	hookWarningsCmd.Flags().StringVar(&hookWarnStatus, "status", hooksdef.StatusOpen, "Show warnings with this status: open, acknowledged or all")
	hookWarningsCmd.Flags().StringArrayVar(&hookWarnAck, "ack", nil, "Acknowledge the warning with this id (repeatable)")
	hookWarningsCmd.Flags().BoolVar(&hookWarnAckAll, "ack-all", false, "Acknowledge every open warning")
	hookTestCmd.Flags().StringVar(&hookTestEvent, "event", hooksdef.EventCreate, "Event to run the hook for: create, update, close or delete")
	hookTestCmd.Flags().StringVar(&hookTestPayloadFile, "payload-file", "", "JSON file holding the payload to pipe to the hook")

	hookCmd.AddCommand(hookListCmd, hookWarningsCmd, hookTestCmd)
	rootCmd.AddCommand(hookCmd)
}
