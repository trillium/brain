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

// bdISAListFail runs `bd isa-list` expecting non-zero exit.
func bdISAListFail(t *testing.T, bd, dir string, wantExitCode int, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-list"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd isa-list %s to fail, succeeded:\n%s",
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

// bdISAListOK runs `bd isa-list` expecting success, with flock retry.
func bdISAListOK(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-list"}, args...)
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd isa-list %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return string(out)
}

// insertISARowWithPhase inserts an ISA row with a specific phase set, so the
// --active filter can be exercised.
func insertISARowWithPhase(t *testing.T, db *sql.DB, id, title, phase string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO issues (id, title, status, priority, issue_type, isa_phase)
		 VALUES (?, ?, 'open', 1, 'isa', ?)`,
		id, title, phase,
	)
	if err != nil {
		t.Fatalf("INSERT isa row %s phase=%s: %v", id, phase, err)
	}
}

// TestISAList covers the F1c-2 verification matrix for the list verb:
//
//  1. Empty store -> exit 0, no output.
//  2. Three ISAs (OBSERVE, BUILD, LEARN) -> isa-list returns all 3.
//  3. --active filters out the LEARN row -> returns 2.
//  4. --json emits a JSON array of length 3.
//  5. Knowledge-kind rows are not listed (kind filter).
func TestISAList(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "ilist")

	t.Run("empty_store_exits_0_no_output", func(t *testing.T) {
		out := bdISAListOK(t, bd, dir)
		if strings.TrimSpace(out) != "" {
			t.Errorf("expected empty output, got: %q", out)
		}
	})

	// Populate three ISAs in distinct phases, plus one knowledge row that
	// must not appear in the list.
	withTestDB(t, beadsDir, func(db *sql.DB) {
		insertISARowWithPhase(t, db, "ilist-isa-observe", "ISA in OBSERVE", "OBSERVE")
		insertISARowWithPhase(t, db, "ilist-isa-build", "ISA in BUILD", "BUILD")
		insertISARowWithPhase(t, db, "ilist-isa-learn", "ISA in LEARN", "LEARN")
		insertNonISARow(t, db, "ilist-know-x", "Knowledge row (excluded)")
	})

	t.Run("lists_all_three_isas_excludes_knowledge", func(t *testing.T) {
		out := bdISAListOK(t, bd, dir)
		for _, id := range []string{"ilist-isa-observe", "ilist-isa-build", "ilist-isa-learn"} {
			if !strings.Contains(out, id) {
				t.Errorf("expected output to contain %s, got:\n%s", id, out)
			}
		}
		if strings.Contains(out, "ilist-know-x") {
			t.Errorf("expected knowledge row to be filtered out, got:\n%s", out)
		}
	})

	t.Run("active_filters_learn_phase", func(t *testing.T) {
		out := bdISAListOK(t, bd, dir, "--active")
		if !strings.Contains(out, "ilist-isa-observe") {
			t.Errorf("expected OBSERVE row to be in --active output, got:\n%s", out)
		}
		if !strings.Contains(out, "ilist-isa-build") {
			t.Errorf("expected BUILD row to be in --active output, got:\n%s", out)
		}
		if strings.Contains(out, "ilist-isa-learn") {
			t.Errorf("expected LEARN row to be filtered out by --active, got:\n%s", out)
		}
	})

	t.Run("json_emits_array_of_three", func(t *testing.T) {
		out := bdISAListOK(t, bd, dir, "--json")
		var rows []map[string]interface{}
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("expected valid JSON array, got error %v\noutput:\n%s", err, out)
		}
		if len(rows) != 3 {
			t.Errorf("expected 3 rows in JSON output, got %d:\n%s", len(rows), out)
		}
		// Spot-check the shape on the first row.
		first := rows[0]
		for _, key := range []string{"id", "slug", "isa_phase", "isa_progress", "isa_effort", "isa_updated_at", "title"} {
			if _, ok := first[key]; !ok {
				t.Errorf("expected JSON row to contain key %q, got: %v", key, first)
			}
		}
		prog, ok := first["isa_progress"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected isa_progress to be an object, got: %T", first["isa_progress"])
		}
		for _, key := range []string{"m", "n"} {
			if _, ok := prog[key]; !ok {
				t.Errorf("expected isa_progress.%s, got: %v", key, prog)
			}
		}
	})

	t.Run("json_active_filters_to_two", func(t *testing.T) {
		out := bdISAListOK(t, bd, dir, "--json", "--active")
		var rows []map[string]interface{}
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("JSON parse: %v\n%s", err, out)
		}
		if len(rows) != 2 {
			t.Errorf("expected 2 rows with --active, got %d:\n%s", len(rows), out)
		}
		for _, r := range rows {
			if r["isa_phase"] == "LEARN" {
				t.Errorf("LEARN row leaked into --active output: %v", r)
			}
		}
	})
}
