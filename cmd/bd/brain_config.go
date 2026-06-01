// Package main — brain_config.go
//
// Brain-specific runtime configuration for the bd binary. Keeps brain
// concerns separable from main.go so future brain features can grow
// here without touching the bd boot path.
package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/storage"
)

// brainSlugActor is the actor name recorded on issues.metadata slot
// writes that originate from the exfiltrator's slug persistence path.
// Kept distinct from "system" so the audit trail clearly attributes
// these writes to the brain exfiltration loop.
const brainSlugActor = "brain-exfiltrator"

// storeSlugPersister adapts a DoltStorage's SlotSet method to the
// exfiltrator.SlugPersister interface so freshly-derived slugs
// survive in issues.metadata.brain_slug across reads.
type storeSlugPersister struct {
	store storage.DoltStorage
}

// SetSlug writes slug into issues.metadata.brain_slug for issueID.
// The exfiltrator calls this after deriving a brand-new slug so
// subsequent reads of the issue see the canonical filename.
func (p storeSlugPersister) SetSlug(ctx context.Context, issueID, slug string) error {
	if p.store == nil {
		return nil
	}
	return p.store.SlotSet(ctx, issueID, "brain_slug", slug, brainSlugActor)
}

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
//
// The store argument is the inner DoltStorage the decorator will
// wrap; it's threaded into a storeSlugPersister so freshly-derived
// slugs are written back to issues.metadata.brain_slug.
func newBrainExfiltrator(store storage.DoltStorage) exfiltrator.Exfiltrator {
	root := brainKnowledgeRoot()
	if root == "" {
		return nil
	}
	return exfiltrator.NewMarkdownExfiltrator(root, storeSlugPersister{store: store})
}
