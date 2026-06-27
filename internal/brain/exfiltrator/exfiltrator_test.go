package exfiltrator_test

import (
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
