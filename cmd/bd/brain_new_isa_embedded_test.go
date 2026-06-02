//go:build cgo

package main

import (
	"database/sql"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestBrainNewISA is the F1d-2 integration battery against a real
// embedded-Dolt store. It traces the F1d verification matrix
// (ISC-1 .. ISC-5 in MEMORY/WORK/20260602-094500_brain-as-isa-substrate/ISA.md):
//
//	ISC-1: kind=isa is accepted by `bd brain new`.
//	ISC-2: kind=isa allocates IDs of shape <prefix>-isa-XXXXX.
//	ISC-3: --slug is honored when supplied, validated when invalid.
//	ISC-4: slug uniqueness is enforced at the DB layer.
//	ISC-5: when --slug is omitted, an auto-slug is generated from <title>.
//
// Pattern mirrors patch_embedded_test.go: gate on the BEADS_TEST_EMBEDDED_DOLT
// env var, build bd once via buildEmbeddedBD, run bdInit per test to get a
// fresh repo, and open the underlying Dolt DB only around SQL work (never
// across a subprocess call — that deadlocks on the embedded-Dolt file lock).
func TestBrainNewISA(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "pt")

	// ===== ISC-1 + ISC-2 + ISC-5: kind=isa with auto-slug, ID shape =====

	t.Run("isa_auto_slug_and_id_shape", func(t *testing.T) {
		out, err := bdRunWithFlockRetry(t, bd, dir, "brain", "new", "isa", "A New ISA")
		if err != nil {
			t.Fatalf("bd brain new isa failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "a-new-isa") {
			t.Errorf("expected stdout to mention auto-slug 'a-new-isa', got: %s", out)
		}

		var (
			id        string
			kind      string
			storedSlg sql.NullString
		)
		withTestDB(t, beadsDir, func(db *sql.DB) {
			row := db.QueryRowContext(t.Context(),
				"SELECT id, issue_type, slug FROM issues WHERE title = ? ORDER BY created_at DESC LIMIT 1",
				"A New ISA",
			)
			if err := row.Scan(&id, &kind, &storedSlg); err != nil {
				t.Fatalf("SELECT freshly-created ISA: %v", err)
			}
		})

		if kind != "isa" {
			t.Errorf("issue_type = %q, want %q", kind, "isa")
		}
		if !storedSlg.Valid || storedSlg.String != "a-new-isa" {
			t.Errorf("slug column = %v, want %q", storedSlg, "a-new-isa")
		}
		// ID shape: configured prefix is "pt"; kind=isa must yield "pt-isa-XXXXX".
		idShape := regexp.MustCompile(`^pt-isa-[A-Za-z0-9]+$`)
		if !idShape.MatchString(id) {
			t.Errorf("ID %q does not match %s — IDPrefix=isa was not routed", id, idShape)
		}
	})

	// ===== ISC-3: --slug honored when supplied =====

	t.Run("isa_explicit_slug_is_honored", func(t *testing.T) {
		out, err := bdRunWithFlockRetry(t, bd, dir,
			"brain", "new", "isa", "Custom", "--slug=my-custom-slug",
		)
		if err != nil {
			t.Fatalf("bd brain new isa --slug=my-custom-slug failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "my-custom-slug") {
			t.Errorf("expected stdout to mention 'my-custom-slug', got: %s", out)
		}

		var storedSlg sql.NullString
		withTestDB(t, beadsDir, func(db *sql.DB) {
			row := db.QueryRowContext(t.Context(),
				"SELECT slug FROM issues WHERE title = ? ORDER BY created_at DESC LIMIT 1",
				"Custom",
			)
			if err := row.Scan(&storedSlg); err != nil {
				t.Fatalf("SELECT slug: %v", err)
			}
		})
		if !storedSlg.Valid || storedSlg.String != "my-custom-slug" {
			t.Errorf("slug column = %v, want %q", storedSlg, "my-custom-slug")
		}
	})

	// ===== ISC-4: slug uniqueness enforced =====

	t.Run("duplicate_slug_exits_2", func(t *testing.T) {
		// First write succeeds.
		out, err := bdRunWithFlockRetry(t, bd, dir,
			"brain", "new", "isa", "Dup", "--slug=collision-slug",
		)
		if err != nil {
			t.Fatalf("first bd brain new with slug=collision-slug failed: %v\n%s", err, out)
		}

		// Second write with the same slug must exit 2 with a clear message.
		cmd := exec.Command(bd, "brain", "new", "isa", "Dup2", "--slug=collision-slug")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		combined, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("second bd brain new with duplicate slug succeeded; expected exit 2:\n%s", combined)
		}
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("expected ExitError, got %T: %v", err, err)
		}
		if got := ee.ExitCode(); got != 2 {
			t.Errorf("expected exit code 2 for slug collision, got %d.\noutput:\n%s", got, combined)
		}
		if !strings.Contains(string(combined), "slug already exists") {
			t.Errorf("expected stderr to mention 'slug already exists', got: %s", combined)
		}
	})

	// ===== ISC-3: invalid slug exits 2 with regex hint =====

	t.Run("invalid_slug_exits_2_with_regex_hint", func(t *testing.T) {
		cmd := exec.Command(bd, "brain", "new", "isa", "Bad", "--slug=BadSlug")
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		combined, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("bd brain new isa --slug=BadSlug succeeded; expected exit 2:\n%s", combined)
		}
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("expected ExitError, got %T: %v", err, err)
		}
		if got := ee.ExitCode(); got != 2 {
			t.Errorf("expected exit code 2 for invalid slug, got %d.\noutput:\n%s", got, combined)
		}
		// The slug helper's ValidationError surfaces the regex pattern;
		// assert on a recognizable subset of the regex so the test does
		// not couple to the full string.
		if !strings.Contains(string(combined), "[a-z0-9") {
			t.Errorf("expected stderr to mention slug regex, got: %s", combined)
		}
	})

	// ===== Backwards-compat sanity: kind=task still works =====

	t.Run("regular_task_still_works", func(t *testing.T) {
		out, err := bdRunWithFlockRetry(t, bd, dir, "brain", "new", "task", "Regular task")
		if err != nil {
			t.Fatalf("bd brain new task failed: %v\n%s", err, out)
		}

		var (
			kind      string
			storedSlg sql.NullString
			id        string
		)
		withTestDB(t, beadsDir, func(db *sql.DB) {
			row := db.QueryRowContext(t.Context(),
				"SELECT id, issue_type, slug FROM issues WHERE title = ? ORDER BY created_at DESC LIMIT 1",
				"Regular task",
			)
			if err := row.Scan(&id, &kind, &storedSlg); err != nil {
				t.Fatalf("SELECT regular task: %v", err)
			}
		})

		if kind != "task" {
			t.Errorf("issue_type = %q, want %q", kind, "task")
		}
		if storedSlg.Valid && storedSlg.String != "" {
			t.Errorf("expected slug to be NULL/empty on task without --slug, got %q", storedSlg.String)
		}
		// kind=task must NOT route through the isa IDPrefix path.
		if strings.Contains(id, "-isa-") {
			t.Errorf("task ID %q contains '-isa-' segment; IDPrefix routing leaked from kind=isa", id)
		}
	})
}
