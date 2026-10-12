// Package main — brain_host_dolt.go
//
// The database half of a promotion: the few Dolt operations it needs, all of
// them Dolt's own. A copy is `CALL DOLT_BACKUP('sync-url', …)` on the old host
// (safe against a live server, incremental on repeat), a restore is
// `dolt backup restore` on the new one, and "the copy is exact" is
// `dolt_hashof_db()` — a hash of the whole database, working set included —
// being equal on both sides.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var doltDatabaseNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// openServerSQL opens a connection to the Dolt server on 127.0.0.1:port with db
// as the default schema ("" for none). The brain's server accepts root without
// a password on loopback, which is how every store wrapper connects too.
func openServerSQL(port int, db string) (*sql.DB, error) {
	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s?timeout=5s&multiStatements=false", port, db)
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(2)
	conn.SetConnMaxLifetime(10 * time.Minute)
	return conn, nil
}

// waitServerReady polls until the server on port answers a query, or ctx ends.
func waitServerReady(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		conn, err := openServerSQL(port, "")
		if err == nil {
			qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			var one int
			err = conn.QueryRowContext(qctx, "SELECT 1").Scan(&one)
			cancel()
			_ = conn.Close()
			if err == nil {
				return nil
			}
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("dolt server on 127.0.0.1:%d did not answer within %s: %w", port, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// portListening reports whether anything accepts TCP connections on 127.0.0.1:port.
func portListening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// listDatabases returns the user databases a server holds (system schemas excluded).
func listDatabases(ctx context.Context, port int) ([]string, error) {
	conn, err := openServerSQL(port, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		switch strings.ToLower(n) {
		case "information_schema", "mysql", "performance_schema", "sys", "dolt_cluster":
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// databaseHash is the hash of the whole database as the server sees it now.
func databaseHash(ctx context.Context, port int, db string) (string, error) {
	if !doltDatabaseNameRE.MatchString(db) {
		return "", fmt.Errorf("invalid database name %q", db)
	}
	conn, err := openServerSQL(port, db)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	var h string
	if err := conn.QueryRowContext(ctx, "SELECT dolt_hashof_db()").Scan(&h); err != nil {
		return "", fmt.Errorf("hashing database %s: %w", db, err)
	}
	return h, nil
}

// headCommit is the commit the default branch points at (informational: lets a
// later check prove the new host's history contains the old host's).
func headCommit(ctx context.Context, port int, db string) string {
	conn, err := openServerSQL(port, db)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	var h string
	if err := conn.QueryRowContext(ctx, "SELECT HASHOF('HEAD')").Scan(&h); err != nil {
		return ""
	}
	return h
}

// commitInHistory reports whether commit appears in the database's log.
func commitInHistory(ctx context.Context, port int, db, commit string) bool {
	if commit == "" {
		return false
	}
	conn, err := openServerSQL(port, db)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = ?", commit).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// quiesce waits until nothing else is running a statement on the server and the
// database hash has stopped moving, and returns that hash. It is the "writes
// have drained" check that follows pausing writers with the host gate.
func quiesce(ctx context.Context, port int, db string, settle time.Duration) (string, error) {
	conn, err := openServerSQL(port, db)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(90 * time.Second)
	calm := 0
	var prev string
	for {
		var busy int
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.processlist WHERE id <> CONNECTION_ID() AND LOWER(command) NOT IN ('sleep','daemon')").Scan(&busy); err != nil {
			// A server without a processlist cannot be asked; fall through to
			// the hash-stability check alone.
			busy = 0
		}
		var h string
		if err := conn.QueryRowContext(ctx, "SELECT dolt_hashof_db()").Scan(&h); err != nil {
			return "", fmt.Errorf("hashing database %s: %w", db, err)
		}
		if busy == 0 && h == prev {
			calm++
		} else {
			calm = 0
		}
		prev = h
		if calm >= 2 {
			return h, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("database %s is still changing after 90s (%d statements running): something is writing to it outside the brain commands, which the freeze cannot stop", db, busy)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(settle):
		}
	}
}

// backupSync copies the database into dir with Dolt's own backup. It is
// incremental: a repeat run transfers only what changed since the last one.
func backupSync(ctx context.Context, port int, db, dir string) error {
	if !doltDatabaseNameRE.MatchString(db) {
		return fmt.Errorf("invalid database name %q", db)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("backup directory %q must be absolute", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	conn, err := openServerSQL(port, db)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// One dedicated connection: DOLT_BACKUP runs on it and holds it until done.
	c, err := conn.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.ExecContext(ctx, "CALL DOLT_BACKUP('sync-url', ?)", "file://"+dir); err != nil {
		return fmt.Errorf("dolt backup of %s into %s: %w", db, dir, err)
	}
	return nil
}

// doltBinary finds dolt and reads its version ("2.2.3").
func doltBinary() (path, version string, err error) {
	path, err = exec.LookPath("dolt")
	if err != nil {
		return "", "", errors.New("the dolt command was not found in PATH")
	}
	out, err := exec.Command(path, "version").Output() //nolint:gosec // found on PATH by LookPath
	if err != nil {
		return path, "", fmt.Errorf("running dolt version: %w", err)
	}
	return path, parseDoltVersion(string(out)), nil
}

var doltVersionRE = regexp.MustCompile(`dolt version (\d+(?:\.\d+){1,2})`)

func parseDoltVersion(out string) string {
	if m := doltVersionRE.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// compareVersions compares dotted numeric versions: -1, 0, 1. Missing parts are 0.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// restoreBackup restores the Dolt backup at backupDir into destParent/name with
// `dolt backup restore`, then proves the result by hashing it offline. The
// restored database is exactly the one the backup was taken from, working set
// included, so wantHash (taken from the source while writes were paused) must
// equal what comes out.
func restoreBackup(ctx context.Context, doltBin, backupDir, destParent, name, wantHash string) (string, error) {
	if !doltDatabaseNameRE.MatchString(name) && !strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("invalid database directory name %q", name)
	}
	if err := os.MkdirAll(destParent, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(destParent, name)
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("%s already exists", dest)
	}
	cmd := exec.CommandContext(ctx, doltBin, "backup", "restore", "file://"+backupDir, name) //nolint:gosec // dolt found by LookPath
	cmd.Dir = destParent
	var stderr bytes.Buffer
	cmd.Stderr, cmd.Stdout = &stderr, &stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dest) // our own half-made directory
		return "", fmt.Errorf("dolt backup restore %s: %w: %s", backupDir, err, strings.TrimSpace(stderr.String()))
	}
	got, err := offlineHash(ctx, doltBin, dest)
	if err != nil {
		return "", err
	}
	if wantHash != "" && got != wantHash {
		return got, fmt.Errorf("restored database hash %s does not match the source's %s", got, wantHash)
	}
	return got, nil
}

// offlineHash hashes a database directory with no server running on it.
func offlineHash(ctx context.Context, doltBin, dbDir string) (string, error) {
	cmd := exec.CommandContext(ctx, doltBin, "sql", "-r", "csv", "-q", "SELECT dolt_hashof_db()") //nolint:gosec // dolt found by LookPath
	cmd.Dir = dbDir
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("hashing %s offline: %w: %s", dbDir, err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("hashing %s offline: unexpected output %q", dbDir, out.String())
	}
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// hostDirSize is the number of bytes under dir (0 if it does not exist).
func hostDirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// ---------------------------------------------------------------------------
// Parity manifest
// ---------------------------------------------------------------------------

// rowDigest is a row count and an order-independent content digest.
type rowDigest struct {
	Rows   int64  `json:"rows"`
	Digest string `json:"digest,omitempty"`
}

// storeManifest is one store's slice of the unified database: the issues whose
// ids carry its prefix, and the child rows that point at them.
type storeManifest struct {
	Store    string               `json:"store"`
	Prefixes []string             `json:"prefixes"`
	Tables   map[string]rowDigest `json:"tables"`
}

// dbManifest is everything a parity check compares between two copies of a
// database: the whole-database hash, a content hash per table, and per store
// the counts and digests of its rows.
type dbManifest struct {
	Database string               `json:"database"`
	Hash     string               `json:"hash"`
	Head     string               `json:"head,omitempty"`
	Tables   map[string]rowDigest `json:"tables"`
	Stores   []storeManifest      `json:"stores"`
}

// storeChildTables are the tables whose rows belong to a store through the id
// of the issue they hang off.
var storeChildTables = []struct{ table, idColumn string }{
	{"issues", "id"},
	{"comments", "issue_id"},
	{"labels", "issue_id"},
	{"dependencies", "issue_id"},
	{"events", "issue_id"},
}

// computeManifest reads the manifest from the server on port.
func computeManifest(ctx context.Context, port int, db string) (*dbManifest, error) {
	conn, err := openServerSQL(port, db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	m := &dbManifest{Database: db, Tables: map[string]rowDigest{}}
	if err := conn.QueryRowContext(ctx, "SELECT dolt_hashof_db()").Scan(&m.Hash); err != nil {
		return nil, fmt.Errorf("hashing database %s: %w", db, err)
	}
	m.Head = headCommit(ctx, port, db)

	tables, err := baseTables(ctx, conn, db)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		var rows int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+t+"`").Scan(&rows); err != nil {
			return nil, fmt.Errorf("counting %s.%s: %w", db, t, err)
		}
		d := rowDigest{Rows: rows}
		var th sql.NullString
		// Ignored (unversioned) tables have no table hash; their row count stands.
		if err := conn.QueryRowContext(ctx, "SELECT dolt_hashof_table(?)", t).Scan(&th); err == nil && th.Valid {
			d.Digest = th.String
		}
		m.Tables[t] = d
	}

	have := map[string]bool{}
	for _, t := range tables {
		have[t] = true
	}
	if !have["brain_store_prefixes"] || !have["issues"] {
		return m, nil // not a unified database: the table hashes are the whole story
	}
	prefixes, err := storePrefixes(ctx, conn)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(prefixes))
	for s := range prefixes {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		sm := storeManifest{Store: s, Prefixes: prefixes[s], Tables: map[string]rowDigest{}}
		for _, ct := range storeChildTables {
			if !have[ct.table] {
				continue
			}
			d, err := storeTableDigest(ctx, conn, db, ct.table, ct.idColumn, prefixes[s])
			if err != nil {
				return nil, fmt.Errorf("store %s, table %s: %w", s, ct.table, err)
			}
			sm.Tables[ct.table] = d
		}
		m.Stores = append(m.Stores, sm)
	}
	return m, nil
}

func baseTables(ctx context.Context, conn *sql.DB, db string) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE' ORDER BY table_name", db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func storePrefixes(ctx context.Context, conn *sql.DB) (map[string][]string, error) {
	rows, err := conn.QueryContext(ctx, "SELECT store, prefix FROM brain_store_prefixes ORDER BY store, prefix")
	if err != nil {
		return nil, fmt.Errorf("reading brain_store_prefixes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var s, p string
		if err := rows.Scan(&s, &p); err != nil {
			return nil, err
		}
		if s == "" || p == "" {
			continue
		}
		out[s] = append(out[s], p)
	}
	return out, rows.Err()
}

// storeTableDigest counts and digests the rows of table whose id column starts
// with one of the store's prefixes followed by '-'.
func storeTableDigest(ctx context.Context, conn *sql.DB, db, table, idColumn string, prefixes []string) (rowDigest, error) {
	cols, err := tableColumns(ctx, conn, db, table)
	if err != nil {
		return rowDigest{}, err
	}
	if len(cols) == 0 {
		return rowDigest{}, fmt.Errorf("table %s has no columns", table)
	}
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, "IFNULL(CAST(`"+c+"` AS CHAR), CHAR(0))")
	}
	rowExpr := "CRC32(CONCAT_WS(CHAR(31), " + strings.Join(parts, ", ") + "))"
	conds := make([]string, 0, len(prefixes))
	args := make([]any, 0, len(prefixes)*2)
	for _, p := range prefixes {
		conds = append(conds, "LEFT(`"+idColumn+"`, ?) = ?")
		args = append(args, len(p)+1, p+"-")
	}
	q := "SELECT COUNT(*), IFNULL(BIT_XOR(" + rowExpr + "), 0), IFNULL(SUM(" + rowExpr + "), 0) FROM `" + table + "` WHERE " + strings.Join(conds, " OR ")
	var n int64
	var x, s string
	if err := conn.QueryRowContext(ctx, q, args...).Scan(&n, &x, &s); err != nil {
		return rowDigest{}, err
	}
	return rowDigest{Rows: n, Digest: x + ":" + s}, nil
}

func tableColumns(ctx context.Context, conn *sql.DB, db, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		"SELECT column_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", db, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// compareManifests lists every way b differs from a (empty when they agree).
func compareManifests(a, b *dbManifest) []string {
	var diffs []string
	if a.Hash != b.Hash {
		diffs = append(diffs, fmt.Sprintf("database hash differs: %s vs %s", a.Hash, b.Hash))
	}
	tables := map[string]bool{}
	for t := range a.Tables {
		tables[t] = true
	}
	for t := range b.Tables {
		tables[t] = true
	}
	names := make([]string, 0, len(tables))
	for t := range tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		x, xok := a.Tables[t]
		y, yok := b.Tables[t]
		switch {
		case !xok:
			diffs = append(diffs, "table "+t+" is missing on the first side")
		case !yok:
			diffs = append(diffs, "table "+t+" is missing on the second side")
		case x != y:
			diffs = append(diffs, fmt.Sprintf("table %s differs: %d rows %q vs %d rows %q", t, x.Rows, x.Digest, y.Rows, y.Digest))
		}
	}
	bs := map[string]storeManifest{}
	for _, s := range b.Stores {
		bs[s.Store] = s
	}
	for _, s := range a.Stores {
		o, ok := bs[s.Store]
		if !ok {
			diffs = append(diffs, "store "+s.Store+" is missing on the second side")
			continue
		}
		delete(bs, s.Store)
		for t, d := range s.Tables {
			if o.Tables[t] != d {
				diffs = append(diffs, fmt.Sprintf("store %s, %s: %d rows %q vs %d rows %q", s.Store, t, d.Rows, d.Digest, o.Tables[t].Rows, o.Tables[t].Digest))
			}
		}
	}
	rest := make([]string, 0, len(bs))
	for s := range bs {
		rest = append(rest, s)
	}
	sort.Strings(rest)
	for _, s := range rest {
		diffs = append(diffs, "store "+s+" is missing on the first side")
	}
	return diffs
}

// manifestFingerprint is a short stable id for a manifest (for logs).
func manifestFingerprint(m *dbManifest) string {
	sum := sha256.New()
	_, _ = fmt.Fprintf(sum, "%s|%s", m.Database, m.Hash)
	for _, s := range m.Stores {
		_, _ = fmt.Fprintf(sum, "|%s", s.Store)
		keys := make([]string, 0, len(s.Tables))
		for t := range s.Tables {
			keys = append(keys, t)
		}
		sort.Strings(keys)
		for _, t := range keys {
			_, _ = fmt.Fprintf(sum, ":%s=%d/%s", t, s.Tables[t].Rows, s.Tables[t].Digest)
		}
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// sumRows totals one table's rows across every store of a manifest.
func (m *dbManifest) storeRows(store, table string) int64 {
	for _, s := range m.Stores {
		if s.Store == store {
			return s.Tables[table].Rows
		}
	}
	return 0
}
