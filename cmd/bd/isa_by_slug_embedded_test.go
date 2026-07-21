//go:build cgo

package main

import (
	"database/sql"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// bdISABySlugFail runs `bd isa-by-slug` expecting non-zero exit.
func bdISABySlugFail(t *testing.T, bd, dir string, wantExitCode int, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-by-slug"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd isa-by-slug %s to fail, succeeded:\n%s",
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

// bdISABySlugOK runs `bd isa-by-slug` expecting success, with flock retry.
func bdISABySlugOK(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"isa-by-slug"}, args...)
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd isa-by-slug %s failed: %v\n%s",
			strings.Join(args, " "), err, out)
	}
	return string(out)
}

// insertISARowWithSlug inserts an ISA row with a specific slug. F1d will
// auto-populate the slug; F1c-2 uses the column manually.
func insertISARowWithSlug(t *testing.T, db *sql.DB, id, title, slug string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO issues (id, title, status, priority, issue_type, slug)
		 VALUES (?, ?, 'open', 1, 'isa', ?)`,
		id, title, slug,
	)
	if err != nil {
		t.Fatalf("INSERT isa row %s slug=%s: %v", id, slug, err)
	}
}

// insertNonISARowWithSlug inserts a non-isa row carrying a slug — used to
// prove the kind filter on isa-by-slug.
func insertNonISARowWithSlug(t *testing.T, db *sql.DB, id, title, slug string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO issues (id, title, status, priority, issue_type, slug)
		 VALUES (?, ?, 'open', 1, 'knowledge', ?)`,
		id, title, slug,
	)
	if err != nil {
		t.Fatalf("INSERT knowledge row %s slug=%s: %v", id, slug, err)
	}
}

// TestISABySlug covers the F1c-2 verification matrix for the slug verb:
//
//  1. Known slug on an ISA row -> prints the id, exit 0.
//  2. Unknown slug -> exit 1, stderr "slug not found".
//  3. Slug on a knowledge-kind row -> exit 1 (kind filter).
func TestISABySlug(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "islug")

	t.Run("known_slug_returns_id", func(t *testing.T) {
		const id = "islug-isa-001"
		const slug = "my-isa-slug"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARowWithSlug(t, db, id, "ISA with slug", slug)
		})

		out := bdISABySlugOK(t, bd, dir, slug)
		got := strings.TrimSpace(out)
		if got != id {
			t.Errorf("expected stdout to be %q, got %q", id, got)
		}
	})

	t.Run("unknown_slug_exits_1", func(t *testing.T) {
		out := bdISABySlugFail(t, bd, dir, 1, "this-slug-does-not-exist")
		if !strings.Contains(out, "slug not found") {
			t.Errorf("expected stderr to contain 'slug not found', got: %s", out)
		}
	})

	t.Run("slug_on_non_isa_kind_exits_1", func(t *testing.T) {
		const id = "islug-know-003"
		const slug = "knowledge-only-slug"
		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertNonISARowWithSlug(t, db, id, "Knowledge with slug", slug)
		})
		out := bdISABySlugFail(t, bd, dir, 1, slug)
		if !strings.Contains(out, "slug not found") {
			t.Errorf("expected non-isa-kind slug to be treated as not found, got: %s", out)
		}
	})
}
