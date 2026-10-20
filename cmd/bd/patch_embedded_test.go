//go:build cgo

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
)

// bdPatchFail runs `bd patch` expecting non-zero exit. Returns combined
// output. Asserts the exit-code matches the documented exit-2 contract for
// validation failures when wantExitCode is non-zero.
func bdPatchFail(t *testing.T, bd, dir string, wantExitCode int, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"patch"}, args...)
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd patch %s to fail, succeeded:\n%s", strings.Join(args, " "), out)
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

// bdPatchOK runs `bd patch` expecting success. Retries on flock contention so
// concurrent test runs do not produce false failures.
func bdPatchOK(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"patch"}, args...)
	out, err := bdRunWithFlockRetry(t, bd, dir, fullArgs...)
	if err != nil {
		t.Fatalf("bd patch %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// withTestDB runs fn against an open-on-demand sql.DB. The DB is opened, the
// callback runs, then the DB is closed before the function returns. This
// matches the querySessionSQL pattern in close_embedded_test.go and is
// REQUIRED: holding the embedded Dolt DB open across a bd subprocess
// invocation deadlocks the subprocess on the underlying lock.
func withTestDB(t *testing.T, beadsDir string, fn func(*sql.DB)) {
	t.Helper()
	dataDir := filepath.Join(beadsDir, "embeddeddolt")
	cfg, _ := configfile.Load(beadsDir)
	database := ""
	if cfg != nil {
		database = cfg.GetDoltDatabase()
	}
	db, cleanup, err := embeddeddolt.OpenSQL(t.Context(), dataDir, database, "main")
	if err != nil {
		t.Fatalf("OpenSQL: %v", err)
	}
	defer func() {
		if cerr := cleanup(); cerr != nil {
			t.Logf("OpenSQL cleanup: %v", cerr)
		}
	}()
	fn(db)
}

// insertISARow inserts a synthetic ISA-kind row via raw SQL because
// `bd brain new --kind=isa` is not wired yet (F1d scope).
func insertISARow(t *testing.T, db *sql.DB, id, title string, progressN int) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO issues (id, title, status, priority, issue_type, isa_progress_n)
		 VALUES (?, ?, 'open', 1, 'isa', ?)`,
		id, title, progressN,
	)
	if err != nil {
		t.Fatalf("INSERT isa row %s: %v", id, err)
	}
}

// insertNonISARow inserts a knowledge-kind row to exercise the wrong-kind
// rejection path.
func insertNonISARow(t *testing.T, db *sql.DB, id, title string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO issues (id, title, status, priority, issue_type)
		 VALUES (?, ?, 'open', 1, 'knowledge')`,
		id, title,
	)
	if err != nil {
		t.Fatalf("INSERT knowledge row %s: %v", id, err)
	}
}

// readISAColumns reads phase + updated_at for verification. phase comes back
// as a Go string; NULL surfaces as "".
func readISAColumns(t *testing.T, db *sql.DB, id string) (string, sql.NullTime) {
	t.Helper()
	var (
		phase     sql.NullString
		updatedAt sql.NullTime
	)
	err := db.QueryRowContext(t.Context(),
		"SELECT isa_phase, isa_updated_at FROM issues WHERE id = ?", id,
	).Scan(&phase, &updatedAt)
	if err != nil {
		t.Fatalf("read isa columns for %s: %v", id, err)
	}
	return phase.String, updatedAt
}

// readStatusAndISAUpdatedAt reads status + isa_updated_at for the non-ISA
// passthrough case (must update status; must NOT touch isa_updated_at).
func readStatusAndISAUpdatedAt(t *testing.T, db *sql.DB, id string) (string, sql.NullTime) {
	t.Helper()
	var (
		status    string
		updatedAt sql.NullTime
	)
	err := db.QueryRowContext(t.Context(),
		"SELECT status, isa_updated_at FROM issues WHERE id = ?", id,
	).Scan(&status, &updatedAt)
	if err != nil {
		t.Fatalf("read status/isa_updated_at for %s: %v", id, err)
	}
	return status, updatedAt
}

// TestPatch is the integration battery for `bd patch`. It exercises the
// five cases from the F1b verification matrix:
//
//  1. Valid ISA-field patch updates the value and bumps isa_updated_at.
//  2. Invalid enum value exits 2 with stderr listing the valid set.
//  3. isa_progress_m > current isa_progress_n exits 2.
//  4. ISA-field patch on a non-isa row exits 2 with "only valid for kind=isa".
//  5. Non-ISA field (status=closed) on a regular task succeeds via UpdateIssue
//     path; isa_updated_at remains NULL.
//
// Each subtest opens the embedded Dolt DB only around SQL work (INSERTs and
// SELECTs), closing it before any bd-subprocess call. Holding the DB open
// across a subprocess deadlocks the subprocess on the embedded-Dolt file
// lock.
func TestPatch(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "pt")

	// ===== Case 1: valid ISA phase patch updates value + bumps updated_at =====

	t.Run("valid_isa_phase_updates_value_and_bumps_updated_at", func(t *testing.T) {
		const id = "pt-isa-001"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "ISA phase happy path", 10)

			_, beforeUpdated := readISAColumns(t, db, id)
			if beforeUpdated.Valid {
				t.Fatalf("expected isa_updated_at to start NULL, got %v", beforeUpdated.Time)
			}
		})

		// Sleep so NOW() can advance past row-insert; MySQL/Dolt NOW()
		// granularity is only guaranteed to the second across versions.
		time.Sleep(1100 * time.Millisecond)

		out := bdPatchOK(t, bd, dir, id, "--field", "isa_phase", "--value", "BUILD")
		if !strings.Contains(out, "BUILD") {
			t.Errorf("expected success output to mention BUILD, got: %s", out)
		}

		withTestDB(t, beadsDir, func(db *sql.DB) {
			phase, afterUpdated := readISAColumns(t, db, id)
			if phase != "BUILD" {
				t.Errorf("expected phase=BUILD after patch, got %q", phase)
			}
			if !afterUpdated.Valid {
				t.Fatal("expected isa_updated_at to be set after patch, got NULL")
			}
			if afterUpdated.Time.IsZero() {
				t.Fatal("expected isa_updated_at to be a real timestamp, got zero")
			}
		})
	})

	// ===== Case 2: invalid enum value exits 2 + lists valid set =====

	t.Run("invalid_phase_exits_2_with_valid_set", func(t *testing.T) {
		const id = "pt-isa-002"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "ISA invalid enum", 5)
		})

		out := bdPatchFail(t, bd, dir, 2, id, "--field", "isa_phase", "--value", "NONSENSE")
		if !strings.Contains(out, "NONSENSE") {
			t.Errorf("expected stderr to mention rejected value NONSENSE, got: %s", out)
		}
		if !strings.Contains(out, "BUILD") || !strings.Contains(out, "OBSERVE") {
			t.Errorf("expected stderr to list valid phases (BUILD, OBSERVE), got: %s", out)
		}

		withTestDB(t, beadsDir, func(db *sql.DB) {
			phase, updated := readISAColumns(t, db, id)
			if phase != "" {
				t.Errorf("expected phase unchanged (empty), got %q", phase)
			}
			if updated.Valid {
				t.Errorf("expected isa_updated_at unchanged (NULL) on failed patch, got %v", updated.Time)
			}
		})
	})

	// ===== Case 3: progress_m > current progress_n exits 2 =====

	t.Run("progress_m_over_n_exits_2", func(t *testing.T) {
		const id = "pt-isa-003"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertISARow(t, db, id, "ISA progress invariant", 10)
		})

		out := bdPatchFail(t, bd, dir, 2, id, "--field", "isa_progress_m", "--value", "999")
		if !strings.Contains(out, "isa_progress_m") {
			t.Errorf("expected stderr to mention isa_progress_m, got: %s", out)
		}
		if !strings.Contains(out, "isa_progress_n") {
			t.Errorf("expected stderr to mention the conflicting isa_progress_n, got: %s", out)
		}
	})

	// ===== Case 4: ISA field on non-isa kind exits 2 with "only valid for kind=isa" =====

	t.Run("isa_field_on_non_isa_kind_exits_2", func(t *testing.T) {
		const id = "pt-know-004"

		withTestDB(t, beadsDir, func(db *sql.DB) {
			insertNonISARow(t, db, id, "Knowledge row, not ISA")
		})

		out := bdPatchFail(t, bd, dir, 2, id, "--field", "isa_phase", "--value", "BUILD")
		if !strings.Contains(out, "only valid for kind=isa") {
			t.Errorf("expected stderr to contain 'only valid for kind=isa', got: %s", out)
		}

		withTestDB(t, beadsDir, func(db *sql.DB) {
			phase, updated := readISAColumns(t, db, id)
			if phase != "" {
				t.Errorf("expected phase unchanged on wrong-kind patch, got %q", phase)
			}
			if updated.Valid {
				t.Errorf("expected isa_updated_at unchanged (NULL) on wrong-kind patch, got %v", updated.Time)
			}
		})
	})

	// ===== Case 5: non-ISA field on a regular task; isa_updated_at stays NULL =====

	t.Run("non_isa_field_passthrough_does_not_touch_isa_updated_at", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Regular task for passthrough", "--type", "task")

		withTestDB(t, beadsDir, func(db *sql.DB) {
			_, beforeUpdated := readStatusAndISAUpdatedAt(t, db, issue.ID)
			if beforeUpdated.Valid {
				t.Fatalf("expected isa_updated_at to start NULL on task, got %v", beforeUpdated.Time)
			}
		})

		bdPatchOK(t, bd, dir, issue.ID, "--field", "status", "--value", "closed")

		withTestDB(t, beadsDir, func(db *sql.DB) {
			status, afterUpdated := readStatusAndISAUpdatedAt(t, db, issue.ID)
			if status != "closed" {
				t.Errorf("expected status=closed after patch, got %q", status)
			}
			if afterUpdated.Valid {
				t.Errorf("expected isa_updated_at to remain NULL after non-ISA patch, got %v", afterUpdated.Time)
			}
		})
	})

	// description and body share storage. A --body-file write followed by
	// patch --field=description must refuse and leave the body intact.
	t.Run("description_patch_refuses_to_replace_body_file", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Body clobber guard", "--type", "task")
		const marker = "The rest of the body must survive a description patch."
		body := "Opening relevance.\n\n" + marker + "\n"
		bodyFile := filepath.Join(t.TempDir(), "body.md")
		if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil {
			t.Fatalf("write body file: %v", err)
		}
		bdUpdate(t, bd, dir, issue.ID, "--body-file", bodyFile)

		const shortLine = "short relevance line"
		out := bdPatchFail(t, bd, dir, 2, issue.ID, "--field", "description", "--value", shortLine)
		for _, want := range []string{"opening sentence", "body-file", "share storage"} {
			if !strings.Contains(out, want) {
				t.Errorf("refusal output missing %q:\n%s", want, out)
			}
		}

		got := bdShow(t, bd, dir, issue.ID)
		if !strings.Contains(got.Description, marker) {
			t.Errorf("body was clobbered: got %q", got.Description)
		}
		if strings.TrimSpace(got.Description) == shortLine {
			t.Errorf("description patch replaced the body with %q", shortLine)
		}
	})

	// An empty body is not a replace. The description patch may set it.
	t.Run("description_patch_sets_empty_body", func(t *testing.T) {
		issue := bdCreate(t, bd, dir, "Empty body description patch", "--type", "task")
		before := bdShow(t, bd, dir, issue.ID)
		if strings.TrimSpace(before.Description) != "" {
			t.Fatalf("expected a new issue to start with an empty body, got %q", before.Description)
		}

		const line = "first description line"
		bdPatchOK(t, bd, dir, issue.ID, "--field", "description", "--value", line)
		got := bdShow(t, bd, dir, issue.ID)
		if got.Description != line {
			t.Errorf("description = %q, want %q", got.Description, line)
		}
	})

	// Documented two-step mint: create (which persists a title-derived
	// metadata.brain_slug), then patch the slug column. Both fields must
	// hold the patched slug, and sibling metadata keys must survive.
	//
	// The slug write is one raw UPDATE. The embedded store does not
	// implement RawDBAccessor, and brain runs on dolt sql-server, so this
	// case uses its own server instead of the embedded workspace above.
	t.Run("two_step_mint_slug_patch_syncs_brain_slug", func(t *testing.T) {
		const want = "canonical-ship-slug"
		port := startPatchDoltServer(t)
		srvDir := t.TempDir()
		initGitRepoAt(t, srvDir)
		runBDInit(t, bd, srvDir,
			"--skip-hooks", "--skip-agents",
			"--server", "--external",
			"--server-host", "127.0.0.1",
			"--server-port", fmt.Sprintf("%d", port),
			"--server-user", "root",
			"--database", "slugpatch",
			"--prefix", "sp",
		)
		issue := bdCreate(t, bd, srvDir, "Ship The Thing", "--type", "isa")
		db := openPatchServerDB(t, port, "slugpatch")

		_, brainSlug, meta := readSlugAndMetadata(t, db, issue.ID)
		if brainSlug == "" {
			brainSlug = "ship-the-thing"
		}
		if brainSlug == want {
			t.Fatalf("mint stored %q before the slug patch; the case would not show divergence", want)
		}
		meta["brain_slug"] = brainSlug
		meta["keep"] = "yes"
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("marshal seeded metadata: %v", err)
		}
		if _, err := db.ExecContext(t.Context(),
			"UPDATE issues SET metadata = ? WHERE id = ?",
			string(raw), issue.ID,
		); err != nil {
			t.Fatalf("seed metadata: %v", err)
		}
		_, beforeUpdated := readISAColumns(t, db, issue.ID)

		bdPatchOK(t, bd, srvDir, issue.ID, "--field", "slug", "--value", want)

		slug, gotBrain, gotMeta := readSlugAndMetadata(t, db, issue.ID)
		if slug != want {
			t.Errorf("slug column = %q, want %q", slug, want)
		}
		if gotBrain != want {
			t.Errorf("metadata.brain_slug = %q, want %q", gotBrain, want)
		}
		if slug != gotBrain {
			t.Errorf("slug column %q and metadata.brain_slug %q diverged", slug, gotBrain)
		}
		if keep, _ := gotMeta["keep"].(string); keep != "yes" {
			t.Errorf("sibling metadata key keep = %#v, want %q", gotMeta["keep"], "yes")
		}
		_, afterUpdated := readISAColumns(t, db, issue.ID)
		if beforeUpdated.Valid != afterUpdated.Valid || (beforeUpdated.Valid && !beforeUpdated.Time.Equal(afterUpdated.Time)) {
			t.Errorf("slug patch changed isa_updated_at from %v to %v", beforeUpdated, afterUpdated)
		}
	})
}

// startPatchDoltServer starts a scratch dolt sql-server and returns its port.
// The process is stopped when the test ends. Skips when the dolt binary is absent.
func startPatchDoltServer(t *testing.T) int {
	t.Helper()
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt binary not on PATH")
	}
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := listen.Addr().(*net.TCPAddr).Port
	if err := listen.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := doltServerEnv(root)
	initCmd := exec.Command("dolt", "init", "--name", "slug-patch-test", "--email", "slug-patch@test.invalid")
	initCmd.Dir, initCmd.Env = dataDir, env
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt init: %v\n%s", err, out)
	}
	srv := exec.Command("dolt", "sql-server", "--host", "127.0.0.1", fmt.Sprintf("--port=%d", port), "--loglevel=error")
	srv.Dir, srv.Env = dataDir, env
	if err := srv.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		if srv.Process != nil {
			_ = srv.Process.Signal(syscall.SIGTERM)
			_ = srv.Wait()
		}
	})

	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := sql.Open("mysql", dsn)
		if err == nil {
			var one int
			qerr := c.QueryRow("SELECT 1").Scan(&one)
			_ = c.Close()
			if qerr == nil && one == 1 {
				return port
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("dolt sql-server not ready in 30s")
	return 0
}

func doltServerEnv(root string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "DOLT_ROOT_PATH=") || strings.HasPrefix(e, "BEADS_") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "DOLT_ROOT_PATH="+root)
}

func openPatchServerDB(t *testing.T, port int, database string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s?parseTime=true", port, database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open server db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("ping server db: %v", err)
	}
	return db
}

// readSlugAndMetadata returns the slug column, metadata.brain_slug, and the
// decoded metadata object. A missing or null metadata blob decodes as an
// empty object and an empty brain slug.
func readSlugAndMetadata(t *testing.T, db *sql.DB, id string) (string, string, map[string]any) {
	t.Helper()
	var (
		slug sql.NullString
		meta []byte
	)
	err := db.QueryRowContext(t.Context(),
		"SELECT slug, CAST(metadata AS CHAR) FROM issues WHERE id = ?", id,
	).Scan(&slug, &meta)
	if err != nil {
		t.Fatalf("read slug/metadata for %s: %v", id, err)
	}
	decoded := map[string]any{}
	if len(meta) > 0 && string(meta) != "null" {
		if err := json.Unmarshal(meta, &decoded); err != nil {
			t.Fatalf("decode metadata for %s: %v\nraw: %s", id, err, meta)
		}
	}
	brainSlug, _ := decoded["brain_slug"].(string)
	return slug.String, brainSlug, decoded
}
