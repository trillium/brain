package brainupgrade

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage/doltutil"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// ManifestName is the file in a backup directory that records each backup
// and the state it holds.
const ManifestName = "brain-upgrade-manifest.json"

// manifestFormat is the manifest's format version.
const manifestFormat = 1

// Manifest records the backups in one backup directory.
type Manifest struct {
	FormatVersion int      `json:"format_version"`
	Server        string   `json:"server"`
	BD            string   `json:"bd_version,omitempty"`
	Backups       []Backup `json:"backups"`
}

// Backup is one database's backup: where it is, the state it holds, and
// whether a restore of it was proven to reproduce that state.
type Backup struct {
	Database   string        `json:"database"`
	URL        string        `json:"url"`
	State      DatabaseState `json:"state"`
	Verified   bool          `json:"verified"`
	VerifiedAt time.Time     `json:"verified_at,omitzero"`
	// VerifyDiffs are the differences a failed verification found.
	VerifyDiffs []string `json:"verify_diffs,omitempty"`
}

// Get returns the backup of database.
func (m *Manifest) Get(database string) (Backup, bool) {
	for _, b := range m.Backups {
		if b.Database == database {
			return b, true
		}
	}
	return Backup{}, false
}

// Put records b, replacing an earlier backup of the same database.
func (m *Manifest) Put(b Backup) {
	for i := range m.Backups {
		if m.Backups[i].Database == b.Database {
			m.Backups[i] = b
			return
		}
	}
	m.Backups = append(m.Backups, b)
}

// LoadManifest reads dir's manifest. A directory without one answers an
// error that wraps os.ErrNotExist.
func LoadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName)) //nolint:gosec // operator-supplied backup dir
	if err != nil {
		return nil, fmt.Errorf("reading the backup manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filepath.Join(dir, ManifestName), err)
	}
	if m.FormatVersion != manifestFormat {
		return nil, fmt.Errorf("%s has format %d; this binary reads format %d", filepath.Join(dir, ManifestName), m.FormatVersion, manifestFormat)
	}
	return &m, nil
}

// Save writes the manifest into dir, replacing it in one rename.
func (m *Manifest) Save(dir string) error {
	m.FormatVersion = manifestFormat
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ManifestName+".tmp")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing the backup manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, ManifestName)); err != nil {
		return fmt.Errorf("writing the backup manifest: %w", err)
	}
	return nil
}

// BackupURL is where database's backup goes inside dir.
func BackupURL(dir, database string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return "file://" + filepath.ToSlash(filepath.Join(abs, database)), nil
}

// Endpoint names a Dolt sql-server and the login to use on it.
type Endpoint struct {
	Host     string
	Port     int
	User     string
	Password string
}

func (e Endpoint) String() string { return fmt.Sprintf("%s:%d", e.Host, e.Port) }

// Open opens a pool on the server, with database selected ("" for none).
func (e Endpoint) Open(ctx context.Context, database string) (*sql.DB, error) {
	dsn := doltutil.ServerDSN{Host: e.Host, Port: e.Port, User: e.User, Password: e.Password, Database: database, Timeout: 30 * time.Second}
	db, err := sql.Open("mysql", dsn.String())
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connecting to %s as %s: %w", e, e.User, err)
	}
	return db, nil
}

// Conn opens one pinned connection with database selected. Closing it closes
// its pool.
func (e Endpoint) Conn(ctx context.Context, database string) (*Conn, error) {
	db, err := e.Open(ctx, database)
	if err != nil {
		return nil, err
	}
	c, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Conn{Conn: c, db: db}, nil
}

// Conn is a pinned connection that owns its pool.
type Conn struct {
	*sql.Conn
	db *sql.DB
}

// Close closes the connection and its pool.
func (c *Conn) Close() error {
	err := c.Conn.Close()
	return errors.Join(err, c.db.Close())
}

// BeadsDatabases lists the databases on the server that hold beads (an issues
// table), sorted.
func BeadsDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT DISTINCT table_schema FROM information_schema.tables WHERE table_name = 'issues' ORDER BY table_schema")
	if err != nil {
		return nil, fmt.Errorf("listing beads databases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		switch name {
		case "information_schema", "mysql", "performance_schema", "sys":
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Remotes lists the Dolt remotes of the database conn has selected, as
// "name=url".
func Remotes(ctx context.Context, conn *sql.Conn) ([]string, error) {
	return registrations(ctx, conn, "dolt_remotes")
}

// ErrChanged reports a database that is no longer the state its backup holds.
var ErrChanged = errors.New("database changed since its backup")

// TakeBackup syncs the database conn has selected to url with
// DOLT_BACKUP('sync-url'), which registers no backup remote on the server, and
// returns the state the backup holds. It reads the state before the sync and
// the database hash after it; the two agreeing proves nothing wrote while the
// backup ran.
func TakeBackup(ctx context.Context, conn *sql.Conn, url string) (DatabaseState, error) {
	st, err := Capture(ctx, conn)
	if err != nil {
		return st, err
	}
	if err := schema.DrainCall(ctx, conn, "CALL DOLT_BACKUP('sync-url', ?)", url); err != nil {
		return st, fmt.Errorf("backing %s up to %s: %w", st.Name, url, err)
	}
	_, after, err := Identity(ctx, conn)
	if err != nil {
		return st, err
	}
	if after != st.DBHash {
		return st, fmt.Errorf("%w: %s moved from %s to %s while the backup ran; stop every writer and back up again", ErrChanged, st.Name, st.DBHash, after)
	}
	return st, nil
}

// Restore restores the backup at url as database on the server db talks to.
// With force it replaces a database of that name; without, it refuses one.
func Restore(ctx context.Context, db *sql.DB, url, database string, force bool) error {
	q := "CALL DOLT_BACKUP('restore', ?, ?)"
	if force {
		q = "CALL DOLT_BACKUP('restore', '--force', ?, ?)"
	}
	if err := schema.DrainCall(ctx, db, q, url, database); err != nil {
		return fmt.Errorf("restoring %s from %s: %w", database, url, err)
	}
	return nil
}

// VerifyBackup lays b out as a database under scratchDir (see Materialize),
// opens it on a Dolt server of its own, reads its state and compares it with
// the state b recorded. It returns the differences; none means the backup
// reproduces the database. scratchDir is left for the caller to remove.
func VerifyBackup(ctx context.Context, bin, scratchDir string, b Backup) ([]string, error) {
	dataDir := filepath.Join(scratchDir, "data")
	if err := Materialize(b, filepath.Join(dataDir, b.Database)); err != nil {
		return nil, err
	}
	srv, err := StartServer(ctx, bin, dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = srv.Stop() }()
	return CheckRestored(ctx, srv.Endpoint(), b)
}

// CheckRestored reads the state of b's database on the server at ep and
// compares it with the state b recorded, for a copy that must be exactly the
// backup (a verification copy, a rollback).
func CheckRestored(ctx context.Context, ep Endpoint, b Backup) ([]string, error) {
	c, err := ep.Conn(ctx, b.Database)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	got, err := Capture(ctx, c.Conn)
	if err != nil {
		return nil, err
	}
	return CompareIdentical(b.State, got), nil
}

// ApplyResult is one database's migration.
type ApplyResult struct {
	Database string             `json:"database"`
	Before   schema.UpgradePlan `json:"before"`
	Applied  int                `json:"applied"`
	Seconds  float64            `json:"seconds"`
	Report   MigrationReport    `json:"report"`
}

// Apply migrates the database conn has selected to this binary's schema and
// verifies the result against b. It refuses unless b was verified and the
// database is still exactly the state b holds, so whatever the migration does
// can be undone by restoring b.
func Apply(ctx context.Context, conn *sql.Conn, b Backup) (ApplyResult, error) {
	res := ApplyResult{Database: b.Database}
	if !b.Verified {
		return res, fmt.Errorf("the backup of %s was never verified; run 'backup' again", b.Database)
	}
	name, dbHash, err := Identity(ctx, conn)
	if err != nil {
		return res, err
	}
	if name != b.Database {
		return res, fmt.Errorf("connected to %s, but the backup is of %s", name, b.Database)
	}
	if dbHash != b.State.DBHash {
		return res, fmt.Errorf("%w: %s is at %s, its backup holds %s; take a new backup with every writer stopped", ErrChanged, name, dbHash, b.State.DBHash)
	}
	if res.Before, err = schema.PlanUpgrade(ctx, conn); err != nil {
		return res, err
	}
	if res.Before.HealRefusal != "" {
		return res, fmt.Errorf("%s: %s", name, res.Before.HealRefusal)
	}
	start := time.Now()
	res.Applied, err = schema.MigrateUpWithLock(ctx, conn, name)
	res.Seconds = time.Since(start).Seconds()
	if err != nil {
		return res, fmt.Errorf("migrating %s: %w", name, err)
	}
	if res.Report, err = VerifyMigrated(ctx, conn, b.State); err != nil {
		return res, err
	}
	return res, nil
}

// RollbackResult is one database's return to its backup.
type RollbackResult struct {
	Database string `json:"database"`
	// Mode is "online" (DOLT_BACKUP restore on the running server) or
	// "offline" (the backup laid out in place of the stopped server's
	// database directory).
	Mode    string   `json:"mode"`
	Aside   string   `json:"replaced_dir_kept_at,omitempty"`
	Seconds float64  `json:"seconds"`
	Diffs   []string `json:"diffs,omitempty"`
}

// OK reports whether the database is the backed-up state again.
func (r RollbackResult) OK() bool { return len(r.Diffs) == 0 }

// Rollback restores b over its database on the running server at srv with
// DOLT_BACKUP('restore', '--force'), puts back the remotes and backup
// registrations b recorded (a restore drops the repository's config), and
// proves the result is the backed-up state. On a large database this is slow:
// Dolt checks every chunk's references across the whole history. The offline
// path (OfflineRollback) takes minutes.
func Rollback(ctx context.Context, srv Endpoint, b Backup) (RollbackResult, error) {
	res := RollbackResult{Database: b.Database, Mode: "online"}
	root, err := srv.Open(ctx, "")
	if err != nil {
		return res, err
	}
	defer func() { _ = root.Close() }()
	start := time.Now()
	if err := Restore(ctx, root, b.URL, b.Database, true); err != nil {
		return res, err
	}
	res.Seconds = time.Since(start).Seconds()
	c, err := srv.Conn(ctx, b.Database)
	if err != nil {
		return res, err
	}
	for _, reg := range []struct {
		call  string
		pairs []string
	}{{"DOLT_REMOTE", b.State.Remotes}, {"DOLT_BACKUP", b.State.Backups}} {
		for _, pair := range reg.pairs {
			name, u, _ := strings.Cut(pair, "=")
			if err := schema.DrainCall(ctx, c.Conn, "CALL "+reg.call+"('add', ?, ?)", name, u); err != nil {
				_ = c.Close()
				return res, fmt.Errorf("re-registering %s %s on %s: %w", strings.ToLower(strings.TrimPrefix(reg.call, "DOLT_")), name, b.Database, err)
			}
		}
	}
	_ = c.Close()
	res.Diffs, err = CheckRestored(ctx, srv, b)
	return res, err
}

// Rehearsal is a migration run on a copy laid out from a backup.
type Rehearsal struct {
	Database string `json:"database"`
	// CopyDiffs compares the copy with the backed-up state; it must be
	// empty, or the rehearsal did not run on the backup.
	CopyDiffs []string       `json:"copy_diffs,omitempty"`
	Apply     ApplyResult    `json:"apply"`
	After     DatabaseState  `json:"after"`
	Rollback  RollbackResult `json:"rollback"`
}

// OK reports whether the copy was the backup, the migration verified and the
// rollback reproduced the backed-up state.
func (r Rehearsal) OK() bool {
	return len(r.CopyDiffs) == 0 && r.Apply.Report.OK() && r.Rollback.OK()
}

// Rehearse lays b out under scratchDir, serves it on a Dolt server of its own,
// migrates and verifies the copy, reads its state after the migration, then
// rolls it back the way the runbook does on the host (stop the server, put
// the backup in place of the database directory, start it) and proves the
// rollback. Nothing outside scratchDir is touched.
func Rehearse(ctx context.Context, bin, scratchDir string, b Backup) (Rehearsal, error) {
	reh := Rehearsal{Database: b.Database}
	dataDir := filepath.Join(scratchDir, "data")
	if err := Materialize(b, filepath.Join(dataDir, b.Database)); err != nil {
		return reh, err
	}
	srv, err := StartServer(ctx, bin, dataDir)
	if err != nil {
		return reh, err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = srv.Stop()
		}
	}()
	c, err := srv.Endpoint().Conn(ctx, b.Database)
	if err != nil {
		return reh, err
	}
	copyState, err := Capture(ctx, c.Conn)
	if err != nil {
		_ = c.Close()
		return reh, err
	}
	if reh.CopyDiffs = CompareIdentical(b.State, copyState); len(reh.CopyDiffs) > 0 {
		_ = c.Close()
		return reh, nil
	}
	if reh.Apply, err = Apply(ctx, c.Conn, b); err != nil {
		_ = c.Close()
		return reh, err
	}
	if reh.After, err = Capture(ctx, c.Conn); err != nil {
		_ = c.Close()
		return reh, err
	}
	_ = c.Close()

	start := time.Now()
	if err := srv.Stop(); err != nil {
		return reh, err
	}
	stopped = true
	aside, err := OfflineRollback(dataDir, b)
	if err != nil {
		return reh, err
	}
	srv, err = StartServer(ctx, bin, dataDir)
	if err != nil {
		return reh, err
	}
	stopped = false
	reh.Rollback = RollbackResult{Database: b.Database, Mode: "offline", Aside: aside, Seconds: time.Since(start).Seconds()}
	reh.Rollback.Diffs, err = CheckRestored(ctx, srv.Endpoint(), b)
	return reh, err
}
