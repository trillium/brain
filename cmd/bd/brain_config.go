// Package main — brain_config.go
//
// Brain-specific runtime configuration for the bd binary. Keeps brain
// concerns separable from main.go so future brain features can grow
// here without touching the bd boot path.
package main

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
)

// brainKnowledgeRoot returns the filesystem root where brain renders
// markdown files. Resolution order:
//
//  1. BRAIN_KNOWLEDGE_ROOT environment variable (absolute path).
//  2. ~/data/knowledge (default per WHAT_IS_BRAIN.md § 1).
//
// Returns empty string only if the home directory cannot be resolved
// and no override is set — caller should treat empty as "skip
// exfiltration entirely" (defensive; the decorator passes through
// when constructed with a nil exfiltrator).
func brainKnowledgeRoot() string {
	if env := os.Getenv("BRAIN_KNOWLEDGE_ROOT"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "data", "knowledge")
}

// newBrainExfiltrator constructs the default markdown exfiltrator for
// the bd binary, or nil if the knowledge root cannot be resolved.
// A nil exfiltrator is a passthrough — BrainExfiltrationDecorator
// handles that case gracefully.
func newBrainExfiltrator() exfiltrator.Exfiltrator {
	root := brainKnowledgeRoot()
	if root == "" {
		return nil
	}
	return exfiltrator.NewMarkdownExfiltrator(root)
}
