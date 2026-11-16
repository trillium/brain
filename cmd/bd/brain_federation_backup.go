package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brainupgrade"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage/schema"
)

// 'bd brain federation-backup' backs the federation up the only way the
// 2026-08-04 outage left open: it asks the Dolt server for a backup of each
// database (`CALL DOLT_BACKUP('sync-url', 'file://…')`) and never reads the
// server's data directory itself. A file-level copy of a live data directory
// is what took the federation down; this command has no code path that
// copies one, and it refuses a destination that lies inside one.
//
// Every database the registry's stores name is backed up once (the shared
// brain_unified database is named by every store and backed up once), then
// each backup is proved: laid out on a Dolt server of its own, compared with
// the state the database had before the sync, and listed with the real
// binary (`bd list`) for every store that lives in it.

var brainFederationBackupCmd = &cobra.Command{
	Use:     "federation-backup",
	Aliases: []string{"fedbackup"},
	Short:   "Back every registered store's database up with DOLT_BACKUP and prove each backup on a Dolt server of its own",
	Long: `Backs the federation up without ever copying the Dolt server's data directory.

For every store in the registry (the stores 'brain stores list' prints) the
command reads the store's metadata, collects the databases the stores name
(a shared database is backed up once), confirms each is on the server with
SHOW DATABASES, and asks the server for a Dolt-native backup of it:

  CALL DOLT_BACKUP('sync-url', 'file://<dest>/<database>')

The server writes the backup; this command never reads the data directory.
A second run syncs into the same backup, so <dest> stays one backup per
database. Then each backup is proved:

  - it is laid out as a database on a Dolt sql-server of its own (a free
    loopback port under --scratch-dir, stopped and removed afterwards);
  - its dolt_hashof_db, HEAD and every table hash are compared with the
    state the database had before the sync; when something wrote to the
    database while the backup ran, the pre-sync HEAD must be in the backup's
    history instead;
  - 'bd list' is run by this binary against the copy for every store that
    lives in the database, the way the store's wrapper would run it.

Each step is timed, and a report is written under <dest>/reports/.

The destination is refused when it lies inside a Dolt database or data
directory, or inside a registered store's directory. Run the command on the
server's own machine: a file:// backup is written where the server runs, and
the proof opens it from there.

Login: --user, else BEADS_DOLT_SERVER_USER, else root; the password from
BEADS_DOLT_PASSWORD, else the [host:port] entry of the beads credentials
file. The login must be allowed to run DOLT_BACKUP.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runFederationBackup,
}

var (
	fedBackupDest         string
	fedBackupHost         string
	fedBackupPort         int
	fedBackupUser         string
	fedBackupRegistry     string
	fedBackupStores       []string
	fedBackupAllDatabases bool
	fedBackupSkipVerify   bool
	fedBackupScratch      string
	fedBackupKeep         bool
	fedBackupDoltBin      string
	fedBackupBDBin        string
	fedBackupJSON         bool
	fedBackupTimeout      time.Duration
)

func init() {
	f := brainFederationBackupCmd.Flags()
	f.StringVar(&fedBackupDest, "dest", "", "directory the backups go to, on the server's machine, outside every data directory (required)")
	f.StringVar(&fedBackupHost, "host", envOr("BEADS_DOLT_SERVER_HOST", "127.0.0.1"), "Dolt sql-server host")
	f.IntVar(&fedBackupPort, "port", envIntOr("BEADS_DOLT_SERVER_PORT", 3307), "Dolt sql-server port")
	f.StringVar(&fedBackupUser, "user", "", "login on the server (default BEADS_DOLT_SERVER_USER, then root)")
	f.StringVar(&fedBackupRegistry, "registry", "", "store registry to iterate (default ~/.config/brain/stores.yaml)")
	f.StringSliceVar(&fedBackupStores, "store", nil, "only these registered stores (repeatable; default: every store in the registry)")
	f.BoolVar(&fedBackupAllDatabases, "all-databases", false, "also back up databases on the server that no registered store names")
	f.BoolVar(&fedBackupSkipVerify, "skip-verify", false, "take the backups without proving them (the report says so)")
	f.StringVar(&fedBackupScratch, "scratch-dir", "", "where a backup is laid out on a Dolt server of its own; same filesystem as --dest so it is cloned, not copied (default: <dest>/.scratch, removed afterwards)")
	f.BoolVar(&fedBackupKeep, "keep-scratch", false, "keep the proof server's data directory afterwards")
	f.StringVar(&fedBackupDoltBin, "dolt-bin", "dolt", "dolt binary for the proof server")
	f.StringVar(&fedBackupBDBin, "bd-bin", "", "bd binary that runs 'bd list' against each copy (default: this binary)")
	f.BoolVar(&fedBackupJSON, "json", false, "print the report as JSON")
	f.DurationVar(&fedBackupTimeout, "timeout", 12*time.Hour, "overall time budget")
	brainCmd.AddCommand(brainFederationBackupCmd)
}

// isFederationBackupVerb reports whether cmd is 'brain federation-backup',
// which talks to the Dolt server directly and must never open the caller's
// store.
func isFederationBackupVerb(cmd *cobra.Command) bool {
	return cmd != nil && cmd == brainFederationBackupCmd
}

// fedStore is one registered store as the backup sees it.
type fedStore struct {
	Name     string `json:"store"`
	Path     string `json:"path"`
	Database string `json:"database,omitempty"`
	Global   string `json:"global_database,omitempty"`
	Port     int    `json:"port,omitempty"`
	// Status: ok, unreadable (metadata could not be read), elsewhere (the
	// metadata names another server port).
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	meta map[string]any
}

// fedState is what a backup must reproduce: the database hash, HEAD, every
// table's dolt_hashof_table, and the issues row count.
type fedState struct {
	DBHash  string            `json:"db_hash"`
	Head    string            `json:"head"`
	Tables  map[string]string `json:"tables"`
	Issues  int64             `json:"issues"`
	TakenAt time.Time         `json:"taken_at"`
}

// fedListCheck is one 'bd list' run against a backup's copy.
type fedListCheck struct {
	Store   string  `json:"store"`
	OK      bool    `json:"ok"`
	Rows    int     `json:"rows"`
	Seconds float64 `json:"seconds"`
	Error   string  `json:"error,omitempty"`
}

// fedVerify is the proof of one backup.
type fedVerify struct {
	Status        string         `json:"status"` // verified, failed, skipped
	ScratchDir    string         `json:"scratch_dir,omitempty"`
	LayoutSeconds float64        `json:"layout_seconds"`
	Port          int            `json:"port,omitempty"`
	Got           fedState       `json:"got"`
	HeadInHistory bool           `json:"head_in_history"`
	Diffs         []string       `json:"diffs,omitempty"`
	Lists         []fedListCheck `json:"lists,omitempty"`
	Seconds       float64        `json:"seconds"`
	Error         string         `json:"error,omitempty"`
}

// fedDatabase is one database the registry names (or, with --all-databases,
// one the server holds) and what happened to it.
type fedDatabase struct {
	Name      string   `json:"database"`
	Stores    []string `json:"stores"`
	GlobalFor []string `json:"global_for,omitempty"`
	// Status: verified, backed-up (not proved), absent (named by a store,
	// not on the server), failed.
	Status        string     `json:"status"`
	URL           string     `json:"url,omitempty"`
	Before        fedState   `json:"before"`
	HashAfter     string     `json:"hash_after,omitempty"`
	Changed       bool       `json:"changed_during_backup"`
	BackupSeconds float64    `json:"backup_seconds"`
	Bytes         int64      `json:"bytes"`
	Verify        *fedVerify `json:"verify,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// fedReport is the whole run.
type fedReport struct {
	Server       string        `json:"server"`
	BD           string        `json:"bd_version"`
	Registry     string        `json:"registry"`
	Dest         string        `json:"dest"`
	StartedAt    time.Time     `json:"started_at"`
	Seconds      float64       `json:"seconds"`
	Stores       []fedStore    `json:"stores"`
	Databases    []fedDatabase `json:"databases"`
	Unregistered []string      `json:"unregistered_databases,omitempty"`
	Warnings     []string      `json:"warnings,omitempty"`
	Failures     []string      `json:"failures,omitempty"`
	OK           bool          `json:"ok"`
	ReportPath   string        `json:"report_path,omitempty"`
}

func fedEndpoint() brainupgrade.Endpoint {
	user := fedBackupUser
	if user == "" {
		user = envOr("BEADS_DOLT_SERVER_USER", "root")
	}
	password := os.Getenv("BEADS_DOLT_PASSWORD")
	if password == "" {
		password = configfile.LookupCredentialsPassword(fedBackupHost, fedBackupPort)
	}
	return brainupgrade.Endpoint{Host: fedBackupHost, Port: fedBackupPort, User: user, Password: password}
}

// loadFederationRegistry reads the registry at path, or the default one.
func loadFederationRegistry(path string) (map[string]storeEntry, string, error) {
	if path == "" {
		path = storesYamlFile()
	}
	stores, found, err := readStoresFile(path)
	if err != nil {
		return nil, path, err
	}
	if !found {
		return nil, path, fmt.Errorf("no store registry at %s", path)
	}
	return stores, path, nil
}

// resolveFederationStores reads each registered store's metadata and reports
// the database (and global database) it names. only narrows the set; a name
// not in the registry is an error. port is the server this run backs up: a
// store whose metadata names another port is marked elsewhere.
func resolveFederationStores(registry map[string]storeEntry, only []string, port int) ([]fedStore, error) {
	names := make([]string, 0, len(registry))
	if len(only) > 0 {
		for _, n := range only {
			n = strings.TrimSpace(n)
			if _, ok := registry[n]; !ok {
				return nil, fmt.Errorf("store %q is not in the registry", n)
			}
			names = append(names, n)
		}
	} else {
		for n := range registry {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]fedStore, 0, len(names))
	for _, n := range names {
		st := fedStore{Name: n, Path: registry[n].Path, Status: "ok"}
		raw, err := os.ReadFile(configfile.ConfigPath(st.Path)) //nolint:gosec // registry-recorded path
		if err == nil {
			err = json.Unmarshal(raw, &st.meta)
		}
		var cfg *configfile.Config
		if err == nil {
			cfg, err = configfile.Load(st.Path)
		}
		if err != nil {
			st.Status, st.Error = "unreadable", err.Error()
			out = append(out, st)
			continue
		}
		st.Database = cfg.GetDoltDatabase()
		st.Global = cfg.GetGlobalDoltDatabase()
		st.Port = cfg.GetDoltServerPort()
		if st.Port != 0 && st.Port != port {
			st.Status = "elsewhere"
			st.Error = fmt.Sprintf("metadata names port %d, not %d", st.Port, port)
		}
		out = append(out, st)
	}
	return out, nil
}

// planFederationDatabases collects the databases the ok stores name, each
// once, sorted, with the stores that live in it and the stores it is the
// global database for.
func planFederationDatabases(stores []fedStore) []fedDatabase {
	byName := map[string]*fedDatabase{}
	get := func(name string) *fedDatabase {
		d, ok := byName[name]
		if !ok {
			d = &fedDatabase{Name: name}
			byName[name] = d
		}
		return d
	}
	for _, st := range stores {
		if st.Status != "ok" {
			continue
		}
		if st.Database != "" {
			d := get(st.Database)
			d.Stores = append(d.Stores, st.Name)
		}
		if st.Global != "" && st.Global != st.Database {
			d := get(st.Global)
			d.GlobalFor = append(d.GlobalFor, st.Name)
		}
	}
	out := make([]fedDatabase, 0, len(byName))
	for _, d := range byName {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// checkFederationDest refuses a destination inside a Dolt database or data
// directory (the server would serve or overwrite it, and a backup written
// into the live data directory is the 2026-08-04 mistake in another form),
// or inside a registered store's directory.
func checkFederationDest(dest string, storePaths []string) error {
	if dest == "" {
		return errors.New("--dest is required")
	}
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("--dest %s must be an absolute path", dest)
	}
	dest = filepath.Clean(dest)
	if dest == string(filepath.Separator) {
		return errors.New("--dest must not be the filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil && dest == filepath.Clean(home) {
		return errors.New("--dest must not be the home directory itself")
	}
	if err := checkFederationWritePath("--dest", dest, storePaths); err != nil {
		return err
	}
	if entries, err := os.ReadDir(dest); err == nil {
		for _, e := range entries {
			if e.IsDir() && isDoltDatabaseDir(filepath.Join(dest, e.Name())) {
				return fmt.Errorf("--dest %s is a Dolt sql-server data directory (it holds the database %s): a backup must go outside every data directory", dest, e.Name())
			}
		}
	}
	return nil
}

func checkFederationWritePath(label, path string, storePaths []string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolving %s %s: %w", label, path, err)
	}
	path = filepath.Clean(path)
	resolved, err := resolveFederationPath(path)
	if err != nil {
		return fmt.Errorf("resolving %s %s: %w", label, path, err)
	}
	for _, candidate := range []string{path, resolved} {
		for dir := candidate; ; dir = filepath.Dir(dir) {
			if isDoltDatabaseDir(dir) {
				return fmt.Errorf("%s %s is inside the Dolt database %s: a backup must go outside every database and data directory", label, path, dir)
			}
			if _, err := os.Lstat(filepath.Join(dir, ".doltcfg")); err == nil {
				return fmt.Errorf("%s %s is inside %s, a Dolt sql-server data directory (.doltcfg present): a backup must go outside every data directory", label, path, dir)
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	for _, p := range storePaths {
		p, err = resolveFederationPath(p)
		if err != nil {
			return fmt.Errorf("resolving registered store %s: %w", p, err)
		}
		for _, root := range []string{p, filepath.Dir(p)} {
			if root == "" || root == string(filepath.Separator) {
				continue
			}
			for _, candidate := range []string{path, resolved} {
				if candidate == root || strings.HasPrefix(candidate, root+string(filepath.Separator)) {
					return fmt.Errorf("%s %s is inside the registered store directory %s", label, path, root)
				}
			}
		}
	}
	return nil
}

func checkFederationBackupTarget(dest, database string, storePaths []string) error {
	target := filepath.Join(dest, database)
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup target %s must not be a symlink", target)
	}
	return checkFederationWritePath("backup target", target, storePaths)
}

func resolveFederationPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	for existing := path; ; existing = filepath.Dir(existing) {
		if resolved, err := filepath.EvalSymlinks(existing); err == nil {
			rel, err := filepath.Rel(existing, path)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, rel), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(existing) == existing {
			return "", fmt.Errorf("no existing ancestor")
		}
	}
}

// isDoltDatabaseDir reports whether dir is a Dolt database (.dolt/noms).
func isDoltDatabaseDir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".dolt", "noms"))
	return err == nil
}

// fedCapture reads the state a backup must reproduce from the database conn
// has selected. Reads only.
func fedCapture(ctx context.Context, conn *sql.Conn) (fedState, error) {
	st := fedState{Tables: map[string]string{}, Issues: -1, TakenAt: time.Now().UTC()}
	name, hash, err := brainupgrade.Identity(ctx, conn)
	if err != nil {
		return st, err
	}
	st.DBHash = hash
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&st.Head); err != nil {
		return st, fmt.Errorf("reading HEAD of %s: %w", name, err)
	}
	tables, err := brainupgrade.ListTables(ctx, conn)
	if err != nil {
		return st, err
	}
	for _, t := range tables {
		var h string
		if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF_TABLE(?)", t).Scan(&h); err != nil {
			return st, fmt.Errorf("hashing table %s of %s: %w", t, name, err)
		}
		st.Tables[t] = h
		if t == "issues" {
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM `issues`").Scan(&st.Issues); err != nil {
				return st, fmt.Errorf("counting issues of %s: %w", name, err)
			}
		}
	}
	return st, nil
}

// compareFedState lists how got fails to reproduce want. With changed false
// (nothing wrote while the backup ran) the two must be identical. With
// changed true the backup holds a state between the two readings: the
// pre-sync HEAD must be in its history (headInHistory) and no table may be
// missing.
func compareFedState(want, got fedState, changed, headInHistory bool) []string {
	var diffs []string
	if changed {
		if !headInHistory {
			diffs = append(diffs, fmt.Sprintf("pre-sync HEAD %s is not in the backup's history (backup HEAD %s)", want.Head, got.Head))
		}
	} else {
		if want.DBHash != got.DBHash {
			diffs = append(diffs, fmt.Sprintf("dolt_hashof_db %s, want %s", got.DBHash, want.DBHash))
		}
		if want.Head != got.Head {
			diffs = append(diffs, fmt.Sprintf("HEAD %s, want %s", got.Head, want.Head))
		}
		if want.Issues != got.Issues {
			diffs = append(diffs, fmt.Sprintf("issues rows %d, want %d", got.Issues, want.Issues))
		}
	}
	names := make([]string, 0, len(want.Tables))
	for t := range want.Tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		gh, ok := got.Tables[t]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("table %s missing", t))
			continue
		}
		if !changed && gh != want.Tables[t] {
			diffs = append(diffs, fmt.Sprintf("table %s: dolt_hashof_table %s, want %s", t, gh, want.Tables[t]))
		}
	}
	if !changed {
		extra := make([]string, 0)
		for t := range got.Tables {
			if _, ok := want.Tables[t]; !ok {
				extra = append(extra, t)
			}
		}
		sort.Strings(extra)
		for _, t := range extra {
			diffs = append(diffs, fmt.Sprintf("table %s not in the backed-up state", t))
		}
	}
	return diffs
}

// fedBackupDatabase asks the server for a backup of d and records the state
// it holds. The pre-sync state is read first; the hash is read again after,
// so a write during the sync is known (Changed) and the proof adapts.
func fedBackupDatabase(ctx context.Context, ep brainupgrade.Endpoint, dest string, storePaths []string, d *fedDatabase) error {
	if err := checkFederationBackupTarget(dest, d.Name, storePaths); err != nil {
		return err
	}
	url, err := brainupgrade.BackupURL(dest, d.Name)
	if err != nil {
		return err
	}
	d.URL = url
	dir := filepath.Join(dest, d.Name)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		if _, err := os.Stat(filepath.Join(dir, "manifest")); err != nil {
			return fmt.Errorf("%s exists and is not a Dolt backup (no manifest): refusing to sync into it", dir)
		}
	}
	c, err := ep.Conn(ctx, d.Name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if d.Before, err = fedCapture(ctx, c.Conn); err != nil {
		return err
	}
	start := time.Now()
	if err := schema.DrainCall(ctx, c.Conn, "CALL DOLT_BACKUP('sync-url', ?)", url); err != nil {
		return fmt.Errorf("backing %s up to %s: %w", d.Name, url, err)
	}
	d.BackupSeconds = time.Since(start).Seconds()
	if _, d.HashAfter, err = brainupgrade.Identity(ctx, c.Conn); err != nil {
		return err
	}
	d.Changed = d.HashAfter != d.Before.DBHash
	// The server wrote the backup; seeing it here is what proves this
	// command runs where the backup can be opened.
	if _, err := os.Stat(filepath.Join(dir, "manifest")); err != nil {
		return fmt.Errorf("the server reports a backup of %s at %s, but it is not visible here (%w): run this on the server's machine", d.Name, url, err)
	}
	d.Bytes = hostDirSize(dir)
	return nil
}

// fedScratchDir is a fresh directory for one proof server; done removes it
// unless --keep-scratch.
func fedScratchDir(base, purpose string) (dir string, done func()) {
	dir = filepath.Join(base, fmt.Sprintf("%s-%d", purpose, time.Now().UnixNano()))
	return dir, func() {
		if !fedBackupKeep {
			_ = os.RemoveAll(dir)
		}
	}
}

// fedVerifyDatabase proves d's backup: lays it out on a Dolt server of its
// own, compares its state with the pre-sync state, and runs 'bd list' for
// every store that lives in the database.
func fedVerifyDatabase(ctx context.Context, scratchBase, bdBin string, d *fedDatabase, stores []fedStore) *fedVerify {
	v := &fedVerify{Status: "failed", Got: fedState{Tables: map[string]string{}, Issues: -1}}
	start := time.Now()
	defer func() { v.Seconds = time.Since(start).Seconds() }()
	dir, done := fedScratchDir(scratchBase, "verify-"+d.Name)
	defer done()
	v.ScratchDir = dir
	dataDir := filepath.Join(dir, "data")
	layout := time.Now()
	if err := brainupgrade.Materialize(brainupgrade.Backup{Database: d.Name, URL: d.URL}, filepath.Join(dataDir, d.Name)); err != nil {
		v.Error = err.Error()
		return v
	}
	v.LayoutSeconds = time.Since(layout).Seconds()
	srv, err := brainupgrade.StartServer(ctx, fedBackupDoltBin, dataDir)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	defer func() { _ = srv.Stop() }()
	v.Port = srv.Port
	ep := srv.Endpoint()
	c, err := ep.Conn(ctx, d.Name)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Got, err = fedCapture(ctx, c.Conn)
	if err == nil && d.Changed {
		var n int
		err = c.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = ?", d.Before.Head).Scan(&n)
		v.HeadInHistory = n > 0
	} else if err == nil {
		v.HeadInHistory = v.Got.Head == d.Before.Head
	}
	_ = c.Close()
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Diffs = compareFedState(d.Before, v.Got, d.Changed, v.HeadInHistory)
	listsOK := true
	for _, name := range d.Stores {
		st, ok := findFedStore(stores, name)
		if !ok {
			continue
		}
		check := fedRunList(ctx, bdBin, filepath.Join(dir, "stores", st.Name), st, ep.Port, d.Name)
		if !check.OK {
			listsOK = false
		}
		v.Lists = append(v.Lists, check)
	}
	if len(v.Diffs) == 0 && listsOK {
		v.Status = "verified"
	}
	return v
}

func findFedStore(stores []fedStore, name string) (fedStore, bool) {
	for _, st := range stores {
		if st.Name == name {
			return st, true
		}
	}
	return fedStore{}, false
}

// fedRunList runs 'bd list --json --limit 1' for st against the copy of
// database on the proof server at port, the way the store's wrapper would:
// a .beads directory holding the store's own metadata re-pointed at the
// proof server, BD_NAME set, server-mode env set, every other BEADS_, BD_
// and BRAIN_ variable dropped so nothing reaches the real store.
func fedRunList(ctx context.Context, bdBin, storeDir string, st fedStore, port int, database string) fedListCheck {
	check := fedListCheck{Store: st.Name}
	beads := filepath.Join(storeDir, ".beads")
	if err := os.MkdirAll(beads, 0o700); err != nil {
		check.Error = err.Error()
		return check
	}
	meta := map[string]any{}
	for k, val := range st.meta {
		meta[k] = val
	}
	meta["database"] = "dolt"
	meta["backend"] = "dolt"
	meta["dolt_mode"] = "server"
	meta["dolt_server_host"] = "127.0.0.1"
	meta["dolt_server_port"] = port
	meta["dolt_database"] = database
	delete(meta, "global_dolt_database")
	delete(meta, "global_project_id")
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err == nil {
		err = os.WriteFile(configfile.ConfigPath(beads), append(raw, '\n'), 0o600)
	}
	if err != nil {
		check.Error = err.Error()
		return check
	}
	if cfg, err := os.ReadFile(filepath.Join(st.Path, "config.yaml")); err == nil { //nolint:gosec // the store's own config
		if err := os.WriteFile(filepath.Join(beads, "config.yaml"), cfg, 0o600); err != nil {
			check.Error = err.Error()
			return check
		}
	}
	env := make([]string, 0, len(os.Environ())+10)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BEADS_") || strings.HasPrefix(kv, "BD_") || strings.HasPrefix(kv, "BRAIN_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"BEADS_DIR="+beads,
		"BD_NAME="+st.Name,
		"BEADS_DOLT_SERVER_MODE=1",
		"BEADS_DOLT_SHARED_SERVER=1",
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BEADS_DOLT_SERVER_PORT="+strconv.Itoa(port),
		"BEADS_DOLT_SERVER_USER=root",
		"BEADS_DOLT_PASSWORD=",
		"BEADS_CREDENTIALS_FILE="+filepath.Join(storeDir, "no-credentials"),
		"BD_DISABLE_METRICS=1",
	)
	cmd := exec.CommandContext(ctx, bdBin, "list", "--json", "--limit", "1") //nolint:gosec // this binary, or --bd-bin
	cmd.Dir = storeDir
	cmd.Env = env
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	start := time.Now()
	err = cmd.Run()
	check.Seconds = time.Since(start).Seconds()
	if err != nil {
		check.Error = fmt.Sprintf("bd list: %v: %s", err, lastLines(stderr.String(), 3))
		return check
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &rows); err != nil {
		check.Error = fmt.Sprintf("bd list printed no JSON list: %s", lastLines(out.String(), 2))
		return check
	}
	check.OK, check.Rows = true, len(rows)
	return check
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// fedShowDatabases is the one read on the server before any backup: SHOW
// DATABASES, minus the system schemas.
func fedShowDatabases(ctx context.Context, ep brainupgrade.Endpoint) ([]string, error) {
	db, err := ep.Open(ctx, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("SHOW DATABASES on %s: %w", ep, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		switch name {
		case "information_schema", "mysql", "performance_schema", "sys", "dolt_cluster":
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func runFederationBackup(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), fedBackupTimeout)
	defer cancel()
	out := cmd.ErrOrStderr()
	rep := fedReport{BD: combinedVersion(), StartedAt: time.Now().UTC()}
	defer func() { rep.Seconds = time.Since(rep.StartedAt).Seconds() }()

	registry, regPath, err := loadFederationRegistry(fedBackupRegistry)
	if err != nil {
		return err
	}
	rep.Registry = regPath
	if len(registry) == 0 {
		return fmt.Errorf("the registry at %s has no stores", regPath)
	}
	rep.Stores, err = resolveFederationStores(registry, fedBackupStores, fedBackupPort)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(rep.Stores))
	for _, st := range rep.Stores {
		paths = append(paths, st.Path)
	}
	if err := checkFederationDest(fedBackupDest, paths); err != nil {
		return err
	}
	rep.Dest = filepath.Clean(fedBackupDest)
	if err := os.MkdirAll(rep.Dest, 0o700); err != nil {
		return err
	}
	scratchBase := fedBackupScratch
	if scratchBase == "" {
		scratchBase = filepath.Join(rep.Dest, ".scratch")
	}
	if err := checkFederationWritePath("--scratch-dir", scratchBase, paths); err != nil {
		return err
	}
	bdBin := fedBackupBDBin
	if bdBin == "" {
		if bdBin, err = os.Executable(); err != nil {
			return fmt.Errorf("finding this binary for the proof: %w", err)
		}
	}

	ep := fedEndpoint()
	rep.Server = ep.String()
	for _, st := range rep.Stores {
		switch st.Status {
		case "unreadable":
			rep.Failures = append(rep.Failures, fmt.Sprintf("store %s: metadata unreadable (%s): its database is unknown, so this run cannot claim to cover it", st.Name, st.Error))
		case "elsewhere":
			rep.Failures = append(rep.Failures, fmt.Sprintf("store %s: %s: its database is not on this server; back that server up too, or scope this run with --store", st.Name, st.Error))
		}
	}
	rep.Databases = planFederationDatabases(rep.Stores)
	ok := 0
	for _, st := range rep.Stores {
		if st.Status == "ok" {
			ok++
		}
	}
	_, _ = fmt.Fprintf(out, "federation backup: %d registered stores, %d on %s, %d databases named; backups go to %s\n", len(rep.Stores), ok, rep.Server, len(rep.Databases), rep.Dest)

	present, err := fedShowDatabases(ctx, ep)
	if err != nil {
		return err
	}
	onServer := map[string]bool{}
	for _, name := range present {
		onServer[name] = true
	}
	named := map[string]bool{}
	for _, d := range rep.Databases {
		named[d.Name] = true
	}
	for _, name := range present {
		if !named[name] {
			rep.Unregistered = append(rep.Unregistered, name)
			if fedBackupAllDatabases {
				rep.Databases = append(rep.Databases, fedDatabase{Name: name})
			}
		}
	}
	if len(rep.Unregistered) > 0 && !fedBackupAllDatabases {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("on the server but named by no registered store, not backed up (pass --all-databases to include): %s", strings.Join(rep.Unregistered, ", ")))
	}

	for i := range rep.Databases {
		d := &rep.Databases[i]
		who := describeFedStores(d)
		if !onServer[d.Name] {
			d.Status = "absent"
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("database %s is named by %s but is not on the server: nothing to back up; fix the metadata or create the database", d.Name, who))
			_, _ = fmt.Fprintf(out, "%s: ABSENT on the server (named by %s)\n", d.Name, who)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s: backing up (%s) to %s\n", d.Name, who, filepath.Join(rep.Dest, d.Name))
		if err := fedBackupDatabase(ctx, ep, rep.Dest, paths, d); err != nil {
			d.Status, d.Error = "failed", err.Error()
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: %s", d.Name, err))
			_, _ = fmt.Fprintf(out, "%s: FAILED: %v\n", d.Name, err)
			continue
		}
		wrote := "no writes during the backup"
		if d.Changed {
			wrote = "the database changed while the backup ran"
		}
		_, _ = fmt.Fprintf(out, "%s: backed up in %.1fs (%s)  HEAD %s  hash %s  %s\n", d.Name, d.BackupSeconds, humanBytes(d.Bytes), d.Before.Head, d.Before.DBHash, wrote)
		if fedBackupSkipVerify {
			d.Status = "backed-up"
			d.Verify = &fedVerify{Status: "skipped"}
			continue
		}
		_, _ = fmt.Fprintf(out, "%s: proving: laying the backup out on a Dolt server of its own and running bd list for %d store(s)\n", d.Name, len(d.Stores))
		d.Verify = fedVerifyDatabase(ctx, scratchBase, bdBin, d, rep.Stores)
		if d.Verify.Status == "verified" {
			d.Status = "verified"
			_, _ = fmt.Fprintf(out, "%s: VERIFIED in %.1fs: %s; bd list OK for %d/%d stores%s\n", d.Name, d.Verify.Seconds, fedProofWords(d), len(d.Verify.Lists), len(d.Stores), fedListRange(d.Verify.Lists))
			continue
		}
		d.Status = "failed"
		msg := d.Verify.Error
		if msg == "" {
			msg = strings.Join(append(append([]string{}, d.Verify.Diffs...), fedListFailures(d.Verify.Lists)...), "; ")
		}
		d.Error = "backup did not verify: " + msg
		rep.Failures = append(rep.Failures, fmt.Sprintf("%s: %s", d.Name, d.Error))
		_, _ = fmt.Fprintf(out, "%s: NOT VERIFIED: %s\n", d.Name, msg)
	}
	rep.OK = len(rep.Failures) == 0
	rep.Seconds = time.Since(rep.StartedAt).Seconds()

	if path, err := writeFedReport(rep.Dest, &rep); err != nil {
		rep.Failures = append(rep.Failures, "writing the report: "+err.Error())
		rep.OK = false
	} else {
		rep.ReportPath = path
	}
	if fedBackupJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		writeFedSummary(cmd.OutOrStdout(), &rep)
	}
	if !rep.OK {
		return fmt.Errorf("the federation backup did not complete: %d failure(s); see the report%s", len(rep.Failures), reportSuffix(rep.ReportPath))
	}
	return nil
}

func reportSuffix(path string) string {
	if path == "" {
		return ""
	}
	return " at " + path
}

func describeFedStores(d *fedDatabase) string {
	var parts []string
	if n := len(d.Stores); n == 1 {
		parts = append(parts, "store "+d.Stores[0])
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d stores", n))
	}
	if n := len(d.GlobalFor); n > 0 {
		parts = append(parts, fmt.Sprintf("global database of %d store(s)", n))
	}
	if len(parts) == 0 {
		return "no registered store"
	}
	return strings.Join(parts, ", ")
}

func fedProofWords(d *fedDatabase) string {
	if d.Changed {
		return "pre-sync HEAD in the backup's history, every table present"
	}
	return fmt.Sprintf("hash, HEAD and %d table hashes identical", len(d.Before.Tables))
}

func fedListRange(lists []fedListCheck) string {
	if len(lists) == 0 {
		return ""
	}
	lo, hi := lists[0].Seconds, lists[0].Seconds
	for _, l := range lists[1:] {
		if l.Seconds < lo {
			lo = l.Seconds
		}
		if l.Seconds > hi {
			hi = l.Seconds
		}
	}
	return fmt.Sprintf(" (%.2f–%.2fs each)", lo, hi)
}

func fedListFailures(lists []fedListCheck) []string {
	var out []string
	for _, l := range lists {
		if !l.OK {
			out = append(out, fmt.Sprintf("bd list for %s: %s", l.Store, l.Error))
		}
	}
	return out
}

// writeFedReport records the run beside the backups (never inside one):
// <dest>/reports/<stamp>-federation-backup.json, and the same at
// <dest>/federation-backup-latest.json.
func writeFedReport(dest string, rep *fedReport) (string, error) {
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	path := filepath.Join(dest, "reports", rep.StartedAt.Format("20060102T150405Z")+"-federation-backup.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, os.WriteFile(filepath.Join(dest, "federation-backup-latest.json"), raw, 0o600)
}

func writeFedSummary(w io.Writer, rep *fedReport) {
	verdict := "OK"
	if !rep.OK {
		verdict = "FAILED"
	}
	_, _ = fmt.Fprintf(w, "federation backup %s  %s  bd %s  %.1fs\n", verdict, rep.Server, rep.BD, rep.Seconds)
	_, _ = fmt.Fprintf(w, "%-16s %-10s %10s %8s %8s  %s\n", "DATABASE", "STATUS", "SIZE", "BACKUP", "PROOF", "STORES")
	for _, d := range rep.Databases {
		proof := "-"
		if d.Verify != nil && d.Verify.Status != "skipped" {
			proof = fmt.Sprintf("%.1fs", d.Verify.Seconds)
		}
		backup := "-"
		if d.Status != "absent" && d.Error == "" || d.BackupSeconds > 0 {
			backup = fmt.Sprintf("%.1fs", d.BackupSeconds)
		}
		size := "-"
		if d.Bytes > 0 {
			size = humanBytes(d.Bytes)
		}
		_, _ = fmt.Fprintf(w, "%-16s %-10s %10s %8s %8s  %s\n", d.Name, d.Status, size, backup, proof, describeFedStores(&d))
		if d.Verify != nil {
			for _, l := range d.Verify.Lists {
				mark := "ok"
				if !l.OK {
					mark = "FAILED " + l.Error
				}
				_, _ = fmt.Fprintf(w, "    bd list %-20s %6.2fs  %s\n", l.Store, l.Seconds, mark)
			}
		}
	}
	for _, wmsg := range rep.Warnings {
		_, _ = fmt.Fprintf(w, "warning: %s\n", wmsg)
	}
	for _, f := range rep.Failures {
		_, _ = fmt.Fprintf(w, "failure: %s\n", f)
	}
	if rep.ReportPath != "" {
		_, _ = fmt.Fprintf(w, "report: %s\n", rep.ReportPath)
	}
}
