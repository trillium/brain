package versioncontrolops

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	qMarker   = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
	qBackups  = "SELECT name, url FROM dolt_backups"
	qHead     = "SELECT DOLT_HASHOF('HEAD')"
	qDatabase = "SELECT DATABASE()"
	qGetLock  = "SELECT GET_LOCK(?, ?)"
	qRelease  = "SELECT RELEASE_LOCK(?)"
	qState    = "SELECT value FROM " + SharedBackupStateTable + " WHERE `key` = ?"
	qSave     = "REPLACE INTO " + SharedBackupStateTable + " (`key`, value) VALUES (?, ?)"
	qIgnore   = "REPLACE INTO dolt_ignore VALUES (?, true)"
	qCreate   = "CREATE TABLE IF NOT EXISTS " + SharedBackupStateTable
	qSync     = "CALL DOLT_BACKUP('sync', ?)"
	destURL   = "file:///backups/shared"
)

var t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func q(s string) string { return regexp.QuoteMeta(s) }

func expectCount(mock sqlmock.Sqlmock, n int) {
	mock.ExpectQuery(q(qMarker)).WithArgs(SharedBackupStateTable).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(n))
}

// expectNoState expects a load that finds no state table yet.
func expectNoState(mock sqlmock.Sqlmock) { expectCount(mock, 0) }

func expectState(mock sqlmock.Sqlmock, json string) {
	expectCount(mock, 1)
	mock.ExpectQuery(q(qState)).WithArgs(SharedBackupStateKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(json))
}

func expectBackups(mock sqlmock.Sqlmock, kv ...string) {
	rows := sqlmock.NewRows([]string{"name", "url"})
	for i := 0; i+1 < len(kv); i += 2 {
		rows.AddRow(kv[i], kv[i+1])
	}
	mock.ExpectQuery(q(qBackups)).WillReturnRows(rows)
}

func expectHead(mock sqlmock.Sqlmock, h string) {
	mock.ExpectQuery(q(qHead)).WillReturnRows(sqlmock.NewRows([]string{"h"}).AddRow(h))
}

func newMock(t *testing.T) (sqlmock.Sqlmock, func(opts SharedBackupOptions) (SharedBackupResult, error)) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mock, func(opts SharedBackupOptions) (SharedBackupResult, error) {
		if opts.Now == nil {
			opts.Now = func() time.Time { return t0 }
		}
		return RunSharedBackup(context.Background(), db, opts)
	}
}

func stateJSON(commit string, at time.Time) string {
	return `{"last_dolt_commit":"` + commit + `","timestamp":"` + at.Format(time.RFC3339) + `"}`
}

func TestIsSharedDatabase(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want bool
	}{{"marker present", 1, true}, {"marker absent", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			defer db.Close()
			mock.ExpectQuery(q(qMarker)).WithArgs(sharedMarkerTable).
				WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(tc.n))
			got, err := IsSharedDatabase(context.Background(), db)
			if err != nil || got != tc.want {
				t.Fatalf("IsSharedDatabase = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// An unanswerable probe is an error, never "not shared": the caller must skip
// the backup rather than take a per-store copy of a shared database.
func TestIsSharedDatabaseProbeErrorIsNotFoldedIntoFalse(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(q(qMarker)).WithArgs(sharedMarkerTable).WillReturnError(errors.New("server gone away"))
	got, err := IsSharedDatabase(context.Background(), db)
	if err == nil || got {
		t.Fatalf("IsSharedDatabase = %v, %v; want an error and false", got, err)
	}
}

func TestSharedBackupDestination(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    []string
		wantURL string
		refuse  string // substring of the refusal, "" for none
	}{
		{name: "default registered", rows: []string{"default", destURL}, wantURL: destURL},
		{name: "default wins over a stale backup_export", rows: []string{"backup_export", "file:///a/.beads/backup", "default", destURL}, wantURL: destURL},
		{name: "nothing registered", refuse: "bd backup init"},
		{name: "only backup_export registered", rows: []string{"backup_export", "file:///a/.beads/backup"}, refuse: "registered: backup_export"},
		{name: "two others, no default", rows: []string{"b", "file:///b", "a", "file:///a"}, refuse: "registered: a, b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			defer db.Close()
			expectBackups(mock, tc.rows...)
			got, err := SharedBackupDestination(context.Background(), db)
			if tc.refuse == "" {
				if err != nil || got != tc.wantURL {
					t.Fatalf("destination = %q, %v; want %q", got, err, tc.wantURL)
				}
				return
			}
			if !errors.Is(err, ErrSharedBackupRefused) || !strings.Contains(err.Error(), tc.refuse) {
				t.Fatalf("err = %v; want a refusal containing %q", err, tc.refuse)
			}
		})
	}
}

func TestSharedBackupDestinationUnreadableIsARefusal(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(q(qBackups)).WillReturnError(errors.New("table not found: dolt_backups"))
	if _, err := SharedBackupDestination(context.Background(), db); !errors.Is(err, ErrSharedBackupRefused) {
		t.Fatalf("err = %v; want a refusal", err)
	}
}

func TestRunSharedBackupThrottledTakesNoLockAndNoSync(t *testing.T) {
	mock, run := newMock(t)
	expectState(mock, stateJSON("c1", t0.Add(-5*time.Minute)))
	res, err := run(SharedBackupOptions{Interval: 15 * time.Minute})
	if err != nil || res.Outcome != SharedBackupThrottled {
		t.Fatalf("got %+v, %v; want throttled", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunSharedBackupUnchangedTakesNoLockAndNoSync(t *testing.T) {
	mock, run := newMock(t)
	expectState(mock, stateJSON("c1", t0.Add(-time.Hour)))
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c1")
	res, err := run(SharedBackupOptions{Interval: 15 * time.Minute})
	if err != nil || res.Outcome != SharedBackupUnchanged {
		t.Fatalf("got %+v, %v; want unchanged", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The first backup of a database: no state table yet. The table is registered
// in dolt_ignore and created before the sync, and the sync targets 'default'.
func TestRunSharedBackupFirstSync(t *testing.T) {
	mock, run := newMock(t)
	// outside the lock
	expectNoState(mock)
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c2")
	mock.ExpectQuery(q(qDatabase)).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("brain_unified"))
	mock.ExpectQuery(q(qGetLock)).WithArgs(sharedBackupLockName("brain_unified"), 0).
		WillReturnRows(sqlmock.NewRows([]string{"g"}).AddRow(1))
	// decided again under the lock
	expectNoState(mock)
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c2")
	// the state table is ensured before the sync, never after
	expectCount(mock, 0)
	mock.ExpectExec(q(qIgnore)).WithArgs(SharedBackupStateTable).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q(qCreate)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(q(qSync)).WithArgs(SharedBackupDestinationName).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(q(qSave)).WithArgs(SharedBackupStateKey, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(q(qRelease)).WithArgs(sharedBackupLockName("brain_unified")).
		WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))

	res, err := run(SharedBackupOptions{Interval: 15 * time.Minute})
	if err != nil || res.Outcome != SharedBackupSynced {
		t.Fatalf("got %+v, %v; want synced", res, err)
	}
	if res.State.LastDoltCommit != "c2" || !res.State.Timestamp.Equal(t0) || res.State.Destination != destURL {
		t.Fatalf("state = %+v", res.State)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Another store's command holds the lock: this one neither waits nor syncs.
func TestRunSharedBackupLockHeldIsInProgress(t *testing.T) {
	mock, run := newMock(t)
	expectState(mock, stateJSON("c1", t0.Add(-time.Hour)))
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c2")
	mock.ExpectQuery(q(qDatabase)).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("brain_unified"))
	mock.ExpectQuery(q(qGetLock)).WithArgs(sharedBackupLockName("brain_unified"), 0).
		WillReturnRows(sqlmock.NewRows([]string{"g"}).AddRow(0))
	res, err := run(SharedBackupOptions{Interval: 15 * time.Minute})
	if err != nil || res.Outcome != SharedBackupInProgress {
		t.Fatalf("got %+v, %v; want in-progress", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The store that held the lock finished a backup between our first check and
// our taking the lock: we see its state and do not sync again.
func TestRunSharedBackupRechecksUnderTheLock(t *testing.T) {
	mock, run := newMock(t)
	expectState(mock, stateJSON("c1", t0.Add(-time.Hour)))
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c2")
	mock.ExpectQuery(q(qDatabase)).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("brain_unified"))
	mock.ExpectQuery(q(qGetLock)).WithArgs(sharedBackupLockName("brain_unified"), 0).
		WillReturnRows(sqlmock.NewRows([]string{"g"}).AddRow(1))
	expectState(mock, stateJSON("c2", t0.Add(-time.Second))) // the other store's fresh state
	mock.ExpectQuery(q(qRelease)).WithArgs(sharedBackupLockName("brain_unified")).
		WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))
	res, err := run(SharedBackupOptions{Interval: 15 * time.Minute})
	if err != nil || res.Outcome != SharedBackupThrottled {
		t.Fatalf("got %+v, %v; want throttled", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A manual sync (Force) ignores the throttle and change detection, and waits
// for the lock for the time it is given.
func TestRunSharedBackupForceWaitsForTheLock(t *testing.T) {
	mock, run := newMock(t)
	expectState(mock, stateJSON("c1", t0))
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c1")
	mock.ExpectQuery(q(qDatabase)).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("brain_unified"))
	mock.ExpectQuery(q(qGetLock)).WithArgs(sharedBackupLockName("brain_unified"), 120).
		WillReturnRows(sqlmock.NewRows([]string{"g"}).AddRow(1))
	expectState(mock, stateJSON("c1", t0))
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c1")
	expectCount(mock, 1)
	mock.ExpectExec(q(qSync)).WithArgs(SharedBackupDestinationName).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(q(qSave)).WithArgs(SharedBackupStateKey, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(q(qRelease)).WithArgs(sharedBackupLockName("brain_unified")).
		WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))
	res, err := run(SharedBackupOptions{Force: true, LockWait: 2 * time.Minute})
	if err != nil || res.Outcome != SharedBackupSynced {
		t.Fatalf("got %+v, %v; want synced", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Refusals: each wraps ErrSharedBackupRefused, and none reaches the lock or
// the sync.
func TestRunSharedBackupRefusals(t *testing.T) {
	t.Run("no destination", func(t *testing.T) {
		mock, run := newMock(t)
		expectNoState(mock)
		expectBackups(mock)
		_, err := run(SharedBackupOptions{})
		if !errors.Is(err, ErrSharedBackupRefused) || !strings.Contains(err.Error(), "bd backup init") {
			t.Fatalf("err = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("state not json", func(t *testing.T) {
		mock, run := newMock(t)
		expectState(mock, "not json")
		if _, err := run(SharedBackupOptions{}); !errors.Is(err, ErrSharedBackupRefused) {
			t.Fatalf("err = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("state table unreadable", func(t *testing.T) {
		mock, run := newMock(t)
		expectCount(mock, 1)
		mock.ExpectQuery(q(qState)).WithArgs(SharedBackupStateKey).WillReturnError(errors.New("column not found"))
		if _, err := run(SharedBackupOptions{}); !errors.Is(err, ErrSharedBackupRefused) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("lock cannot be taken", func(t *testing.T) {
		mock, run := newMock(t)
		expectNoState(mock)
		expectBackups(mock, "default", destURL)
		expectHead(mock, "c2")
		mock.ExpectQuery(q(qDatabase)).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("brain_unified"))
		mock.ExpectQuery(q(qGetLock)).WillReturnError(errors.New("no GET_LOCK"))
		if _, err := run(SharedBackupOptions{}); !errors.Is(err, ErrSharedBackupRefused) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("state table cannot be created: refused before the sync", func(t *testing.T) {
		mock, run := newMock(t)
		expectNoState(mock)
		expectBackups(mock, "default", destURL)
		expectHead(mock, "c2")
		mock.ExpectQuery(q(qDatabase)).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("brain_unified"))
		mock.ExpectQuery(q(qGetLock)).WillReturnRows(sqlmock.NewRows([]string{"g"}).AddRow(1))
		expectNoState(mock)
		expectBackups(mock, "default", destURL)
		expectHead(mock, "c2")
		expectCount(mock, 0)
		mock.ExpectExec(q(qIgnore)).WillReturnError(errors.New("read only"))
		mock.ExpectQuery(q(qRelease)).WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))
		if _, err := run(SharedBackupOptions{}); !errors.Is(err, ErrSharedBackupRefused) {
			t.Fatalf("err = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// A sync that fails is not a refusal and leaves the state untouched, so the
// next command retries.
func TestRunSharedBackupSyncFailureKeepsState(t *testing.T) {
	mock, run := newMock(t)
	expectState(mock, stateJSON("c1", t0.Add(-time.Hour)))
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c2")
	mock.ExpectQuery(q(qDatabase)).WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow("brain_unified"))
	mock.ExpectQuery(q(qGetLock)).WillReturnRows(sqlmock.NewRows([]string{"g"}).AddRow(1))
	expectState(mock, stateJSON("c1", t0.Add(-time.Hour)))
	expectBackups(mock, "default", destURL)
	expectHead(mock, "c2")
	expectCount(mock, 1)
	mock.ExpectExec(q(qSync)).WillReturnError(errors.New("disk full"))
	mock.ExpectQuery(q(qRelease)).WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))
	_, err := run(SharedBackupOptions{Interval: 15 * time.Minute})
	if err == nil || errors.Is(err, ErrSharedBackupRefused) {
		t.Fatalf("err = %v; want a plain sync error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil { // no REPLACE INTO state was expected
		t.Fatal(err)
	}
}

func TestSharedBackupLockName(t *testing.T) {
	if got := sharedBackupLockName("brain_unified"); got != "bd-shared-backup:brain_unified" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", 80)
	got := sharedBackupLockName(long)
	if len(got) > sharedBackupLockNameMax || got == sharedBackupLockName(long+"y") {
		t.Fatalf("long names must fit GET_LOCK's limit and stay distinct: %q (%d)", got, len(got))
	}
}
