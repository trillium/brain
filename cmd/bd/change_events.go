// Package main — change_events.go
//
// Optional change-event emission. When change-events.enabled is set, every
// write command appends one JSON line to <beadsDir>/change-events.jsonl
// describing the mutation. This is a lightweight, append-only audit/eventing
// hook other tools can tail. It is disabled by default and best-effort: a
// failure to emit never fails the command.
//
// The file is a runtime artifact (like last-touched and export-state.json);
// callers who don't want it in git should .gitignore it themselves.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
)

// changeEventsFile is the append-only JSONL sink under the store's .beads dir.
const changeEventsFile = "change-events.jsonl"

// changeEvent is one line in change-events.jsonl.
type changeEvent struct {
	TS      string `json:"ts"`      // RFC3339 UTC timestamp of the write
	Store   string `json:"store"`   // logical store name (BD_NAME / store dir)
	Command string `json:"command"` // cobra command name that wrote (create, patch, ...)
	ID      string `json:"id"`      // last-touched issue id, when known
}

// maybeEmitChangeEvent appends a change-event line when change-events.enabled
// is true. Best-effort: any error is warned on stderr and swallowed so it
// cannot fail the command. Called from PersistentPostRunE after a write.
func maybeEmitChangeEvent(commandName string) {
	if !config.GetBool("change-events.enabled") {
		return
	}
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return
	}

	ev := changeEvent{
		TS:      time.Now().UTC().Format(time.RFC3339),
		Store:   changeEventStoreName(beadsDir),
		Command: commandName,
		ID:      GetLastTouchedID(),
	}
	line, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: change-events: marshal failed: %v\n", err)
		return
	}

	path := filepath.Join(beadsDir, changeEventsFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: change-events: open %s failed: %v\n", path, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: change-events: append failed: %v\n", err)
	}
}

// changeEventStoreName resolves a stable store label for the event. Prefers
// BD_NAME (set by store wrapper scripts), falling back to the store directory
// name (the parent of .beads), then to the brain default.
func changeEventStoreName(beadsDir string) string {
	if name := os.Getenv("BD_NAME"); name != "" {
		return name
	}
	if parent := filepath.Dir(beadsDir); parent != "" && parent != "." && parent != string(filepath.Separator) {
		return filepath.Base(parent)
	}
	return primaryStoreName
}
