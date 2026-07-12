//go:build cgo

package main

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

// setDescriptionSQL writes a description body directly via SQL, bypassing the
// bd surface. Used to stage a large existing body before exercising the
// body-clobber guard.
func setDescriptionSQL(t *testing.T, db *sql.DB, id, body string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		"UPDATE issues SET description = ? WHERE id = ?", body, id)
	if err != nil {
		t.Fatalf("UPDATE description for %s: %v", id, err)
	}
}

// readDescriptionSQL reads back the current description body. NULL → "".
func readDescriptionSQL(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var body sql.NullString
	err := db.QueryRowContext(t.Context(),
		"SELECT description FROM issues WHERE id = ?", id).Scan(&body)
	if err != nil {
		t.Fatalf("read description for %s: %v", id, err)
	}
	return body.String
}

// TestPatchDescriptionClobberGuard covers the body-clobber guard on
// `bd patch --field=description`. In brain v0.3 the description column IS the
// full body, so replacing a large body with a short tagline is almost always
// an accident (the agent-bkm incident, 2026-07-12). The guard refuses that
// unless --overwrite-body is passed.
func TestPatchDescriptionClobberGuard(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "dg")

	// A body comfortably over the 200-byte threshold.
	longBody := strings.Repeat(
		"This is a substantial knowledge body that must not be clobbered. ", 20)
	if len(longBody) < 200 {
		t.Fatalf("test setup: longBody is only %d bytes, need >= 200", len(longBody))
	}

	// ===== Guard fires: large body, short replacement, no bypass =====

	t.Run("large_body_short_value_is_blocked", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Guarded knowledge entry", "--type", "task")

		withTestDB(t, beadsDir, func(db *sql.DB) {
			setDescriptionSQL(t, db, issue.ID, longBody)
		})

		out := bdPatchFail(t, bd, dir, 1, issue.ID,
			"--field", "description", "--value", "short relevance line")

		if !strings.Contains(out, "would replace") {
			t.Errorf("expected guard message to mention 'would replace', got: %s", out)
		}
		if !strings.Contains(out, "--overwrite-body") {
			t.Errorf("expected guard message to mention '--overwrite-body', got: %s", out)
		}
		if !strings.Contains(out, "update") {
			t.Errorf("expected guard message to point at `brain update --stdin`, got: %s", out)
		}

		// The blocked patch must NOT have touched the body.
		withTestDB(t, beadsDir, func(db *sql.DB) {
			if got := readDescriptionSQL(t, db, issue.ID); got != longBody {
				t.Errorf("expected body unchanged after blocked patch, got %d bytes: %q",
					len(got), got)
			}
		})
	})

	// ===== Bypass: --overwrite-body lets the shrink through =====

	t.Run("overwrite_body_bypasses_guard", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Intentional rewrite entry", "--type", "task")

		withTestDB(t, beadsDir, func(db *sql.DB) {
			setDescriptionSQL(t, db, issue.ID, longBody)
		})

		bdPatchOK(t, bd, dir, issue.ID,
			"--field", "description", "--value", "short", "--overwrite-body")

		withTestDB(t, beadsDir, func(db *sql.DB) {
			if got := readDescriptionSQL(t, db, issue.ID); got != "short" {
				t.Errorf("expected body='short' after --overwrite-body patch, got: %q", got)
			}
		})
	})

	// ===== Conservative: small existing body never trips the guard =====

	t.Run("small_body_is_not_guarded", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Tiny entry", "--type", "task")

		// A short body (< 200 bytes): fixing a typo down to a shorter string
		// must succeed without --overwrite-body.
		withTestDB(t, beadsDir, func(db *sql.DB) {
			setDescriptionSQL(t, db, issue.ID, "a short note with a typpo in it")
		})

		bdPatchOK(t, bd, dir, issue.ID,
			"--field", "description", "--value", "fixed")

		withTestDB(t, beadsDir, func(db *sql.DB) {
			if got := readDescriptionSQL(t, db, issue.ID); got != "fixed" {
				t.Errorf("expected body='fixed' after small-body patch, got: %q", got)
			}
		})
	})
}
