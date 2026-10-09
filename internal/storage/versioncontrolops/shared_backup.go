package versioncontrolops

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// This file is the automatic backup of a database that several stores share
// (the brain unified database). bd's per-store auto-backup registers a
// backup_export destination under the store's own .beads/backup folder and
// keeps its throttle there; on a shared database that produces one backup of
// the same data per store. The shared path replaces all three with
// database-level state:
//
//   - destination: the 'default' entry of dolt_backups, the one `bd backup
//     init` registers. Auto-backup never registers a destination of its own on
//     a shared database, so the registration is not pointed back and forth.
//   - throttle and change detection: one row in a table of its own,
//     brain_shared_backup_state, registered in dolt_ignore. The table is
//     unversioned, so recording a backup does not move HEAD and trigger the
//     next one. (local_metadata is not used: the merged database does not
//     carry bd's dolt_ignore rows, so there it is a tracked table.)
//   - exclusion: a Dolt named lock (GET_LOCK) per database, so two stores
//     whose commands finish together run one sync, and the second sees the
//     first's state.
//
// divergence/0032-shared-database-backup.md records the design.

// SharedBackupDestinationName is the dolt_backups entry the shared backup
// syncs to. It is the name `bd backup init` registers and `bd backup sync`
// pushes to, so manual and automatic backups of a shared database are one
// destination.
const SharedBackupDestinationName = "default"

// SharedBackupStateTable is the dolt-ignored table holding the database-level
// throttle and change-detection state, one row under SharedBackupStateKey.
const SharedBackupStateTable = "brain_shared_backup_state"

// SharedBackupStateKey is the row key of the state in SharedBackupStateTable.
const SharedBackupStateKey = "state"

// sharedMarkerTable is the table whose presence marks a database as shared by
// several stores. It is the same signal issueops.UnifiedScope uses.
const sharedMarkerTable = "brain_unified_config"

// sharedBackupLockPrefix namespaces the Dolt named lock. Named locks are
// server-wide, so the database name is part of the lock name.
const sharedBackupLockPrefix = "bd-shared-backup:"

// sharedBackupLockNameMax is MySQL's GET_LOCK name limit.
const sharedBackupLockNameMax = 64

// ErrSharedBackupRefused marks the cases where a shared database's backup
// cannot be resolved. The caller must report it loudly and skip the backup;
// it must never fall back to a per-store copy.
var ErrSharedBackupRefused = errors.New("shared-database backup refused")

// SharedBackupState is the database-level record of the last backup.
type SharedBackupState struct {
	LastDoltCommit string    `json:"last_dolt_commit"`
	Timestamp      time.Time `json:"timestamp"`
	Destination    string    `json:"destination,omitempty"`
}

// SharedBackupOutcome says what RunSharedBackup did.
type SharedBackupOutcome string

const (
	// SharedBackupSynced: this call ran the sync.
	SharedBackupSynced SharedBackupOutcome = "synced"
	// SharedBackupThrottled: a backup finished within the interval.
	SharedBackupThrottled SharedBackupOutcome = "throttled"
	// SharedBackupUnchanged: HEAD is the commit the last backup covered.
	SharedBackupUnchanged SharedBackupOutcome = "unchanged"
	// SharedBackupInProgress: another store's command holds the lock and is
	// running the sync; nothing for this call to do.
	SharedBackupInProgress SharedBackupOutcome = "in-progress"
)

// SharedBackupOptions tunes one RunSharedBackup call.
type SharedBackupOptions struct {
	// Interval is the minimum time between backups (backup.interval).
	Interval time.Duration
	// Force skips the throttle and change detection (a manual sync).
	Force bool
	// LockWait is how long to wait for a sync another store is running. Zero
	// does not wait: the call reports SharedBackupInProgress.
	LockWait time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
}

// SharedBackupResult is the outcome plus the state in force afterwards.
type SharedBackupResult struct {
	Outcome     SharedBackupOutcome
	Destination string
	State       SharedBackupState
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSharedBackupRefused, fmt.Sprintf(format, args...))
}

// IsSharedDatabase reports whether the connected database is shared by several
// stores: it carries the unified database's re-keyed config table. A probe
// error is returned, not folded into "not shared" — treating an unanswerable
// question as a per-store database is exactly the guess that makes a copy per
// store.
func IsSharedDatabase(ctx context.Context, db DBConn) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?",
		sharedMarkerTable).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("probe for the shared-database marker table: %w", err)
	}
	return n > 0, nil
}

// SharedBackupDestination resolves the shared backup's destination from the
// database's own registrations. Only the 'default' entry qualifies; anything
// else, or nothing, is a refusal.
func SharedBackupDestination(ctx context.Context, db DBConn) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, url FROM dolt_backups")
	if err != nil {
		return "", refuse("cannot read the registered backup destinations (dolt_backups): %v", err)
	}
	defer func() { _ = rows.Close() }()
	registered := map[string]string{}
	for rows.Next() {
		var name, url string
		if err := rows.Scan(&name, &url); err != nil {
			return "", refuse("cannot read the registered backup destinations (dolt_backups): %v", err)
		}
		registered[name] = url
	}
	if err := rows.Err(); err != nil {
		return "", refuse("cannot read the registered backup destinations (dolt_backups): %v", err)
	}
	if url := registered[SharedBackupDestinationName]; url != "" {
		return url, nil
	}
	if len(registered) == 0 {
		return "", refuse("this database is shared by several stores and has no backup destination; " +
			"register one once with 'bd backup init <path>' (auto-backup will not register a per-store copy)")
	}
	names := make([]string, 0, len(registered))
	for n := range registered {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", refuse("this database is shared by several stores and has no %q backup destination "+
		"(registered: %s); register the shared one with 'bd backup init <path>' — auto-backup will not guess between destinations",
		SharedBackupDestinationName, strings.Join(names, ", "))
}

// sharedBackupStateTableExists says whether the state table is present. A
// database that has never been backed up does not have it yet.
func sharedBackupStateTableExists(ctx context.Context, db DBConn) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?",
		SharedBackupStateTable).Scan(&n)
	return n > 0, err
}

// LoadSharedBackupState reads the database-level state. A database with no
// state table or row yet has the zero state; an unreadable table is a
// refusal, because without the state every store's command would run its own
// sync.
func LoadSharedBackupState(ctx context.Context, db DBConn) (SharedBackupState, error) {
	exists, err := sharedBackupStateTableExists(ctx, db)
	if err != nil {
		return SharedBackupState{}, refuse("cannot look for the shared backup state (%s): %v", SharedBackupStateTable, err)
	}
	if !exists {
		return SharedBackupState{}, nil
	}
	var raw string
	err = db.QueryRowContext(ctx, "SELECT value FROM "+SharedBackupStateTable+" WHERE `key` = ?", SharedBackupStateKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedBackupState{}, nil
	}
	if err != nil {
		return SharedBackupState{}, refuse("cannot read the shared backup state (%s): %v", SharedBackupStateTable, err)
	}
	var st SharedBackupState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return SharedBackupState{}, refuse("the shared backup state is not readable (%s): %v", SharedBackupStateTable, err)
	}
	return st, nil
}

// EnsureSharedBackupStateTable creates the state table, registered in
// dolt_ignore first so it is never versioned. It runs before a sync, so a
// database that cannot hold the state is refused instead of syncing on every
// command.
func EnsureSharedBackupStateTable(ctx context.Context, db DBConn) error {
	if exists, err := sharedBackupStateTableExists(ctx, db); err != nil {
		return refuse("cannot look for the shared backup state (%s): %v", SharedBackupStateTable, err)
	} else if exists {
		return nil
	}
	if _, err := db.ExecContext(ctx, "REPLACE INTO dolt_ignore VALUES (?, true)", SharedBackupStateTable); err != nil {
		return refuse("cannot register the shared backup state table in dolt_ignore: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+SharedBackupStateTable+
		" (`key` VARCHAR(64) PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		return refuse("cannot create the shared backup state table %s: %v", SharedBackupStateTable, err)
	}
	return nil
}

// SaveSharedBackupState records the state of a completed backup.
func SaveSharedBackupState(ctx context.Context, db DBConn, st SharedBackupState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "REPLACE INTO "+SharedBackupStateTable+" (`key`, value) VALUES (?, ?)", SharedBackupStateKey, string(data)); err != nil {
		return fmt.Errorf("record the shared backup state: %w", err)
	}
	return nil
}

// sharedBackupLockName names the per-database lock, hashing a name that would
// not fit GET_LOCK's limit.
func sharedBackupLockName(database string) string {
	raw := sharedBackupLockPrefix + database
	if len(raw) <= sharedBackupLockNameMax {
		return raw
	}
	sum := sha256.Sum256([]byte(database))
	return sharedBackupLockPrefix + hex.EncodeToString(sum[:])[:32]
}

func throttled(st SharedBackupState, interval time.Duration, now time.Time) bool {
	return !st.Timestamp.IsZero() && now.Sub(st.Timestamp) < interval
}

func unchanged(st SharedBackupState, head string) bool {
	return st.LastDoltCommit != "" && st.LastDoltCommit == head
}

// RunSharedBackup backs a shared database up once, to its registered
// destination, whichever store's command asks. db must be the shared database
// (see IsSharedDatabase). It checks the throttle and change detection without
// the lock, takes the database's named lock, checks them again — the store
// that held the lock may have just finished — and only then syncs.
//
// Every refusal wraps ErrSharedBackupRefused. A sync that fails leaves the
// state untouched, so the next command retries.
func RunSharedBackup(ctx context.Context, db *sql.DB, opts SharedBackupOptions) (SharedBackupResult, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = 15 * time.Minute
	}

	// Locks are session-scoped: the lock, the checks and the sync share one
	// connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return SharedBackupResult{}, refuse("cannot open a connection to the shared database: %v", err)
	}
	defer func() { _ = conn.Close() }()

	st, err := LoadSharedBackupState(ctx, conn)
	if err != nil {
		return SharedBackupResult{}, err
	}
	if !opts.Force && throttled(st, opts.Interval, now()) {
		return SharedBackupResult{Outcome: SharedBackupThrottled, State: st}, nil
	}
	dest, err := SharedBackupDestination(ctx, conn)
	if err != nil {
		return SharedBackupResult{}, err
	}
	head, err := sharedHead(ctx, conn)
	if err != nil {
		return SharedBackupResult{}, err
	}
	if !opts.Force && unchanged(st, head) {
		return SharedBackupResult{Outcome: SharedBackupUnchanged, Destination: dest, State: st}, nil
	}

	var database string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil || database == "" {
		return SharedBackupResult{}, refuse("cannot name the connected database for the backup lock: %v", err)
	}
	lock := sharedBackupLockName(database)
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", lock, int(opts.LockWait.Seconds())).Scan(&got); err != nil || !got.Valid {
		return SharedBackupResult{}, refuse("cannot take the backup lock %q: %v", lock, err)
	}
	if got.Int64 != 1 {
		return SharedBackupResult{Outcome: SharedBackupInProgress, Destination: dest, State: st}, nil
	}
	defer releaseSharedBackupLock(conn, lock)

	// The store that held the lock may have finished a backup: decide again.
	if st, err = LoadSharedBackupState(ctx, conn); err != nil {
		return SharedBackupResult{}, err
	}
	if !opts.Force && throttled(st, opts.Interval, now()) {
		return SharedBackupResult{Outcome: SharedBackupThrottled, Destination: dest, State: st}, nil
	}
	if dest, err = SharedBackupDestination(ctx, conn); err != nil {
		return SharedBackupResult{}, err
	}
	if head, err = sharedHead(ctx, conn); err != nil {
		return SharedBackupResult{}, err
	}
	if !opts.Force && unchanged(st, head) {
		return SharedBackupResult{Outcome: SharedBackupUnchanged, Destination: dest, State: st}, nil
	}

	if err := EnsureSharedBackupStateTable(ctx, conn); err != nil {
		return SharedBackupResult{}, err
	}
	if err := BackupSync(ctx, conn, SharedBackupDestinationName); err != nil {
		return SharedBackupResult{}, err
	}
	next := SharedBackupState{LastDoltCommit: head, Timestamp: now().UTC(), Destination: dest}
	if err := SaveSharedBackupState(ctx, conn, next); err != nil {
		return SharedBackupResult{}, err
	}
	return SharedBackupResult{Outcome: SharedBackupSynced, Destination: dest, State: next}, nil
}

// sharedHead reads HEAD before the sync, so a commit that lands during the
// sync is never recorded as backed up.
func sharedHead(ctx context.Context, db DBConn) (string, error) {
	var head string
	if err := db.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&head); err != nil {
		return "", refuse("cannot read the current Dolt commit: %v", err)
	}
	return head, nil
}

func releaseSharedBackupLock(conn *sql.Conn, lock string) {
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released sql.NullInt64
	if err := conn.QueryRowContext(cleanup, "SELECT RELEASE_LOCK(?)", lock).Scan(&released); err != nil || !released.Valid || released.Int64 != 1 {
		// Closing the connection ends the session, which frees the lock.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}
