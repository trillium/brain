package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// isBrainMode reports whether the binary is running in brain persona.
// True when BD_NAME=brain or the binary basename is "brain".
func isBrainMode() bool {
	if os.Getenv("BD_NAME") == "brain" {
		return true
	}
	return filepath.Base(os.Args[0]) == "brain"
}

// autoFileBrainFeatureRequest is a cobra FlagErrorFunc that intercepts
// "unknown flag" errors on any brain-mode invocation. It prints a friendly
// message, execs itself to file a feature request in the active store, prints
// the created issue ID, then returns the original error so cobra still exits
// with a non-zero status.
//
// Opt-out: set BRAIN_NO_AUTO_FEATURE_REQUEST=1 (automatically set on the
// subprocess to prevent infinite recursion).
func autoFileBrainFeatureRequest(cmd *cobra.Command, err error) error {
	if os.Getenv("BRAIN_NO_AUTO_FEATURE_REQUEST") != "" {
		return err
	}
	if !isBrainMode() {
		return err
	}

	errStr := err.Error()
	if !strings.HasPrefix(errStr, "unknown flag: ") {
		return err
	}
	flagName := strings.TrimPrefix(errStr, "unknown flag: ")

	// Walk up to root to build the full command path (e.g. "brain create").
	var parts []string
	for c := cmd; c != nil; c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	cmdChain := strings.Join(parts, " ")

	title := fmt.Sprintf("Support %s flag on %s", flagName, cmdChain)
	description := fmt.Sprintf(
		"User ran `%s %s` — expected %s to be a recognized flag.\n\nWhat should %s do on %s?\n\n(Auto-filed by brain flag-error handler. Set BRAIN_NO_AUTO_FEATURE_REQUEST=1 to suppress.)",
		cmdChain, flagName, flagName, flagName, cmdChain,
	)

	fmt.Fprintf(os.Stderr, "Unrecognized flag %s. What did you expect to happen here? Filing a feature request...\n", flagName)

	sub := exec.Command(os.Args[0],
		"create",
		"--title", title,
		"--type", "feature",
		"--description", description,
		"--priority", "4",
		"--silent",
	)
	sub.Env = append(os.Environ(), "BRAIN_NO_AUTO_FEATURE_REQUEST=1")
	out, subErr := sub.Output()
	if subErr == nil {
		id := strings.TrimSpace(string(out))
		if id != "" {
			fmt.Fprintf(os.Stderr, "Filed: %s\n", id)
		}
	} else {
		fmt.Fprintf(os.Stderr, "Note: could not auto-file feature request: %v\n", subErr)
	}

	return err
}

func init() {
	rootCmd.SetFlagErrorFunc(autoFileBrainFeatureRequest)
}
