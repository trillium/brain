//go:build cgo

package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bdISASectionFail runs `bd isa-section` expecting non-zero exit. Asserts the
// observed exit code matches wantExitCode when non-zero. Returns the combined
// output for assertion-on-message tests.
func bdISASectionFail(t *testing.T, bd, dir string, wantExitCode int, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-section"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd isa-section %s to fail, succeeded:\n%s",
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

// bdISASectionOK runs `bd isa-section` expecting success, with flock retry.
func bdISASectionOK(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-section"}, args...)
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd isa-section %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return string(out)
}

// readISASectionRow reads (body, updated_at) for an (issue_id, section_name)
// pair. Returns sql.ErrNoRows if no row exists.
func readISASectionRow(t *testing.T, db *sql.DB, issueID, section string) (string, sql.NullTime, error) {
	t.Helper()
	var (
		body      string
		updatedAt sql.NullTime
	)
	err := db.QueryRowContext(t.Context(),
		`SELECT body, updated_at FROM isa_sections
		 WHERE issue_id = ? AND section_name = ?`,
		issueID, section,
	).Scan(&body, &updatedAt)
	return body, updatedAt, err
}

// countISASectionsFor returns the count of rows in isa_sections for the
// given issue_id. Used to assert "no DB writes" on failure paths.
func countISASectionsFor(t *testing.T, db *sql.DB, issueID string) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM isa_sections WHERE issue_id = ?", issueID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count isa_sections for %s: %v", issueID, err)
	}
	return n
}

// readISAUpdatedAt reads issues.isa_updated_at for a row. Helper for
// asserting the auto-touch happened.
func readISAUpdatedAt(t *testing.T, db *sql.DB, id string) sql.NullTime {
	t.Helper()
	var updatedAt sql.NullTime
	err := db.QueryRowContext(t.Context(),
		"SELECT isa_updated_at FROM issues WHERE id = ?", id,
	).Scan(&updatedAt)
	if err != nil {
		t.Fatalf("read isa_updated_at for %s: %v", id, err)
	}
	return updatedAt
}

// writeTempSectionFile writes body to a fresh tempfile and returns the path.
// Used to drive --value-from-file from the test.
func writeTempSectionFile(t *testing.T, body string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "isa-section-*.md")
	if err != nil {
		t.Fatalf("create tempfile: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatalf("write tempfile: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tempfile: %v", err)
	}
	return f.Name()
}

// touchAbs returns the absolute path of a path under the given dir, useful
// when passing files to a bd subprocess that runs with Dir set to dir.
func absInTmp(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs %s: %v", p, err)
	}
	return abs
}

// TestISASection covers the F1c-1 verification matrix:
//
//  1. --value-from-file upserts a new section on a kind=isa row AND bumps
//     issues.isa_updated_at.
//  2. Re-running with new content on the same (id, section) overwrites body
//     and bumps updated_at (UPSERT path).
//  3. Unknown section name -> exit 2, no isa_sections row.
//  4. isa-section on a knowledge-kind row -> exit 2 with the documented
//     error string, no isa_sections row.
//  5. Both --value-from-file and --value-stdin supplied -> exit 2 with the
//     "mutually exclusive" message.
func TestISASection(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "isec")

	// ===== Case 1: --value-from-file upserts new section + bumps isa_updated_at =====

	t.Run("from_file_upserts_new_section_and_bumps_updated_at", func(t *testing.T) {
		const id = "isec-isa-001"
		const section = "problem"
		const body = "Current state: substrate is forming.\nIdeal state: substrate is real.\n"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "ISA upsert happy path", 10)
			if got := countISASectionsFor(t, db, id); got != 0 {
				t.Fatalf("expected 0 isa_sections rows pre-upsert, got %d", got)
			}
			if before := readISAUpdatedAt(t, db, id); before.Valid {
				t.Fatalf("expected isa_updated_at to start NULL, got %v", before.Time)
			}
		})

		bodyFile := absInTmp(t, writeTempSectionFile(t, body))

		// Sleep so NOW() inside the verb strictly advances past row insert.
		// Dolt/MySQL NOW() granularity is second-level on some versions.
		time.Sleep(1100 * time.Millisecond)

		out := bdISASectionOK(t, bd, dir, id, section, "--value-from-file", bodyFile)
		if !strings.Contains(out, section) {
			t.Errorf("expected success output to mention section %q, got: %s", section, out)
		}

		withTestDB(t, beadsDir, func(db *sql.DB) {
			gotBody, gotUpdated, err := readISASectionRow(t, db, id, section)
			if err != nil {
				t.Fatalf("expected isa_sections row to exist post-upsert: %v", err)
			}
			if gotBody != body {
				t.Errorf("expected body %q, got %q", body, gotBody)
			}
			if !gotUpdated.Valid || gotUpdated.Time.IsZero() {
				t.Fatal("expected isa_sections.updated_at to be a real timestamp")
			}

			issueUpdated := readISAUpdatedAt(t, db, id)
			if !issueUpdated.Valid {
				t.Fatal("expected issues.isa_updated_at to be set after isa-section, got NULL")
			}
			if issueUpdated.Time.IsZero() {
				t.Fatal("expected issues.isa_updated_at to be a real timestamp, got zero")
			}
		})
	})

	// ===== Case 2: UPSERT path — second write overwrites body and bumps updated_at =====

	t.Run("rewriting_same_section_overwrites_body_and_bumps_updated_at", func(t *testing.T) {
		const id = "isec-isa-002"
		const section = "changelog"
		const bodyA = "## v0.1\n- initial draft\n"
		const bodyB = "## v0.2\n- second pass, more detail\n- two entries now\n"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "ISA upsert overwrite", 5)
		})

		fileA := absInTmp(t, writeTempSectionFile(t, bodyA))
		bdISASectionOK(t, bd, dir, id, section, "--value-from-file", fileA)

		var firstUpdated sql.NullTime
		withTestDB(t, beadsDir, func(db *sql.DB) {
			gotBody, gotUpdated, err := readISASectionRow(t, db, id, section)
			if err != nil {
				t.Fatalf("expected isa_sections row after first write: %v", err)
			}
			if gotBody != bodyA {
				t.Errorf("expected first body %q, got %q", bodyA, gotBody)
			}
			if !gotUpdated.Valid {
				t.Fatal("expected updated_at to be set after first write")
			}
			firstUpdated = gotUpdated
			if got := countISASectionsFor(t, db, id); got != 1 {
				t.Errorf("expected 1 isa_sections row after first write, got %d", got)
			}
		})

		// Wait so NOW() strictly advances; otherwise the timestamp comparison
		// below would be a coin flip on second-granularity backends.
		time.Sleep(1100 * time.Millisecond)

		fileB := absInTmp(t, writeTempSectionFile(t, bodyB))
		bdISASectionOK(t, bd, dir, id, section, "--value-from-file", fileB)

		withTestDB(t, beadsDir, func(db *sql.DB) {
			gotBody, gotUpdated, err := readISASectionRow(t, db, id, section)
			if err != nil {
				t.Fatalf("expected isa_sections row after second write: %v", err)
			}
			if gotBody != bodyB {
				t.Errorf("expected second body %q, got %q", bodyB, gotBody)
			}
			if !gotUpdated.Valid {
				t.Fatal("expected updated_at to be set after second write")
			}
			if !gotUpdated.Time.After(firstUpdated.Time) {
				t.Errorf("expected updated_at to advance: first=%v second=%v",
					firstUpdated.Time, gotUpdated.Time)
			}
			if got := countISASectionsFor(t, db, id); got != 1 {
				t.Errorf("expected still 1 isa_sections row after UPSERT, got %d", got)
			}
		})
	})

	// ===== Case 3: unknown section name exits 2 with no DB writes =====

	t.Run("unknown_section_name_exits_2", func(t *testing.T) {
		const id = "isec-isa-003"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "ISA bad section name", 0)
		})

		bodyFile := absInTmp(t, writeTempSectionFile(t, "x"))
		out := bdISASectionFail(t, bd, dir, 2,
			id, "not_a_section", "--value-from-file", bodyFile)
		if !strings.Contains(out, "not_a_section") {
			t.Errorf("expected stderr to mention rejected name, got: %s", out)
		}
		// Sanity: the error message should enumerate valid names; check two
		// canonical names so a future drift in the list is caught.
		if !strings.Contains(out, "problem") || !strings.Contains(out, "changelog") {
			t.Errorf("expected stderr to list valid sections, got: %s", out)
		}

		withTestDB(t, beadsDir, func(db *sql.DB) {
			if got := countISASectionsFor(t, db, id); got != 0 {
				t.Errorf("expected 0 isa_sections rows after rejected write, got %d", got)
			}
		})
	})

	// ===== Case 4: isa-section on a knowledge-kind row exits 2 =====

	t.Run("wrong_kind_exits_2_no_writes", func(t *testing.T) {
		const id = "isec-know-004"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertNonISARow(t, db, id, "Knowledge row, not ISA")
		})

		bodyFile := absInTmp(t, writeTempSectionFile(t, "anything"))
		out := bdISASectionFail(t, bd, dir, 2,
			id, "problem", "--value-from-file", bodyFile)
		if !strings.Contains(out, "kind=isa") {
			t.Errorf("expected stderr to contain 'kind=isa', got: %s", out)
		}
		if !strings.Contains(out, "issue_type=knowledge") {
			t.Errorf("expected stderr to report actual issue_type, got: %s", out)
		}

		withTestDB(t, beadsDir, func(db *sql.DB) {
			if got := countISASectionsFor(t, db, id); got != 0 {
				t.Errorf("expected no isa_sections rows after wrong-kind write, got %d", got)
			}
		})
	})

	// ===== Case 5: both --value-from-file and --value-stdin -> exit 2 =====

	t.Run("both_value_sources_exits_2", func(t *testing.T) {
		const id = "isec-isa-005"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "ISA both sources rejected", 0)
		})

		bodyFile := absInTmp(t, writeTempSectionFile(t, "x"))
		out := bdISASectionFail(t, bd, dir, 2,
			id, "problem",
			"--value-from-file", bodyFile,
			"--value-stdin",
		)
		if !strings.Contains(out, "mutually exclusive") {
			t.Errorf("expected stderr to contain 'mutually exclusive', got: %s", out)
		}

		withTestDB(t, beadsDir, func(db *sql.DB) {
			if got := countISASectionsFor(t, db, id); got != 0 {
				t.Errorf("expected no isa_sections rows after rejected dual-source, got %d", got)
			}
		})
	})
}
