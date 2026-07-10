package exfiltrator_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

// fakePersister records SetSlug calls so tests can verify that fresh
// slugs flow back to the storage layer.
type fakePersister struct {
	mu    sync.Mutex
	calls []slugCall
	err   error
}

type slugCall struct {
	ID   string
	Slug string
}

func (f *fakePersister) SetSlug(_ context.Context, id, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, slugCall{ID: id, Slug: slug})
	return nil
}

func (f *fakePersister) snapshot() []slugCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]slugCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func mustBrainIssue(t *testing.T, id, title string, kind types.IssueType) *types.Issue {
	t.Helper()
	return &types.Issue{
		ID:          id,
		Title:       title,
		Description: "body for " + id,
		Status:      types.StatusOpen,
		Priority:    2,
		IssueType:   kind,
		CreatedAt:   time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC),
	}
}

// ── Slug derivation ────────────────────────────────────────────────

func TestSlugFor_KebabFromTitle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-abc123", "Hello, World! Tricky Case.", types.TypeTask)
	slug, derived, err := exf.SlugFor(issue)
	if err != nil {
		t.Fatalf("SlugFor: %v", err)
	}
	if !derived {
		t.Fatalf("derived = false, want true (no metadata)")
	}
	if slug != "hello-world-tricky-case" {
		t.Fatalf("slug = %q, want %q", slug, "hello-world-tricky-case")
	}
}

func TestSlugFor_PrefersMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-abc123", "Renamed Later", types.TypeKnowledge)
	issue.Metadata = json.RawMessage(`{"brain_slug":"original-slug","other":"data"}`)

	slug, derived, err := exf.SlugFor(issue)
	if err != nil {
		t.Fatalf("SlugFor: %v", err)
	}
	if derived {
		t.Fatalf("derived = true, want false (metadata present)")
	}
	if slug != "original-slug" {
		t.Fatalf("slug = %q, want %q", slug, "original-slug")
	}
}

func TestSlugFor_FallbackToID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-fallback1", "", types.TypeBoth)
	slug, derived, err := exf.SlugFor(issue)
	if err != nil {
		t.Fatalf("SlugFor: %v", err)
	}
	if !derived {
		t.Fatalf("derived = false, want true")
	}
	if slug != "b-fallback1" {
		t.Fatalf("slug = %q, want %q", slug, "b-fallback1")
	}
}

func TestSlugFor_NoTitleNoID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := &types.Issue{IssueType: types.TypeTask}
	if _, _, err := exf.SlugFor(issue); err == nil {
		t.Fatalf("SlugFor with empty title+id should error")
	}
}

func TestSlugFor_CollisionAppendsShortID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	// Two issues with the same title at the same kind — second one
	// gets a short-id suffix.
	a := mustBrainIssue(t, "B-aaa111", "Shared Title", types.TypeTask)
	b := mustBrainIssue(t, "B-bbb222zzzzzz", "Shared Title", types.TypeTask)

	slugA, _, err := exf.SlugFor(a)
	if err != nil {
		t.Fatalf("SlugFor a: %v", err)
	}
	slugB, _, err := exf.SlugFor(b)
	if err != nil {
		t.Fatalf("SlugFor b: %v", err)
	}
	if slugA != "shared-title" {
		t.Fatalf("slugA = %q, want %q", slugA, "shared-title")
	}
	if !strings.HasPrefix(slugB, "shared-title-") {
		t.Fatalf("slugB = %q, want prefix %q", slugB, "shared-title-")
	}
	if slugA == slugB {
		t.Fatalf("collision not resolved: %q == %q", slugA, slugB)
	}
}

func TestSlugFor_SameIssueIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-ccc333", "Idempotent Title", types.TypeKnowledge)
	first, _, err := exf.SlugFor(issue)
	if err != nil {
		t.Fatalf("first SlugFor: %v", err)
	}
	second, _, err := exf.SlugFor(issue)
	if err != nil {
		t.Fatalf("second SlugFor: %v", err)
	}
	if first != second {
		t.Fatalf("not idempotent: %q != %q", first, second)
	}
}

// ── Render: ISC-117, ISC-118 ───────────────────────────────────────

func TestRender_TaskWritesFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	persister := &fakePersister{}
	exf := exfiltrator.NewMarkdownExfiltrator(root, persister)

	issue := mustBrainIssue(t, "B-task01", "first task", types.TypeTask)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}

	path := filepath.Join(root, "entries", "task", "first-task.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}

	content := string(data)
	for _, want := range []string{
		"---\n",
		"id: B-task01",
		`title: "first task"`,
		"kind: task",
		"type: task",
		"priority: 2",
		"created: 2026-05-31T12:00:00Z",
		"updated: 2026-05-31T12:00:00Z",
		"\n# first task\n",
		"body for B-task01",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("file missing %q\n--- got ---\n%s", want, content)
		}
	}

	calls := persister.snapshot()
	if len(calls) != 1 || calls[0].ID != "B-task01" || calls[0].Slug != "first-task" {
		t.Fatalf("persister calls = %+v, want one SetSlug(B-task01, first-task)", calls)
	}
}

func TestRender_KnowledgeWritesFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-know01", "learned x", types.TypeKnowledge)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}

	path := filepath.Join(root, "entries", "knowledge", "learned-x.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
}

func TestRender_BothWritesFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-both01", "joint doc", types.TypeBoth)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join(root, "entries", "both", "joint-doc.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
}

func TestRender_NonBrainKindWritesFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	persister := &fakePersister{}
	exf := exfiltrator.NewMarkdownExfiltrator(root, persister)

	issue := mustBrainIssue(t, "B-bug01", "a bug", types.TypeBug)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Every kind renders. Bug lands at entries/bug/a-bug.md.
	want := filepath.Join(root, "entries", "bug", "a-bug.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected bug file at %s: %v", want, err)
	}
	if len(persister.snapshot()) != 1 {
		t.Fatalf("persister should have been called once for new slug: %+v", persister.snapshot())
	}
}

func TestRender_NilIssueErrors(t *testing.T) {
	t.Parallel()
	exf := exfiltrator.NewMarkdownExfiltrator(t.TempDir(), nil)
	if err := exf.Render(context.Background(), nil); err == nil {
		t.Fatalf("Render(nil) should error")
	}
}

func TestRender_EmptyIDErrors(t *testing.T) {
	t.Parallel()
	exf := exfiltrator.NewMarkdownExfiltrator(t.TempDir(), nil)
	issue := mustBrainIssue(t, "", "no id", types.TypeTask)
	if err := exf.Render(context.Background(), issue); err == nil {
		t.Fatalf("Render with empty ID should error")
	}
}

// ── ISC-119: re-render reflects updated fields ─────────────────────

func TestRender_RewriteOnUpdate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-up01", "before", types.TypeTask)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("first Render: %v", err)
	}

	// Simulate an update: same slug stays (persisted in metadata)
	// but description changes.
	issue.Description = "after the update"
	issue.UpdatedAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	issue.Metadata = json.RawMessage(`{"brain_slug":"before"}`)

	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("second Render: %v", err)
	}

	path := filepath.Join(root, "entries", "task", "before.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "after the update") {
		t.Fatalf("update body not present:\n%s", content)
	}
	if !strings.Contains(content, "updated: 2026-06-01T12:00:00Z") {
		t.Fatalf("updated timestamp not present:\n%s", content)
	}
}

// ── ISC-121: checkpoint lifecycle ──────────────────────────────────

func TestRender_CheckpointWrittenAndCleared(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	issue := mustBrainIssue(t, "B-ck01", "checkpointed", types.TypeTask)

	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}

	// After successful render, checkpoint file must be absent.
	cp := exf.CheckpointPath()
	if _, err := os.Stat(cp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint not cleared after success: err=%v", err)
	}
}

func TestRender_CheckpointLeftOnPersisterFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	persister := &fakePersister{err: errors.New("storage exploded")}
	exf := exfiltrator.NewMarkdownExfiltrator(root, persister)

	issue := mustBrainIssue(t, "B-ck02", "fails late", types.TypeKnowledge)
	err := exf.Render(context.Background(), issue)
	if err == nil {
		t.Fatalf("Render expected to error on persister failure")
	}

	// Body still on disk (atomic write completed before persister fired).
	path := filepath.Join(root, "entries", "knowledge", "fails-late.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should be on disk despite persister failure: %v", err)
	}
	// Checkpoint cleared on the persister-failure branch (we clear it
	// just before returning the error, so reconcile is not led astray).
	cp := exf.CheckpointPath()
	if _, err := os.Stat(cp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint should be cleared after persister failure cleanup: err=%v", err)
	}
}

// ── Remove ─────────────────────────────────────────────────────────

func TestRemove_DeletesFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	issue := mustBrainIssue(t, "B-rm01", "to delete", types.TypeTask)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join(root, "entries", "task", "to-delete.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should exist before Remove: %v", err)
	}

	if err := exf.Remove(context.Background(), issue.ID, types.TypeTask, "to-delete"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file should be gone after Remove: err=%v", err)
	}
}

func TestRemove_MissingIsIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	if err := exf.Remove(context.Background(), "B-nope", types.TypeTask, "never-existed"); err != nil {
		t.Fatalf("Remove of missing file should be nil: %v", err)
	}
}

func TestRemove_NonBrainKindRemovesFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	// Write a bug file via Render, then remove it via Remove.
	issue := mustBrainIssue(t, "B-bug", "removable bug", types.TypeBug)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join(root, "entries", "bug", "removable-bug.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected bug file before remove: %v", err)
	}

	if err := exf.Remove(context.Background(), "B-bug", types.TypeBug, "removable-bug"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file gone, stat err = %v", err)
	}
}

func TestRemove_EmptySlugErrors(t *testing.T) {
	t.Parallel()
	exf := exfiltrator.NewMarkdownExfiltrator(t.TempDir(), nil)
	if err := exf.Remove(context.Background(), "B-x", types.TypeTask, ""); err == nil {
		t.Fatalf("Remove with empty slug should error")
	}
}

// ── PathFor / Root ──────────────────────────────────────────────────

func TestPathFor_Layout(t *testing.T) {
	t.Parallel()
	exf := exfiltrator.NewMarkdownExfiltrator("/tmp/x", nil)
	got := exf.PathFor(types.TypeKnowledge, "slug")
	want := filepath.Join("/tmp/x", "entries", "knowledge", "slug.md")
	if got != want {
		t.Fatalf("PathFor = %q, want %q", got, want)
	}
}

func TestRoot_Returns(t *testing.T) {
	t.Parallel()
	exf := exfiltrator.NewMarkdownExfiltrator("/tmp/exf-root", nil)
	if exf.Root() != "/tmp/exf-root" {
		t.Fatalf("Root = %q", exf.Root())
	}
}

func TestRoot_ExpandsHome(t *testing.T) {
	// Cannot t.Parallel — t.Setenv forbids parallel.
	t.Setenv("HOME", "/tmp/home-fake")
	exf := exfiltrator.NewMarkdownExfiltrator("~/data/knowledge", nil)
	want := "/tmp/home-fake/data/knowledge"
	if exf.Root() != want {
		t.Fatalf("Root = %q, want %q", exf.Root(), want)
	}
}

// ── MetadataWithSlug ───────────────────────────────────────────────

func TestMetadataWithSlug_NewMetadata(t *testing.T) {
	t.Parallel()
	out, err := exfiltrator.MetadataWithSlug(nil, "fresh-slug")
	if err != nil {
		t.Fatalf("MetadataWithSlug: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m[exfiltrator.BrainSlugMetadataKey] != "fresh-slug" {
		t.Fatalf("slug not set: %v", m)
	}
}

func TestMetadataWithSlug_MergesExisting(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"other":"value","brain_slug":"old"}`)
	out, err := exfiltrator.MetadataWithSlug(in, "new-slug")
	if err != nil {
		t.Fatalf("MetadataWithSlug: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m[exfiltrator.BrainSlugMetadataKey] != "new-slug" {
		t.Fatalf("slug not overwritten: %v", m)
	}
	if m["other"] != "value" {
		t.Fatalf("existing field dropped: %v", m)
	}
}

func TestMetadataWithSlug_MalformedExisting(t *testing.T) {
	t.Parallel()
	if _, err := exfiltrator.MetadataWithSlug(json.RawMessage(`not json`), "x"); err == nil {
		t.Fatalf("expected error on malformed metadata")
	}
}

// ── Atomic write: tmp files do not leak ────────────────────────────

func TestRender_NoTmpLeftover(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	issue := mustBrainIssue(t, "B-tmp01", "tmp test", types.TypeTask)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "entries", "task"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".exf-") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

// ── Kind transition: write new, remove old ─────────────────────────

func TestRenderThenRemoveSimulatesKindTransition(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	// First, the issue is a knowledge doc.
	issue := mustBrainIssue(t, "B-kt01", "transitions", types.TypeKnowledge)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render knowledge: %v", err)
	}
	oldPath := filepath.Join(root, "entries", "knowledge", "transitions.md")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("expected file at %s: %v", oldPath, err)
	}

	// Recast to task. Decorator first writes at the new path...
	issue.IssueType = types.TypeTask
	issue.Metadata = json.RawMessage(`{"brain_slug":"transitions"}`)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render task: %v", err)
	}
	newPath := filepath.Join(root, "entries", "task", "transitions.md")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected file at %s: %v", newPath, err)
	}

	// ...then removes the old.
	if err := exf.Remove(context.Background(), issue.ID, types.TypeKnowledge, "transitions"); err != nil {
		t.Fatalf("Remove old: %v", err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old file should be gone: err=%v", err)
	}
}

// TestRender_EveryKnownKindWritesFile is the every-kind contract: removing
// the brain-trio gate means Render writes a file at entries/<kind>/<slug>.md
// for every IssueType the substrate recognizes. If a new kind is added to
// types.IssueType, append it here so this regression test stays exhaustive.
func TestRender_EveryKnownKindWritesFile(t *testing.T) {
	t.Parallel()

	kinds := []types.IssueType{
		types.TypeTask,
		types.TypeKnowledge,
		types.TypeBoth,
		types.TypeBug,
		types.TypeFeature,
		types.TypeEpic,
		types.TypeChore,
		types.TypeDecision,
		types.TypeMessage,
		types.TypeMolecule,
		types.TypeGate,
		types.TypeSpike,
		types.TypeStory,
		types.TypeMilestone,
		types.TypeISA,
	}

	for _, kind := range kinds {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
			issue := mustBrainIssue(t, "B-"+string(kind), "hello "+string(kind), kind)

			if err := exf.Render(context.Background(), issue); err != nil {
				t.Fatalf("Render(%s): %v", kind, err)
			}

			want := filepath.Join(root, "entries", string(kind), "hello-"+string(kind)+".md")
			body, err := os.ReadFile(want)
			if err != nil {
				t.Fatalf("expected file at %s: %v", want, err)
			}
			gotBody := string(body)
			for _, mustContain := range []string{
				"id: B-" + string(kind),
				"kind: " + string(kind),
				"type: " + string(kind),
				"# hello " + string(kind),
			} {
				if !strings.Contains(gotBody, mustContain) {
					t.Errorf("file body missing %q. Got:\n%s", mustContain, gotBody)
				}
			}
		})
	}
}

// ── OKF v0.1 + Obsidian frontmatter (ISC-1..4) ─────────────────────

// renderFile renders issue through the public Render path and returns the
// on-disk file contents. Used by the OKF frontmatter tests below since
// renderMarkdown is unexported.
func renderFile(t *testing.T, issue *types.Issue) string {
	t.Helper()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render: %v", err)
	}
	slug, _, err := exf.SlugFor(issue)
	if err != nil {
		t.Fatalf("SlugFor: %v", err)
	}
	path := exf.PathFor(issue.IssueType, slug)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(data)
}

// TestRender_TypeMirrorsKind asserts ISC-1: every entry carries a
// non-empty `type` line whose value equals `kind` verbatim, for every
// IssueType, while `kind` is retained (additive).
func TestRender_TypeMirrorsKind(t *testing.T) {
	t.Parallel()

	kinds := []types.IssueType{
		types.TypeTask,
		types.TypeKnowledge,
		types.TypeBoth,
		types.TypeBug,
		types.TypeISA,
	}
	for _, kind := range kinds {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			issue := mustBrainIssue(t, "B-"+string(kind), "mirror "+string(kind), kind)
			content := renderFile(t, issue)

			wantKind := "kind: " + string(kind) + "\n"
			wantType := "type: " + string(kind) + "\n"
			if !strings.Contains(content, wantKind) {
				t.Errorf("missing %q (kind retained, additive):\n%s", wantKind, content)
			}
			if !strings.Contains(content, wantType) {
				t.Errorf("missing %q (type mirrors kind):\n%s", wantType, content)
			}
			// `type` must sit immediately after `kind` — deterministic ordering.
			if idxKind, idxType := strings.Index(content, wantKind), strings.Index(content, wantType); idxType != idxKind+len(wantKind) {
				t.Errorf("type not directly after kind: kind@%d type@%d\n%s", idxKind, idxType, content)
			}
		})
	}
}

// TestRender_TagsMirrorLabels asserts ISC-2: `tags` mirrors `labels`
// element-for-element (same yamlString rendering), `labels` is retained,
// and `tags` sits immediately after the `labels` block.
func TestRender_TagsMirrorLabels(t *testing.T) {
	t.Parallel()
	issue := mustBrainIssue(t, "B-tags01", "tagged doc", types.TypeKnowledge)
	// Include a label needing YAML quoting to prove tags uses the same encoding.
	issue.Labels = []string{"cat-tech", "needs: review", "alpha"}

	content := renderFile(t, issue)

	wantLabels := `labels: ["cat-tech", "needs: review", "alpha"]` + "\n"
	wantTags := `tags: ["cat-tech", "needs: review", "alpha"]` + "\n"
	if !strings.Contains(content, wantLabels) {
		t.Errorf("missing labels block %q:\n%s", wantLabels, content)
	}
	if !strings.Contains(content, wantTags) {
		t.Errorf("missing tags block %q:\n%s", wantTags, content)
	}
	// tags must sit immediately after the labels block — deterministic ordering.
	if idxLabels, idxTags := strings.Index(content, wantLabels), strings.Index(content, wantTags); idxTags != idxLabels+len(wantLabels) {
		t.Errorf("tags not directly after labels: labels@%d tags@%d\n%s", idxLabels, idxTags, content)
	}
}

// TestRender_TagsOmittedWhenNoLabels asserts ISC-2's conditional: an
// entry with no labels emits neither a `labels:` nor a `tags:` line.
func TestRender_TagsOmittedWhenNoLabels(t *testing.T) {
	t.Parallel()
	issue := mustBrainIssue(t, "B-notags01", "untagged doc", types.TypeKnowledge)
	issue.Labels = nil

	content := renderFile(t, issue)

	if strings.Contains(content, "labels:") {
		t.Errorf("labels line present for label-less issue:\n%s", content)
	}
	if strings.Contains(content, "tags:") {
		t.Errorf("tags line present for label-less issue:\n%s", content)
	}
}

// TestRender_ByteStableAcrossRenders asserts ISC-3: two consecutive
// renders of the same issue snapshot produce byte-identical files. This
// underpins the reconciler idempotence guarantee (ISC-123, divergence
// 0012). Covers both the labelled (type+tags) and unlabelled paths.
func TestRender_ByteStableAcrossRenders(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		labels []string
	}{
		{"with-labels", []string{"cat-tech", "alpha", "beta"}},
		{"no-labels", nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
			issue := mustBrainIssue(t, "B-stable01", "stable doc", types.TypeKnowledge)
			issue.Labels = tc.labels
			// Pin the slug so both renders target the same path.
			issue.Metadata = json.RawMessage(`{"brain_slug":"stable-doc"}`)

			if err := exf.Render(context.Background(), issue); err != nil {
				t.Fatalf("first Render: %v", err)
			}
			path := exf.PathFor(types.TypeKnowledge, "stable-doc")
			first, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("first ReadFile: %v", err)
			}

			if err := exf.Render(context.Background(), issue); err != nil {
				t.Fatalf("second Render: %v", err)
			}
			second, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("second ReadFile: %v", err)
			}

			if !bytes.Equal(first, second) {
				t.Fatalf("render not byte-stable across two passes:\n--- first ---\n%s\n--- second ---\n%s", first, second)
			}
		})
	}
}

// TestRender_EmptyKindErrors locks in the only gate left after the brain-trio
// gate removal: an issue with no kind cannot be rendered (there is no
// directory to write to).
func TestRender_EmptyKindErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)
	issue := &types.Issue{
		ID:        "B-empty-kind",
		Title:     "no kind set",
		IssueType: "",
	}
	if err := exf.Render(context.Background(), issue); err == nil {
		t.Fatal("expected error for empty kind, got nil")
	}
	// No file should have been created.
	if _, err := os.Stat(filepath.Join(root, "entries")); err == nil {
		t.Fatal("entries dir created despite empty-kind error")
	}
}

// TestRemove_EmptyKindErrors mirrors TestRender_EmptyKindErrors for the
// Remove path.
func TestRemove_EmptyKindErrors(t *testing.T) {
	t.Parallel()
	exf := exfiltrator.NewMarkdownExfiltrator(t.TempDir(), nil)
	if err := exf.Remove(context.Background(), "B-bad", "", "slug"); err == nil {
		t.Fatal("expected error for empty kind, got nil")
	}
}

// TestRender_VeryLongTitleTruncatesSlug ensures the slug stays short enough
// that the atomic-write tmp+rename dance never trips the filesystem's 255-byte
// component cap. The two real-world failures that prompted this guard were
// 290+-byte slugs in the brain store.
func TestRender_VeryLongTitleTruncatesSlug(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	// 400-byte title — longer than any single filesystem path component allows.
	longTitle := strings.Repeat("really long question about something ", 12)
	issue := mustBrainIssue(t, "B-long", longTitle, types.TypeKnowledge)

	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("Render with long title: %v", err)
	}

	// The written file must exist and its basename (incl. ".md") must fit.
	mx := exf
	slug, _, err := mx.SlugFor(issue)
	if err != nil {
		t.Fatalf("SlugFor: %v", err)
	}
	if len(slug) > 200 {
		t.Errorf("slug = %d bytes, want <= 200", len(slug))
	}
	path := mx.PathFor(types.TypeKnowledge, slug)
	base := filepath.Base(path)
	if len(base) > 255 {
		t.Fatalf("file basename = %d bytes, exceeds POSIX 255 cap: %s", len(base), base)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
}

// ── Related-edge rendering (isa-6zq ISC-5..7) ──────────────────────

// relatedIssue builds a brain issue with a pinned slug (so both renders
// target the same path) and the given resolved related-links.
func relatedIssue(t *testing.T, id, slug string, links []types.RelatedLink) *types.Issue {
	t.Helper()
	issue := mustBrainIssue(t, id, "src "+id, types.TypeKnowledge)
	issue.Metadata = json.RawMessage(`{"brain_slug":"` + slug + `"}`)
	issue.RelatedLinks = links
	return issue
}

// TestRender_NoEdgesNoRelatedBlock asserts ISC-6: an entry with no edges
// renders no `## Related` block, and is byte-identical to the same issue
// with RelatedLinks left nil (the WS1 edgeless render). This pins that
// the new code path adds zero bytes when there are no edges.
func TestRender_NoEdgesNoRelatedBlock(t *testing.T) {
	t.Parallel()

	// Empty slice and nil must both produce the edgeless render.
	for _, name := range []string{"nil", "empty"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var links []types.RelatedLink
			if name == "empty" {
				links = []types.RelatedLink{}
			}
			withLinks := relatedIssue(t, "B-noedge", "noedge-doc", links)
			content := renderFile(t, withLinks)
			if strings.Contains(content, "## Related") {
				t.Fatalf("unexpected Related block for edgeless issue:\n%s", content)
			}

			// Byte-identical to a baseline issue that never set RelatedLinks.
			baseline := relatedIssue(t, "B-noedge", "noedge-doc", nil)
			baseline.RelatedLinks = nil
			baselineContent := renderFile(t, baseline)
			if content != baselineContent {
				t.Fatalf("edgeless render drifted from baseline:\n--- with ---\n%s\n--- baseline ---\n%s", content, baselineContent)
			}
		})
	}
}

// TestRender_SingleEdgeMarkdownLink asserts ISC-5: one resolved edge
// renders as a standard markdown link under `## Related` pointing at the
// OKF bundle-root-relative `/entries/<kind>/<slug>.md` path, annotated
// with the edge type.
func TestRender_SingleEdgeMarkdownLink(t *testing.T) {
	t.Parallel()

	links := []types.RelatedLink{{
		TargetID: "B-target",
		Title:    "Target Doc",
		Kind:     types.TypeTask,
		Slug:     "target-doc",
		EdgeType: types.DepRelatesTo,
		Resolved: true,
	}}
	content := renderFile(t, relatedIssue(t, "B-src1", "src-one", links))

	if !strings.Contains(content, "\n## Related\n\n") {
		t.Fatalf("missing Related heading:\n%s", content)
	}
	want := "- [Target Doc](/entries/task/target-doc.md) — relates-to\n"
	if !strings.Contains(content, want) {
		t.Fatalf("missing expected link %q:\n%s", want, content)
	}
	// The link is a standard markdown link, never a wikilink.
	if strings.Contains(content, "[[") {
		t.Fatalf("wikilink syntax leaked into render:\n%s", content)
	}
}

// TestRender_MultipleEdgesDeterministicOrder asserts ISC-3 + ISC-5:
// multiple edges render in a deterministic (edge-type, slug) order
// regardless of the input slice order.
func TestRender_MultipleEdgesDeterministicOrder(t *testing.T) {
	t.Parallel()

	// Intentionally unsorted input: mixed edge types and slugs.
	links := []types.RelatedLink{
		{TargetID: "B-z", Title: "Zeta", Kind: types.TypeKnowledge, Slug: "zeta", EdgeType: types.DepRelatesTo, Resolved: true},
		{TargetID: "B-a", Title: "Alpha", Kind: types.TypeTask, Slug: "alpha", EdgeType: types.DepExtends, Resolved: true},
		{TargetID: "B-m", Title: "Mid", Kind: types.TypeKnowledge, Slug: "mid", EdgeType: types.DepRelatesTo, Resolved: true},
	}
	content := renderFile(t, relatedIssue(t, "B-multi", "multi-src", links))

	// Expected order: extends (alpha) < relates-to (mid) < relates-to (zeta).
	// Primary sort by edge type, secondary by slug.
	wantOrder := []string{
		"- [Alpha](/entries/task/alpha.md) — extends\n",
		"- [Mid](/entries/knowledge/mid.md) — relates-to\n",
		"- [Zeta](/entries/knowledge/zeta.md) — relates-to\n",
	}
	lastIdx := -1
	for _, w := range wantOrder {
		idx := strings.Index(content, w)
		if idx < 0 {
			t.Fatalf("missing link %q:\n%s", w, content)
		}
		if idx <= lastIdx {
			t.Fatalf("link %q out of deterministic order (idx %d <= %d):\n%s", w, idx, lastIdx, content)
		}
		lastIdx = idx
	}
}

// TestRender_EdgesByteStableAcrossRenders asserts ISC-3: two renders of
// the same issue WITH edges are byte-identical, even when the input slice
// order differs between the two passes (determinism is enforced by the
// renderer's sort, not by caller ordering).
func TestRender_EdgesByteStableAcrossRenders(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	exf := exfiltrator.NewMarkdownExfiltrator(root, nil)

	base := []types.RelatedLink{
		{TargetID: "B-a", Title: "Alpha", Kind: types.TypeTask, Slug: "alpha", EdgeType: types.DepExtends, Resolved: true},
		{TargetID: "B-b", Title: "Beta", Kind: types.TypeKnowledge, Slug: "beta", EdgeType: types.DepRelatesTo, Resolved: true},
		{TargetID: "B-c", Title: "Gamma", Kind: types.TypeKnowledge, Slug: "gamma", EdgeType: types.DepRelatesTo, Resolved: true},
	}
	issue := relatedIssue(t, "B-stableedge", "stable-edge", base)

	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("first Render: %v", err)
	}
	path := exf.PathFor(types.TypeKnowledge, "stable-edge")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("first ReadFile: %v", err)
	}

	// Second pass: same edges, reversed input order. Render must be identical.
	reversed := make([]types.RelatedLink, len(base))
	for i := range base {
		reversed[len(base)-1-i] = base[i]
	}
	issue.RelatedLinks = reversed
	if err := exf.Render(context.Background(), issue); err != nil {
		t.Fatalf("second Render: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("second ReadFile: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("edge render not byte-stable across passes / input orderings:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestRender_UnresolvedEdgeBestEffort asserts ISC-6: an unresolved
// (cross-store / deleted) edge renders a best-effort link tagged
// `(unresolved)` rather than failing the render or being silently dropped.
func TestRender_UnresolvedEdgeBestEffort(t *testing.T) {
	t.Parallel()

	links := []types.RelatedLink{
		{TargetID: "OTHER-42", EdgeType: types.DepRelatesTo, Resolved: false},
		{TargetID: "B-ok", Title: "Resolved One", Kind: types.TypeTask, Slug: "resolved-one", EdgeType: types.DepExtends, Resolved: true},
	}
	content := renderFile(t, relatedIssue(t, "B-mixed", "mixed-src", links))

	wantUnresolved := "- [OTHER-42](/entries/OTHER-42.md) — relates-to (unresolved)\n"
	if !strings.Contains(content, wantUnresolved) {
		t.Fatalf("missing best-effort unresolved link %q:\n%s", wantUnresolved, content)
	}
	wantResolved := "- [Resolved One](/entries/task/resolved-one.md) — extends\n"
	if !strings.Contains(content, wantResolved) {
		t.Fatalf("missing resolved link %q:\n%s", wantResolved, content)
	}
	// A best-effort render must still succeed and still carry the heading.
	if !strings.Contains(content, "## Related") {
		t.Fatalf("Related heading missing despite one resolvable edge:\n%s", content)
	}
}

// TestRender_LinkTextEscapesBrackets asserts the link label escapes the
// `]` character so a title containing brackets cannot break the markdown
// link (which would make it unresolvable in OKF and Obsidian).
func TestRender_LinkTextEscapesBrackets(t *testing.T) {
	t.Parallel()

	links := []types.RelatedLink{{
		TargetID: "B-brk",
		Title:    "Title [with] brackets",
		Kind:     types.TypeKnowledge,
		Slug:     "brk",
		EdgeType: types.DepRelatesTo,
		Resolved: true,
	}}
	content := renderFile(t, relatedIssue(t, "B-esc", "esc-src", links))

	want := `- [Title [with\] brackets](/entries/knowledge/brk.md) — relates-to` + "\n"
	if !strings.Contains(content, want) {
		t.Fatalf("bracket in title not escaped in link label:\n%s\nwant substring: %q", content, want)
	}
}

// ── WS2 render-time `## Related` suppression (isa-6zq reconciliation) ──

// storedRelatedIssue builds a brain issue whose Description already carries
// a stored `## Related` block (as the one-shot backfill wrote), pinned to a
// stable slug, with the given resolved related-links populated on top.
func storedRelatedIssue(t *testing.T, id, slug, desc string, links []types.RelatedLink) *types.Issue {
	t.Helper()
	issue := mustBrainIssue(t, id, "src "+id, types.TypeKnowledge)
	issue.Description = desc
	issue.Metadata = json.RawMessage(`{"brain_slug":"` + slug + `"}`)
	issue.RelatedLinks = links
	return issue
}

// TestRender_StoredMarkedRelatedSuppressesWS2 asserts that when a
// description already contains a stored `## Related` block terminated by the
// `brain:auto-related` marker AND the issue has live edges, the render emits
// EXACTLY ONE `## Related` heading — the stored one — and the WS2 render-time
// block is suppressed.
func TestRender_StoredMarkedRelatedSuppressesWS2(t *testing.T) {
	t.Parallel()

	desc := "Some body text.\n\n## Related\n\n- [[stored-target]]\n<!-- /brain:auto-related -->\n"
	links := []types.RelatedLink{{
		TargetID: "B-live",
		Title:    "Live Edge",
		Kind:     types.TypeTask,
		Slug:     "live-edge",
		EdgeType: types.DepRelatesTo,
		Resolved: true,
	}}
	content := renderFile(t, storedRelatedIssue(t, "B-stored1", "stored-one", desc, links))

	if n := strings.Count(content, "## Related"); n != 1 {
		t.Fatalf("want exactly one `## Related` heading, got %d:\n%s", n, content)
	}
	// The surviving block is the stored wikilink block, not the WS2 markdown link.
	if !strings.Contains(content, "[[stored-target]]") {
		t.Fatalf("stored wikilink block did not survive:\n%s", content)
	}
	if strings.Contains(content, "/entries/task/live-edge.md") {
		t.Fatalf("WS2 render-time block leaked despite stored block present:\n%s", content)
	}
}

// TestRender_StoredBareRelatedSuppressesWS2 asserts the fallback detection:
// a description with a bare `## Related` line (no `brain:auto-related` marker)
// plus populated RelatedLinks still suppresses the WS2 block, leaving exactly
// one `## Related` heading.
func TestRender_StoredBareRelatedSuppressesWS2(t *testing.T) {
	t.Parallel()

	desc := "Intro paragraph.\n\n## Related\n\nSee also the other doc.\n"
	links := []types.RelatedLink{{
		TargetID: "B-live",
		Title:    "Live Edge",
		Kind:     types.TypeTask,
		Slug:     "live-edge",
		EdgeType: types.DepExtends,
		Resolved: true,
	}}
	content := renderFile(t, storedRelatedIssue(t, "B-stored2", "stored-two", desc, links))

	if n := strings.Count(content, "## Related"); n != 1 {
		t.Fatalf("want exactly one `## Related` heading, got %d:\n%s", n, content)
	}
	if strings.Contains(content, "/entries/task/live-edge.md") {
		t.Fatalf("WS2 render-time block leaked despite bare stored block present:\n%s", content)
	}
}

// TestRender_NoStoredRelatedEmitsWS2 asserts unchanged behavior: a
// description with NO `## Related` section plus populated RelatedLinks emits
// the WS2 render-time block exactly as before this guard existed.
func TestRender_NoStoredRelatedEmitsWS2(t *testing.T) {
	t.Parallel()

	desc := "Just a body paragraph with no related section.\n"
	links := []types.RelatedLink{{
		TargetID: "B-live",
		Title:    "Live Edge",
		Kind:     types.TypeTask,
		Slug:     "live-edge",
		EdgeType: types.DepRelatesTo,
		Resolved: true,
	}}
	content := renderFile(t, storedRelatedIssue(t, "B-stored3", "stored-three", desc, links))

	if n := strings.Count(content, "## Related"); n != 1 {
		t.Fatalf("want exactly one WS2 `## Related` heading, got %d:\n%s", n, content)
	}
	want := "- [Live Edge](/entries/task/live-edge.md) — relates-to\n"
	if !strings.Contains(content, want) {
		t.Fatalf("WS2 render-time link missing when no stored block present:\n%s\nwant substring: %q", content, want)
	}
}

// TestRender_NoStoredRelatedNoEdgesByteStable asserts ISC-3: a description
// with NO `## Related` section and NO edges renders byte-identically to the
// pre-guard no-Related baseline (an issue rendered with RelatedLinks nil and
// no stored block). The suppression guard must add zero bytes on this path.
func TestRender_NoStoredRelatedNoEdgesByteStable(t *testing.T) {
	t.Parallel()

	desc := "A plain body with no related section at all.\n"

	withGuardPath := storedRelatedIssue(t, "B-stored4", "stored-four", desc, nil)
	withGuardPath.RelatedLinks = nil
	content := renderFile(t, withGuardPath)

	if strings.Contains(content, "## Related") {
		t.Fatalf("unexpected `## Related` block for edgeless, no-stored-block issue:\n%s", content)
	}

	// Baseline: identical issue, RelatedLinks nil, same empty stored state.
	baseline := storedRelatedIssue(t, "B-stored4", "stored-four", desc, nil)
	baseline.RelatedLinks = nil
	baselineContent := renderFile(t, baseline)

	if content != baselineContent {
		t.Fatalf("guarded no-Related render drifted from baseline:\n--- with ---\n%s\n--- baseline ---\n%s", content, baselineContent)
	}
}
