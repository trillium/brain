package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/versioncontrolops"
)

// Shared-database backup (divergence/0032-shared-database-backup.md).
//
// When several stores share one Dolt database (the brain unified database),
// the per-store auto-backup would write one backup of the same data per store.
// For such a database the backup is taken once, to the destination `bd backup
// init` registered, with throttle and change-detection state kept in the
// database and a Dolt named lock between stores. A database used by one store
// never reaches this file's code paths.

// sharedBackupLockWait is how long a manual `bd backup sync` waits for a sync
// another store's command is running before it gives up.
const sharedBackupLockWait = 2 * time.Minute

// sharedBackupDB returns the raw connection to the shared-database probe and
// backup. A store with no server connection (embedded Dolt) cannot be shared
// with another process, so it is answered as "no connection".
func sharedBackupDB() *sql.DB {
	if store == nil {
		return nil
	}
	accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
	if !ok {
		return nil
	}
	return accessor.UnderlyingDB()
}

// sharedBackupTarget says whether the connected database is shared, returning
// its connection when it is. A probe that cannot be answered is an error: the
// caller refuses rather than treating the database as a per-store one.
func sharedBackupTarget(ctx context.Context) (*sql.DB, bool, error) {
	db := sharedBackupDB()
	if db == nil {
		return nil, false, nil
	}
	shared, err := versioncontrolops.IsSharedDatabase(ctx, db)
	if err != nil {
		return nil, false, err
	}
	return db, shared, nil
}

// reportSharedBackupRefusal says loudly that no backup was taken. It is not
// gated by --quiet: a skipped backup of the shared database is the one
// failure the operator must not miss.
func reportSharedBackupRefusal(err error) {
	fmt.Fprintf(os.Stderr, "Warning: auto-backup REFUSED — no backup was taken: %v\n", err)
}

// maybeSharedAutoBackup handles auto-backup when the connected database is
// shared. It reports whether it handled the call; false means the database is
// a per-store one and the caller proceeds with the per-store backup.
func maybeSharedAutoBackup(ctx context.Context) bool {
	db, shared, err := sharedBackupTarget(ctx)
	if err != nil {
		// Cannot tell whether the database is shared: skip rather than risk
		// a per-store copy of a shared database.
		reportSharedBackupRefusal(fmt.Errorf("cannot tell whether this database is shared by several stores: %w", err))
		return true
	}
	if !shared {
		return false
	}

	res, err := versioncontrolops.RunSharedBackup(ctx, db, versioncontrolops.SharedBackupOptions{
		Interval: config.GetDuration("backup.interval"),
	})
	switch {
	case errors.Is(err, versioncontrolops.ErrSharedBackupRefused):
		reportSharedBackupRefusal(err)
	case err != nil:
		if !isQuiet() && !jsonOutput {
			fmt.Fprintf(os.Stderr, "Warning: auto-backup failed: %v\n", err)
		}
		debug.Logf("backup: shared database: error: %v\n", err)
	default:
		debug.Logf("backup: shared database: %s\n", res.Outcome)
	}
	return true
}

// sharedBackupStatus is the shared database's backup record for `bd backup
// status`.
type sharedBackupStatus struct {
	Destination string                              `json:"destination,omitempty"`
	Refusal     string                              `json:"refusal,omitempty"`
	State       versioncontrolops.SharedBackupState `json:"state"`
}

func loadSharedBackupStatus(ctx context.Context, db *sql.DB) sharedBackupStatus {
	var out sharedBackupStatus
	st, err := versioncontrolops.LoadSharedBackupState(ctx, db)
	if err != nil {
		out.Refusal = err.Error()
		return out
	}
	out.State = st
	dest, err := versioncontrolops.SharedBackupDestination(ctx, db)
	if err != nil {
		out.Refusal = err.Error()
		return out
	}
	out.Destination = dest
	return out
}

// printSharedBackupStatus renders `bd backup status` for a shared database.
func printSharedBackupStatus(ctx context.Context, db *sql.DB) error {
	status := loadSharedBackupStatus(ctx, db)
	if jsonOutput {
		data, err := json.MarshalIndent(map[string]interface{}{"shared_database": status}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Println("Shared database backup (one backup for every store on this database):")
	if status.Destination != "" {
		fmt.Printf("  Destination: %s\n", status.Destination)
	}
	if status.State.LastDoltCommit != "" {
		fmt.Printf("  Last backup: %s (%s ago)\n",
			status.State.Timestamp.Format(time.RFC3339),
			time.Since(status.State.Timestamp).Round(time.Second))
		fmt.Printf("  Dolt commit: %s\n", status.State.LastDoltCommit)
	} else {
		fmt.Println("  Last backup: never")
	}
	fmt.Printf("\nConfig: enabled=%v interval=%s\n", isBackupAutoEnabled(), config.GetDuration("backup.interval"))
	if status.Refusal != "" {
		fmt.Printf("\nAuto-backup is REFUSED: %s\n", status.Refusal)
	}
	return nil
}
