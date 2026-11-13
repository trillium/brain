package main

import (
	"fmt"
	"os"

	"github.com/steveyegge/beads/internal/metrics"
)

// The FatalError family, kept for brain's own commands.
//
// Upstream beads 1.3 removed these os.Exit helpers and converted its commands
// to `return HandleError(...)` from RunE, so each command's deferred metrics
// close runs. Brain's verbs (brain, stores, store-prefix, isa-*, render*,
// patch, transfer, remote, registry) still call them from Run bodies and from
// helpers that return no error, so the helpers live on here, for those callers
// only. They flush queued metrics before exiting, the way CheckReadonly does,
// so an exit here strands nothing already recorded. Converting the callers to
// HandleError is tracked as its own follow-up; new code should return
// HandleError instead.

// FatalError writes an error message and exits with code 1. With --json the
// message goes to stderr as structured JSON.
func FatalError(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if jsonOutput {
		jsonStderrError(msg, "")
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	}
	fatalExit()
}

// FatalErrorRespectJSON writes an error message and exits with code 1. With
// --json it outputs structured JSON to stdout; otherwise plain text to stderr.
func FatalErrorRespectJSON(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if jsonOutput {
		jsonStdoutError(msg, "")
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	}
	fatalExit()
}

// FatalErrorWithHintRespectJSON writes an error message with a hint and exits.
// With --json it emits structured JSON to stdout so callers can parse it.
func FatalErrorWithHintRespectJSON(message, hint string) {
	if jsonOutput {
		jsonStdoutError(message, hint)
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", message)
		fmt.Fprintf(os.Stderr, "Hint: %s\n", hint)
	}
	fatalExit()
}

// FatalErrorWithHint writes an error message with a hint to stderr and exits.
func FatalErrorWithHint(message, hint string) {
	if jsonOutput {
		jsonStderrError(message, hint)
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", message)
		fmt.Fprintf(os.Stderr, "Hint: %s\n", hint)
	}
	fatalExit()
}

func fatalExit() {
	metrics.CloseAndFlush()
	os.Exit(1)
}
