package brainupgrade

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// A Dolt backup written by DOLT_BACKUP('sync-url', 'file://…') is a chunk
// store in the same layout as a database's .dolt/noms: a manifest, table
// files and oldgen. Laying it out under a database directory opens it as that
// database, branches and working set included, with no restore.
//
// That matters at brain's size. DOLT_BACKUP('restore') copies the files in a
// minute and then checks every chunk's references across the whole history,
// which on a 13 GB database runs for hours (Dolt 2.2.3). Materializing takes
// as long as cloning the files; the comparison that follows (dolt_hashof_db,
// HEAD, the commit count and every table digest) is what proves the result.

// backupDir resolves a file:// backup URL to its directory.
func backupDir(backupURL string) (string, error) {
	u, err := url.Parse(backupURL)
	if err != nil {
		return "", fmt.Errorf("parsing backup url %q: %w", backupURL, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("backup url %q: only file:// backups can be laid out as a database", backupURL)
	}
	return filepath.FromSlash(u.Path), nil
}

// Materialize lays the backup b out as a Dolt database directory at dbDir
// (dbDir/.dolt/noms holds the backup's chunk store). Files are cloned
// copy-on-write where the filesystem can (APFS), else copied, so the backup
// itself is never opened for writing. dbDir must not exist.
func Materialize(b Backup, dbDir string) error {
	src, err := backupDir(b.URL)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(src, "manifest")); err != nil {
		return fmt.Errorf("%s is not a Dolt backup (no manifest): %w", src, err)
	}
	if _, err := os.Stat(dbDir); err == nil {
		return fmt.Errorf("%s already exists", dbDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	doltDir := filepath.Join(dbDir, ".dolt")
	if err := os.MkdirAll(doltDir, 0o750); err != nil {
		return err
	}
	if err := cloneTree(src, filepath.Join(doltDir, "noms")); err != nil {
		return fmt.Errorf("laying out the backup of %s: %w", b.Database, err)
	}
	for name, body := range map[string]string{
		"repo_state.json": "{\n  \"head\": \"refs/heads/main\",\n  \"remotes\": {},\n  \"backups\": {},\n  \"branches\": {}\n}\n",
		"config.json":     "{}\n",
	} {
		if err := os.WriteFile(filepath.Join(doltDir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// cloneTree recreates the tree at src under dst, file by file.
func cloneTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return cloneFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // a file inside the operator's backup
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // inside a scratch or data dir the operator named
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// OfflineRollback puts the backup b in place of its database directory under
// dataDir, which the Dolt server serving it must not be running on. The
// current directory is kept beside it as <db>.migrated-<UTC stamp>, and its
// repository config (.dolt/repo_state.json and config.json: the remotes and
// backup registrations, which a backup does not carry) is carried over. It
// returns where the replaced directory went.
func OfflineRollback(dataDir string, b Backup) (string, error) {
	dbDir := filepath.Join(dataDir, b.Database)
	if _, err := os.Stat(filepath.Join(dbDir, ".dolt")); err != nil {
		return "", fmt.Errorf("%s is not a Dolt database directory: %w", dbDir, err)
	}
	aside := fmt.Sprintf("%s.migrated-%s", dbDir, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(dbDir, aside); err != nil {
		return "", fmt.Errorf("moving %s aside: %w", dbDir, err)
	}
	if err := Materialize(b, dbDir); err != nil {
		// Put the database back the way it was; the operator decides from there.
		_ = os.RemoveAll(dbDir)
		if rerr := os.Rename(aside, dbDir); rerr != nil {
			return aside, errors.Join(err, fmt.Errorf("moving %s back: %w", aside, rerr))
		}
		return "", err
	}
	for _, name := range []string{"repo_state.json", "config.json"} {
		from := filepath.Join(aside, ".dolt", name)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		raw, err := os.ReadFile(from) //nolint:gosec // inside the data dir the operator named
		if err != nil {
			return aside, err
		}
		if err := os.WriteFile(filepath.Join(dbDir, ".dolt", name), raw, 0o600); err != nil {
			return aside, err
		}
	}
	return aside, nil
}
