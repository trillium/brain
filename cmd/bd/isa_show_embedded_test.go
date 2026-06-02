//go:build cgo

package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// bdISAShowFail runs `bd isa-show` expecting non-zero exit. Asserts the
// exit-code matches wantExitCode when non-zero.
func bdISAShowFail(t *testing.T, bd, dir string, wantExitCode int, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-show"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd isa-show %s to fail, succeeded:\n%s",
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

// bdISAShowOK runs `bd isa-show` expecting success, with flock retry.
func bdISAShowOK(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-show"}, args...)
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd isa-show %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return string(out)
}

// upsertISASectionSQL writes a section body directly via SQL — avoids the
// bd subprocess overhead and keeps the test focused on the read verb.
func upsertISASectionSQL(t *testing.T, db *sql.DB, issueID, section, body string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO isa_sections (issue_id, section_name, body, updated_at)
		 VALUES (?, ?, ?, NOW())
		 ON DUPLICATE KEY UPDATE body = VALUES(body), updated_at = NOW()`,
		issueID, section, body,
	)
	if err != nil {
		t.Fatalf("upsert isa_sections (%s, %s): %v", issueID, section, err)
	}
}

// TestISAShow covers the F1c-2 verification matrix for the show verb:
//
//  1. Markdown output renders sections in spec order.
//  2. --json output parses, has the ISC-16 shape (isa_progress.m/.n, sections map).
//  3. --section=<name> emits just the body.
//  4. Bogus id -> exit 1 with "isa not found".
//  5. Non-isa kind -> exit 1 with "not an ISA".
//  6. --json --section combo logs the warning and emits the full doc.
func TestISAShow(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "ish")

	t.Run("markdown_renders_sections_in_spec_order", func(t *testing.T) {
		const id = "ish-isa-001"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "Markdown spec order", 12)
			// Insert in reverse-spec order to prove sorter doesn't depend
			// on insertion order.
			upsertISASectionSQL(t, db, id, "verification", "verify body")
			upsertISASectionSQL(t, db, id, "problem", "problem body")
			upsertISASectionSQL(t, db, id, "vision", "vision body")
		})

		out := bdISAShowOK(t, bd, dir, id)
		idxProblem := strings.Index(out, "## Problem")
		idxVision := strings.Index(out, "## Vision")
		idxVerify := strings.Index(out, "## Verification")
		if idxProblem < 0 || idxVision < 0 || idxVerify < 0 {
			t.Fatalf("expected all three section headings, got:\n%s", out)
		}
		if !(idxProblem < idxVision && idxVision < idxVerify) {
			t.Errorf("expected spec order Problem<Vision<Verification, positions %d, %d, %d\noutput:\n%s",
				idxProblem, idxVision, idxVerify, out)
		}
		if !strings.Contains(out, "problem body") || !strings.Contains(out, "verify body") {
			t.Errorf("expected section bodies in output:\n%s", out)
		}
	})

	t.Run("json_shape_isc16", func(t *testing.T) {
		const id = "ish-isa-002"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "JSON shape", 7)
			upsertISASectionSQL(t, db, id, "problem", "we lack a substrate")
			upsertISASectionSQL(t, db, id, "vision", "we have a substrate")
		})

		out := bdISAShowOK(t, bd, dir, id, "--json")
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("expected valid JSON, got error %v\noutput:\n%s", err, out)
		}
		for _, key := range []string{
			"id", "slug", "kind", "isa_phase", "isa_progress",
			"isa_effort", "isa_mode", "isa_started_at", "isa_updated_at",
			"sections",
		} {
			if _, ok := doc[key]; !ok {
				t.Errorf("expected JSON to contain key %q, got: %s", key, out)
			}
		}
		// isa_progress is the nested {m, n} object — not two top-level fields.
		prog, ok := doc["isa_progress"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected isa_progress to be an object, got: %T %v", doc["isa_progress"], doc["isa_progress"])
		}
		if _, ok := prog["m"]; !ok {
			t.Errorf("expected isa_progress.m, got: %v", prog)
		}
		if _, ok := prog["n"]; !ok {
			t.Errorf("expected isa_progress.n, got: %v", prog)
		}
		// sections is a map.
		sections, ok := doc["sections"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected sections to be an object, got: %T", doc["sections"])
		}
		if sections["problem"] != "we lack a substrate" {
			t.Errorf("expected sections.problem = 'we lack a substrate', got %v", sections["problem"])
		}
		// Kind must be 'isa' on every doc returned by this verb.
		if doc["kind"] != "isa" {
			t.Errorf("expected kind='isa', got %v", doc["kind"])
		}
	})

	t.Run("section_flag_returns_just_body", func(t *testing.T) {
		const id = "ish-isa-003"
		const body = "the problem statement, verbatim\n"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "Section filter", 0)
			upsertISASectionSQL(t, db, id, "problem", body)
		})

		out := bdISAShowOK(t, bd, dir, id, "--section=problem")
		if strings.Contains(out, "## Problem") {
			t.Errorf("expected raw body without heading, got:\n%s", out)
		}
		if !strings.Contains(out, "the problem statement, verbatim") {
			t.Errorf("expected body content in output, got:\n%s", out)
		}
	})

	t.Run("section_not_set_exits_1", func(t *testing.T) {
		const id = "ish-isa-004"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "No section set", 0)
		})
		out := bdISAShowFail(t, bd, dir, 1, id, "--section=goal")
		if !strings.Contains(out, "section") {
			t.Errorf("expected stderr to mention section, got: %s", out)
		}
	})

	t.Run("not_found_exits_1", func(t *testing.T) {
		out := bdISAShowFail(t, bd, dir, 1, "ish-isa-does-not-exist")
		if !strings.Contains(strings.ToLower(out), "not found") {
			t.Errorf("expected stderr to say 'not found', got: %s", out)
		}
	})

	t.Run("wrong_kind_exits_1", func(t *testing.T) {
		const id = "ish-know-006"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertNonISARow(t, db, id, "Not an ISA")
		})
		out := bdISAShowFail(t, bd, dir, 1, id)
		if !strings.Contains(out, "not an ISA") {
			t.Errorf("expected stderr to say 'not an ISA', got: %s", out)
		}
	})

	t.Run("json_overrides_section_with_warning", func(t *testing.T) {
		const id = "ish-isa-007"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "JSON+section precedence", 0)
			upsertISASectionSQL(t, db, id, "problem", "p body")
		})
		out := bdISAShowOK(t, bd, dir, id, "--json", "--section=problem")
		if !strings.Contains(out, "ignored when --json") {
			t.Errorf("expected stderr warning about --section being ignored, got: %s", out)
		}
		// Output still contains a full JSON doc (sections map, not just a body).
		if !strings.Contains(out, `"sections"`) {
			t.Errorf("expected full JSON doc with sections key, got: %s", out)
		}
	})
}
