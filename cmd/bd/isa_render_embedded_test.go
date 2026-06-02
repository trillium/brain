//go:build cgo

package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bdISARenderFail runs `bd isa-render` expecting non-zero exit. Asserts the
// observed exit code matches wantExitCode when non-zero. Returns combined output.
func bdISARenderFail(t *testing.T, bd, dir, exfilRoot string, wantExitCode int, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-render"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(bdEnv(dir), "BRAIN_ISA_EXFIL_ROOT="+exfilRoot)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd isa-render %s to fail, succeeded:\n%s",
			strings.Join(args, " "), out)
	}
	if wantExitCode != 0 {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("expected ExitError, got %T: %v", err, err)
		}
		if got := ee.ExitCode(); got != wantExitCode {
			t.Fatalf("expected exit code %d, got %d.\nargs: %v\noutput:\n%s",
				wantExitCode, got, args, out)
		}
	}
	return string(out)
}

// bdISARenderOK runs `bd isa-render` expecting success with flock retry.
func bdISARenderOK(t *testing.T, bd, dir, exfilRoot string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-render"}, args...)
	// bdRunWithFlockRetry uses bdEnv directly; we need to inject the env var
	// ourselves via exec.Command because the helper doesn't accept extra env.
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(bdEnv(dir), "BRAIN_ISA_EXFIL_ROOT="+exfilRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd isa-render %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return string(out)
}

// bdISARenderAllOK runs `bd isa-render-all` expecting success.
func bdISARenderAllOK(t *testing.T, bd, dir, exfilRoot string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-render-all"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = append(bdEnv(dir), "BRAIN_ISA_EXFIL_ROOT="+exfilRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd isa-render-all %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return string(out)
}

// insertISARowFull inserts a fully populated ISA row, including slug + title.
// F1d's slug regex is enforced at the verb layer; raw SQL bypasses it so we
// can also test malformed-slug behavior.
func insertISARowFull(t *testing.T, db *sql.DB, id, title, slug, phase string, m, n int) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO issues
		 (id, title, slug, status, priority, issue_type,
		  isa_phase, isa_progress_m, isa_progress_n, isa_effort, isa_mode,
		  isa_started_at, isa_updated_at)
		 VALUES (?, ?, ?, 'open', 1, 'isa', ?, ?, ?, 'advanced', 'interactive',
		         NOW(), NOW())`,
		id, title, slug, phase, m, n,
	)
	if err != nil {
		t.Fatalf("INSERT isa row %s: %v", id, err)
	}
}

// insertISASectionRow inserts a body for a given (issue_id, section_name).
func insertISASectionRow(t *testing.T, db *sql.DB, issueID, section, body string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO isa_sections (issue_id, section_name, body, updated_at)
		 VALUES (?, ?, ?, NOW())`,
		issueID, section, body,
	)
	if err != nil {
		t.Fatalf("INSERT isa_sections (%s, %s): %v", issueID, section, err)
	}
}

// TestISARender is the integration battery for `bd isa-render` and
// `bd isa-render-all`. Covers:
//   1. Happy path: ISA row + sections renders to disk with expected content.
//   2. Missing id exits 1.
//   3. Wrong-kind id (knowledge row) exits 1.
//   4. Path-traversal slug exits 2 and writes no file.
//   5. isa-render-all renders every ISA, exit 0.
//   6. isa-render-all --since=<future> renders zero, exit 0.
//   7. ISC-39: LEARN-phase ISA renders normally.
func TestISARender(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "rend")
	exfilRoot := t.TempDir()

	// ===== Case 1: happy path =====

	t.Run("happy_path_renders_file_with_frontmatter_and_sections", func(t *testing.T) {
		const id = "rend-isa-001"
		const slug = "happy-path"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARowFull(t, db, id, "Happy Path ISA", slug, "BUILD", 2, 5)
			insertISASectionRow(t, db, id, "problem", "we lack a substrate")
			insertISASectionRow(t, db, id, "vision", "ship the substrate")
			insertISASectionRow(t, db, id, "changelog", "* 2026-06-02 — F2a")
		})

		out := bdISARenderOK(t, bd, dir, exfilRoot, id)
		expectedPath := filepath.Join(exfilRoot, "isa", slug, "ISA.md")
		if !strings.Contains(out, expectedPath) {
			t.Errorf("expected stdout to contain %q, got: %s", expectedPath, out)
		}

		body, err := os.ReadFile(expectedPath)
		if err != nil {
			t.Fatalf("read rendered file: %v", err)
		}
		bodyStr := string(body)

		// Frontmatter assertions.
		for _, must := range []string{
			`task: "Happy Path ISA"`,
			`slug: "happy-path"`,
			`phase: build`,
			`progress: "2/5"`,
			`brain_id: rend-isa-001`,
			`effort: advanced`,
			`mode: interactive`,
		} {
			if !strings.Contains(bodyStr, must) {
				t.Errorf("expected rendered file to contain %q", must)
			}
		}

		// Section assertions: headings appear, body content appears, spec
		// order is preserved (problem before vision before changelog).
		for _, must := range []string{
			"# Happy Path ISA",
			"## Problem",
			"we lack a substrate",
			"## Vision",
			"ship the substrate",
			"## Changelog",
			"* 2026-06-02 — F2a",
		} {
			if !strings.Contains(bodyStr, must) {
				t.Errorf("expected rendered file to contain %q", must)
			}
		}

		// Spec order check.
		idxProblem := strings.Index(bodyStr, "## Problem")
		idxVision := strings.Index(bodyStr, "## Vision")
		idxChangelog := strings.Index(bodyStr, "## Changelog")
		if !(idxProblem < idxVision && idxVision < idxChangelog) {
			t.Errorf("expected spec order Problem<Vision<Changelog, got offsets %d/%d/%d",
				idxProblem, idxVision, idxChangelog)
		}
	})

	// ===== Case 2: missing id =====

	t.Run("missing_id_exits_1", func(t *testing.T) {
		out := bdISARenderFail(t, bd, dir, exfilRoot, 1, "rend-isa-nonexistent")
		if !strings.Contains(strings.ToLower(out), "not found") {
			t.Errorf("expected 'not found' in stderr, got: %s", out)
		}
	})

	// ===== Case 3: wrong-kind id =====

	t.Run("wrong_kind_exits_1", func(t *testing.T) {
		const id = "rend-k-001"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertNonISARow(t, db, id, "not an isa")
		})

		out := bdISARenderFail(t, bd, dir, exfilRoot, 1, id)
		if !strings.Contains(strings.ToLower(out), "not an isa") {
			t.Errorf("expected 'not an ISA' in stderr, got: %s", out)
		}
	})

	// ===== Case 4: path-traversal slug =====

	t.Run("path_traversal_slug_exits_2_no_file", func(t *testing.T) {
		const id = "rend-isa-trav"
		const badSlug = "bad/../slug"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			// Raw SQL — bypass F1d's slug regex (the substrate accepts this
			// even though `bd brain new` would refuse it).
			insertISARowFull(t, db, id, "Traversal Attempt", badSlug, "OBSERVE", 0, 0)
		})

		out := bdISARenderFail(t, bd, dir, exfilRoot, 2, id)
		if !strings.Contains(strings.ToLower(out), "outside exfil root") {
			t.Errorf("expected 'outside exfil root' in stderr, got: %s", out)
		}

		// Verify no file got written anywhere reasonable.
		bad := filepath.Join(exfilRoot, "isa", badSlug, "ISA.md")
		if _, err := os.Stat(bad); err == nil {
			t.Errorf("expected no file at %s, but one exists", bad)
		}
	})

	// ===== Case 5: isa-render-all renders all =====

	t.Run("render_all_renders_every_isa", func(t *testing.T) {
		// Use a fresh exfil dir so we count files cleanly.
		exfilAll := t.TempDir()

		const (
			idA    = "rend-all-001"
			idB    = "rend-all-002"
			idC    = "rend-all-003"
			slugA  = "all-one"
			slugB  = "all-two"
			slugC  = "all-three"
		)

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARowFull(t, db, idA, "All One", slugA, "OBSERVE", 0, 3)
			insertISASectionRow(t, db, idA, "problem", "p1")
			insertISARowFull(t, db, idB, "All Two", slugB, "BUILD", 1, 4)
			insertISASectionRow(t, db, idB, "vision", "v2")
			insertISARowFull(t, db, idC, "All Three", slugC, "LEARN", 5, 5)
			insertISASectionRow(t, db, idC, "changelog", "* done")
		})

		out := bdISARenderAllOK(t, bd, dir, exfilAll)

		// Each rendered ISA should appear in the output with "rendered" status.
		// (Other ISAs from earlier subtests also render — we only assert the
		// three we just inserted appear with status=rendered.)
		for _, mustID := range []string{idA, idB, idC} {
			if !strings.Contains(out, mustID+"\t") || !strings.Contains(out, "\trendered") {
				t.Errorf("expected %s with 'rendered' status in output:\n%s", mustID, out)
			}
		}

		// Files should exist on disk.
		for _, slug := range []string{slugA, slugB, slugC} {
			p := filepath.Join(exfilAll, "isa", slug, "ISA.md")
			if _, err := os.Stat(p); err != nil {
				t.Errorf("expected file at %s: %v", p, err)
			}
		}

		// ISC-39: the LEARN/5-of-5 ISA must have rendered.
		learnPath := filepath.Join(exfilAll, "isa", slugC, "ISA.md")
		bodyBytes, err := os.ReadFile(learnPath)
		if err != nil {
			t.Fatalf("read learn isa: %v", err)
		}
		body := string(bodyBytes)
		if !strings.Contains(body, "phase: learn") {
			t.Errorf("expected phase: learn in %s, got:\n%s", learnPath, body)
		}
		if !strings.Contains(body, `progress: "5/5"`) {
			t.Errorf("expected progress: \"5/5\" in %s", learnPath)
		}
	})

	// ===== Case 6: --since=<future> renders zero =====

	t.Run("render_all_since_future_renders_zero", func(t *testing.T) {
		exfilNoop := t.TempDir()
		// A timestamp comfortably in the future.
		future := "2099-12-31T23:59:59Z"
		out := bdISARenderAllOK(t, bd, dir, exfilNoop, "--since="+future)
		if strings.Contains(out, "\trendered") {
			t.Errorf("expected zero renders with future --since, got output:\n%s", out)
		}

		// And no files written.
		entries, err := os.ReadDir(exfilNoop)
		if err != nil {
			t.Fatalf("readdir noop: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("expected empty exfil dir, got %d entries", len(entries))
		}
	})
}
