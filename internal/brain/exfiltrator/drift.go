package exfiltrator

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DirtyJournalFilename is the journal, under `<root>/entries/`, where a
// failed Render/Remove records the issue id whose markdown is now stale.
// The reconciler drains it; `bd brain stores doctor` reports its size.
const DirtyJournalFilename = ".dirty.jsonl"

// DriftRecorder is the optional seam the storage decorator uses to
// journal a swallowed Render/Remove failure.
type DriftRecorder interface {
	RecordDrift(issueID, op string, cause error) error
}

type dirtyEntry struct {
	ID    string `json:"id"`
	Op    string `json:"op"`
	Error string `json:"error"`
	At    string `json:"at"`
}

// DirtyJournalPath returns the journal path for a render root.
func DirtyJournalPath(root string) string {
	return filepath.Join(root, "entries", DirtyJournalFilename)
}

// RecordDrift appends one line to the dirty journal.
func (m *MarkdownExfiltrator) RecordDrift(issueID, op string, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := DirtyJournalPath(m.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	e := dirtyEntry{ID: issueID, Op: op, At: time.Now().UTC().Format(time.RFC3339)}
	if cause != nil {
		e.Error = cause.Error()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// DirtyIDs returns the distinct, sorted issue ids in the journal under
// root. A missing journal is an empty result, not an error.
func DirtyIDs(root string) ([]string, error) {
	f, err := os.Open(DirtyJournalPath(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e dirtyEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			seen[e.ID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, sc.Err()
}

// DrainDirty returns the dirty ids and truncates the journal; the
// reconciler calls it before re-rendering them.
func DrainDirty(root string) ([]string, error) {
	ids, err := DirtyIDs(root)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(DirtyJournalPath(root)); err != nil && !os.IsNotExist(err) {
		return ids, err
	}
	return ids, nil
}
