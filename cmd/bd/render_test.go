package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

func TestFormatRenderAllSummary_Success(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(42, 42, 0, "/tmp/store")
	want := "Exfiltrated 42 / 42 beads to /tmp/store/entries/ (0 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatRenderAllSummary_PartialFailure(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(39, 42, 3, "/home/me/data/brain")
	want := "Exfiltrated 39 / 42 beads to /home/me/data/brain/entries/ (3 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatRenderAllSummary_AllFailed(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(0, 5, 5, "/var/store")
	want := "Exfiltrated 0 / 5 beads to /var/store/entries/ (5 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatRenderAllSummary_EmptyStore(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(0, 0, 0, "/empty")
	want := "Exfiltrated 0 / 0 beads to /empty/entries/ (0 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestReconcileDirtyExfiltrationRepairsRemovalAndClearsConfirmedRecords(t *testing.T) {
	root := t.TempDir()
	mx := exfiltrator.NewMarkdownExfiltrator(root, nil)
	remove := exfiltrator.DirtyRecord{ID: "task-1", Op: "remove", Kind: types.IssueType("task"), Slug: "old"}
	render := exfiltrator.DirtyRecord{ID: "task-2", Op: "render"}
	gone := exfiltrator.DirtyRecord{ID: "task-3", Op: "render"}
	failing := exfiltrator.DirtyRecord{ID: "task-4", Op: "render"}
	unlisted := exfiltrator.DirtyRecord{ID: "task-5", Op: "render"}
	for _, record := range []exfiltrator.DirtyRecord{remove, render, gone, failing, unlisted} {
		if err := mx.RecordDrift(record); err != nil {
			t.Fatal(err)
		}
	}
	orphan := mx.PathFor(remove.Kind, remove.Slug)
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replayFailedRemovals(context.Background(), mx); err != nil {
		t.Fatal(err)
	}
	deleted := func(_ context.Context, id string) bool { return id == gone.ID }
	if err := reconcileDirtyRenders(context.Background(), mx, map[string]bool{render.ID: true, failing.ID: false}, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if records, err := exfiltrator.DirtyRecords(root); err != nil || len(records) != 2 || records[0].ID != failing.ID || records[1].ID != unlisted.ID {
		t.Fatalf("dirty records = %#v, err=%v", records, err)
	}
}
