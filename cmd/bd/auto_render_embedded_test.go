//go:build cgo

package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bdRunWithEnv runs a bd subcommand with BRAIN_ISA_EXFIL_ROOT injected. Returns
// (combined output, error) so callers can assert exit code AND inspect stderr.
func bdRunWithEnv(t *testing.T, bd, dir, exfilRoot string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = append(bdEnv(dir), "BRAIN_ISA_EXFIL_ROOT="+exfilRoot)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// bdPatchOKWithEnv runs `bd patch` with an exfil root in the env. Fails the
// test on non-zero exit. Used by the auto-render happy-path subtests.
func bdPatchOKWithEnv(t *testing.T, bd, dir, exfilRoot string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"patch"}, args...)
	out, err := bdRunWithEnv(t, bd, dir, exfilRoot, fullArgs...)
	if err != nil {
		t.Fatalf("bd patch %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return out
}

// bdISASectionOKWithEnv runs `bd isa-section` with an exfil root in the env.
func bdISASectionOKWithEnv(t *testing.T, bd, dir, exfilRoot string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-section"}, args...)
	out, err := bdRunWithEnv(t, bd, dir, exfilRoot, fullArgs...)
	if err != nil {
		t.Fatalf("bd isa-section %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return out
}

// bdRenderPending runs `bd isa-render-pending` and returns the combined output.
// Always-zero-exit semantics (we exit 0 for both empty and populated).
func bdRenderPending(t *testing.T, bd, dir, exfilRoot string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-render-pending"}, args...)
	out, err := bdRunWithEnv(t, bd, dir, exfilRoot, fullArgs...)
	if err != nil {
		t.Fatalf("bd isa-render-pending %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return out
}

// TestAutoRenderOnPatch covers ISC-33: a successful `bd patch` of an ISA
// field auto-renders the on-disk markdown synchronously.
func TestAutoRenderOnPatch(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "arpatch")
	exfilRoot := t.TempDir()

	const (
		id   = "arpatch-isa-001"
		slug = "auto-render-patch"
	)

	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowFull(t, db, id, "Auto-render on patch", slug, "OBSERVE", 0, 4)
		insertISASectionRow(t, db, id, "problem", "we need the auto-render hook")
	})

	// Seed disk with an initial render so we can detect mtime advancement.
	out, err := bdRunWithEnv(t, bd, dir, exfilRoot, "isa-render", id)
	if err != nil {
		t.Fatalf("initial bd isa-render failed: %v\n%s", err, out)
	}
	targetPath := filepath.Join(exfilRoot, "isa", slug, "ISA.md")
	preStat, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("stat initial render: %v", err)
	}
	preMtime := preStat.ModTime()

	// Wait so the post-patch mtime is strictly greater than the seed mtime
	// even on second-precision filesystems.
	time.Sleep(1100 * time.Millisecond)

	// Patch isa_phase to BUILD. The auto-render hook should fire.
	bdPatchOKWithEnv(t, bd, dir, exfilRoot, id, "--field", "isa_phase", "--value", "BUILD")

	postStat, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("stat post-patch render: %v", err)
	}
	if !postStat.ModTime().After(preMtime) {
		t.Errorf("expected ISA.md mtime to advance after auto-render; pre=%s post=%s",
			preMtime, postStat.ModTime())
	}

	body, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read post-patch render: %v", err)
	}
	if !strings.Contains(string(body), "phase: build") {
		t.Errorf("expected 'phase: build' in re-rendered ISA, got:\n%s", body)
	}
}

// TestAutoRenderOnSection covers ISC-34: a successful `bd isa-section` UPSERT
// auto-renders the on-disk markdown synchronously.
func TestAutoRenderOnSection(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "arsec")
	exfilRoot := t.TempDir()

	const (
		id   = "arsec-isa-001"
		slug = "auto-render-section"
	)

	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowFull(t, db, id, "Auto-render on section", slug, "OBSERVE", 0, 0)
	})

	// Write a section body to a tempfile and call isa-section --value-from-file.
	body := "we need ISC-34 wired"
	bodyFile := filepath.Join(t.TempDir(), "problem.md")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
		t.Fatalf("write tempfile: %v", err)
	}

	bdISASectionOKWithEnv(t, bd, dir, exfilRoot, id, "problem", "--value-from-file", bodyFile)

	// File should exist on disk (auto-render seeded it; we never called
	// `bd isa-render` first) and contain the section body.
	targetPath := filepath.Join(exfilRoot, "isa", slug, "ISA.md")
	rendered, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read auto-rendered file at %s: %v", targetPath, err)
	}
	for _, must := range []string{"## Problem", body, `slug: "auto-render-section"`} {
		if !strings.Contains(string(rendered), must) {
			t.Errorf("expected rendered ISA.md to contain %q, got:\n%s", must, rendered)
		}
	}
}

// TestAutoRenderDiskFailure covers ISC-40: when the post-commit render fails
// (read-only exfil root), the brain write is NOT rolled back, a stderr
// warning is emitted, and `bd isa-render-pending` surfaces the stale row.
func TestAutoRenderDiskFailure(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "ardisk")

	// Build a read-only exfil root. The directory itself must exist (so
	// ResolveTargetPath can MkdirAll its child), but be unwritable. We chmod
	// it back to 0o755 in t.Cleanup so t.TempDir's reaper can remove it.
	exfilRoot := t.TempDir()
	if err := os.Chmod(exfilRoot, 0o555); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(exfilRoot, 0o755)
	})

	const (
		id   = "ardisk-isa-001"
		slug = "auto-render-disk-failure"
	)

	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowFull(t, db, id, "Auto-render disk failure", slug, "OBSERVE", 0, 4)
	})

	// Patch isa_phase. We expect:
	//  (a) bd patch exits 0 — the brain write succeeded.
	//  (b) stderr contains the warning string.
	//  (c) DB confirms the new isa_phase value.
	//  (d) bd isa-render-pending lists this row.
	out, err := bdRunWithEnv(t, bd, dir, exfilRoot,
		"patch", id, "--field", "isa_phase", "--value", "BUILD")
	if err != nil {
		t.Fatalf("bd patch should exit 0 even on render failure; got err=%v\noutput:\n%s",
			err, out)
	}
	if !strings.Contains(out, "warning: brain write succeeded but markdown render failed") {
		t.Errorf("expected stderr warning, got output:\n%s", out)
	}

	// Confirm the DB was updated despite the render failure.
	withTestDB(t, beadsDir, func(db *sql.DB) {
		var phase sql.NullString
		err := db.QueryRowContext(t.Context(),
			"SELECT isa_phase FROM issues WHERE id = ?", id,
		).Scan(&phase)
		if err != nil {
			t.Fatalf("re-read isa_phase: %v", err)
		}
		if !phase.Valid || phase.String != "BUILD" {
			t.Errorf("expected isa_phase=BUILD after patch, got valid=%v value=%q",
				phase.Valid, phase.String)
		}
	})

	// Need read access to the exfil dir for isa-render-pending to stat the
	// (missing) file. 0o555 still allows reads/traversal, so we're fine.
	pendingOut := bdRenderPending(t, bd, dir, exfilRoot)
	if !strings.Contains(pendingOut, id) {
		t.Errorf("expected bd isa-render-pending to list %s, got:\n%s", id, pendingOut)
	}
}

// TestRenderPendingEmpty: a freshly-rendered ISA emits no output.
func TestRenderPendingEmpty(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "rpe")
	exfilRoot := t.TempDir()

	const (
		id   = "rpe-isa-001"
		slug = "pending-empty"
	)

	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowFull(t, db, id, "Pending Empty", slug, "OBSERVE", 0, 0)
	})

	// Render synchronously so file mtime ≥ isa_updated_at.
	if out, err := bdRunWithEnv(t, bd, dir, exfilRoot, "isa-render", id); err != nil {
		t.Fatalf("seed isa-render failed: %v\n%s", err, out)
	}

	out := bdRenderPending(t, bd, dir, exfilRoot)
	// Strict empty check: text mode should print nothing at all when no row
	// is stale.
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty bd isa-render-pending output, got:\n%s", out)
	}
}

// TestRenderPendingMissing: an ISA row with no rendered file on disk.
func TestRenderPendingMissing(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "rpm")
	exfilRoot := t.TempDir()

	const (
		id   = "rpm-isa-001"
		slug = "pending-missing"
	)

	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowFull(t, db, id, "Pending Missing", slug, "OBSERVE", 0, 0)
	})

	// No render performed — file does not exist.
	out := bdRenderPending(t, bd, dir, exfilRoot)
	if !strings.Contains(out, id) {
		t.Errorf("expected output to mention %s, got:\n%s", id, out)
	}
	if !strings.Contains(out, "missing") {
		t.Errorf("expected reason 'missing' in output, got:\n%s", out)
	}
}

// TestRenderPendingStale: a row whose isa_updated_at has advanced past the
// file mtime (simulated by raw SQL bypassing the auto-render hook).
func TestRenderPendingStale(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "rps")
	exfilRoot := t.TempDir()

	const (
		id   = "rps-isa-001"
		slug = "pending-stale"
	)

	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowFull(t, db, id, "Pending Stale", slug, "OBSERVE", 0, 0)
	})

	// Render to seed the file.
	if out, err := bdRunWithEnv(t, bd, dir, exfilRoot, "isa-render", id); err != nil {
		t.Fatalf("seed isa-render failed: %v\n%s", err, out)
	}

	// Sleep so the next isa_updated_at is strictly greater than the file
	// mtime even on second-precision filesystems.
	time.Sleep(1100 * time.Millisecond)

	// Bump isa_updated_at via raw SQL — bypasses the auto-render hook so the
	// file mtime stays behind.
	withTestDB(t, beadsDir, func(db *sql.DB) {
		if _, err := db.ExecContext(t.Context(),
			"UPDATE issues SET isa_updated_at = NOW() WHERE id = ?", id,
		); err != nil {
			t.Fatalf("bump isa_updated_at: %v", err)
		}
	})

	out := bdRenderPending(t, bd, dir, exfilRoot)
	if !strings.Contains(out, id) {
		t.Errorf("expected output to mention stale row %s, got:\n%s", id, out)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("expected reason 'stale' in output, got:\n%s", out)
	}
}

// TestRenderPendingJSON: --json emits a parseable JSON array with the
// documented shape.
func TestRenderPendingJSON(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "rpj")
	exfilRoot := t.TempDir()

	const (
		id   = "rpj-isa-001"
		slug = "pending-json"
	)

	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowFull(t, db, id, "Pending JSON", slug, "OBSERVE", 0, 0)
	})

	out := bdRenderPending(t, bd, dir, exfilRoot, "--json")

	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("expected valid JSON array, got error %v:\n%s", err, out)
	}
	if len(got) == 0 {
		t.Fatalf("expected at least one entry, got: %s", out)
	}
	var found bool
	for _, entry := range got {
		if entry["id"] == id {
			found = true
			if entry["slug"] != slug {
				t.Errorf("expected slug=%q, got %v", slug, entry["slug"])
			}
			if reason, _ := entry["reason"].(string); reason != "missing" {
				t.Errorf("expected reason=missing, got %v", entry["reason"])
			}
			// file_mtime should be nil for a missing file.
			if entry["file_mtime"] != nil {
				t.Errorf("expected file_mtime=null for missing, got %v", entry["file_mtime"])
			}
			// isa_updated_at should be a non-empty string.
			if s, _ := entry["isa_updated_at"].(string); s == "" {
				t.Errorf("expected non-empty isa_updated_at, got %v", entry["isa_updated_at"])
			} else {
				if _, err := time.Parse(time.RFC3339, s); err != nil {
					t.Errorf("isa_updated_at not RFC3339: %v (%q)", err, s)
				}
			}
		}
	}
	if !found {
		t.Errorf("expected entry with id=%q in JSON output, got: %s", id, out)
	}
}
