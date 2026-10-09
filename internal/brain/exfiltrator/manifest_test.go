package exfiltrator_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

// fileAt resolves a file path under a temporary exfil root the way the
// renderer lays them out.
func fileAt(t *testing.T, root string, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{root, "entries"}, parts...)...)
}

// TestRender_RecordsManifestEntry asserts a successful render leaves a
// manifest entry behind — the memory the edit-back run reads so "this
// bead's render was deleted" is distinguishable from "never rendered".
func TestRender_RecordsManifestEntry(t *testing.T) {
	root := t.TempDir()
	m := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "brain-k00001", "Manifest subject", types.TypeKnowledge)
	if err := m.Render(t.Context(), issue); err != nil {
		t.Fatalf("render: %v", err)
	}

	mf, path, err := exfiltrator.ReadManifest(root)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if path == "" {
		t.Fatal("empty manifest path")
	}
	entry, ok := mf.Files["brain-k00001"]
	if !ok {
		t.Fatalf("manifest has no entry for brain-k00001: %+v", mf.Files)
	}
	if entry.Kind != string(types.TypeKnowledge) {
		t.Errorf("entry.Kind = %q, want knowledge", entry.Kind)
	}
	if entry.Slug != "manifest-subject" {
		t.Errorf("entry.Slug = %q, want manifest-subject", entry.Slug)
	}
	if !strings.HasSuffix(entry.Path, "entries/knowledge/manifest-subject.md") {
		t.Errorf("entry.Path = %q, want entries/knowledge/manifest-subject.md", entry.Path)
	}
	if entry.Time.IsZero() {
		t.Error("entry.Rendered is zero; the manifest should timestamp its writes")
	}
}

// TestRender_ManifestRefreshesOnRerender asserts re-rendering the same bead
// keeps exactly one entry.
func TestRender_ManifestRefreshesOnRerender(t *testing.T) {
	root := t.TempDir()
	m := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "brain-k00002", "Refresh subject", types.TypeKnowledge)
	for range 2 {
		if err := m.Render(t.Context(), issue); err != nil {
			t.Fatalf("render: %v", err)
		}
	}
	mf, _, err := exfiltrator.ReadManifest(root)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(mf.Files) != 1 {
		t.Fatalf("manifest has %d entries, want 1", len(mf.Files))
	}
}

// TestRemove_DropsManifestEntry asserts the store's own removal (kind
// transitions) cleans the manifest, so the edit-back run can never read a
// deliberate removal as a later deletion.
func TestRemove_DropsManifestEntry(t *testing.T) {
	root := t.TempDir()
	m := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "brain-k00003", "Drop subject", types.TypeKnowledge)
	if err := m.Render(t.Context(), issue); err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := m.Remove(t.Context(), issue.ID, types.TypeKnowledge, "drop-subject"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	mf, _, err := exfiltrator.ReadManifest(root)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if _, ok := mf.Files["brain-k00003"]; ok {
		t.Fatal("manifest still records a file the store itself removed")
	}
}

// TestRender_MarkedBeadSkips asserts the load-bearing rule: a bead carrying
// the deletion mark is never re-rendered. An unmarked render must not
// resurrect a bead that was marked, and the skip is a typed outcome the
// render verbs report, not a failure.
func TestRender_MarkedBeadSkips(t *testing.T) {
	root := t.TempDir()
	m := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "brain-k00004", "Marked subject", types.TypeKnowledge)
	issue.Labels = []string{"alpha", exfiltrator.DeletionMarkLabel, "omega"}

	err := m.Render(t.Context(), issue)
	if !exfiltrator.IsSkipMarked(err) {
		t.Fatalf("render of a marked bead: err = %v, want a deletion-mark skip", err)
	}
	if !strings.Contains(err.Error(), "render-marks clear") {
		t.Errorf("skip error should name the way out: %v", err)
	}
	if _, err := os.Stat(fileAt(t, root, "knowledge", "marked-subject.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marked bead's render must not be resurrected: err = %v", err)
	}

	// The manifest stays absent for this root: the bead has no render.
	if _, _, err := exfiltrator.ReadManifest(root); !errors.Is(err, exfiltrator.ErrNoManifest) {
		t.Errorf("read manifest: err = %v, want ErrNoManifest", err)
	}
}

// TestRender_MarkedBead_ClearedMarkRendersAgain asserts clearing the mark
// (removing the label) lets the render through — the exact sequence
// 'bd render-marks clear' sets up through the store's post-write render.
func TestRender_MarkedBead_ClearedMarkRendersAgain(t *testing.T) {
	root := t.TempDir()
	m := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "brain-k00005", "Unmarked subject", types.TypeKnowledge)
	issue.Labels = []string{exfiltrator.DeletionMarkLabel}
	if err := m.Render(t.Context(), issue); !exfiltrator.IsSkipMarked(err) {
		t.Fatalf("expected skip: %v", err)
	}
	issue.Labels = nil
	if err := m.Render(t.Context(), issue); err != nil {
		t.Fatalf("render after clear: %v", err)
	}
	if _, err := os.Stat(fileAt(t, root, "knowledge", "unmarked-subject.md")); err != nil {
		t.Fatalf("file should exist after the clear: %v", err)
	}
}

// TestReadManifest_Missing asserts the sentinel the edit-back run reports
// as a named refusal.
func TestReadManifest_Missing(t *testing.T) {
	_, _, err := exfiltrator.ReadManifest(t.TempDir())
	if !errors.Is(err, exfiltrator.ErrNoManifest) {
		t.Fatalf("err = %v, want ErrNoManifest", err)
	}
}
