package exfiltrator

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/steveyegge/beads/internal/types"
)

// DirtyJournalFilename is the journal, under `<root>/entries/`, where a
// failed Render/Remove records the issue id whose markdown is now stale.
// The reconciler drains it; `bd brain stores doctor` reports its size.
const DirtyJournalFilename = ".dirty.jsonl"

// DriftRecorder is the optional seam the storage decorator uses to
// journal a swallowed Render/Remove failure.
type DriftRecorder interface {
	RecordDrift(record DirtyRecord) error
}

// DirtyRecord identifies one failed exfiltration operation and, for removals,
// the exact file target that a later reconciliation must remove.
type DirtyRecord struct {
	ID    string          `json:"id"`
	Op    string          `json:"op"`
	Kind  types.IssueType `json:"kind,omitempty"`
	Slug  string          `json:"slug,omitempty"`
	Error string          `json:"error"`
	At    string          `json:"at"`
}

// DirtyJournalPath returns the journal path for a render root.
func DirtyJournalPath(root string) string {
	return filepath.Join(root, "entries", DirtyJournalFilename)
}

// RecordDrift appends one line to the dirty journal.
func (m *MarkdownExfiltrator) RecordDrift(record DirtyRecord) error {
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
	record.At = time.Now().UTC().Format(time.RFC3339Nano)
	if record.Error == "" {
		record.Error = "exfiltration failed"
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// DirtyRecords returns the recorded exfiltration failures under root. A
// missing journal is an empty result, not an error.
func DirtyRecords(root string) ([]DirtyRecord, error) {
	f, err := os.Open(DirtyJournalPath(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var records []DirtyRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var record DirtyRecord
		if json.Unmarshal(sc.Bytes(), &record) == nil && record.ID != "" {
			records = append(records, record)
		}
	}
	return records, sc.Err()
}

// DirtyIDs returns the distinct, sorted issue ids in the journal under root.
func DirtyIDs(root string) ([]string, error) {
	records, err := DirtyRecords(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, record := range records {
		seen[record.ID] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// ResolveDirty removes exactly the successfully repaired records while
// preserving failures and records appended concurrently by another mutation.
func (m *MarkdownExfiltrator) ResolveDirty(resolved []DirtyRecord) error {
	if len(resolved) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := DirtyRecords(m.root)
	if err != nil {
		return err
	}
	resolvedSet := make(map[DirtyRecord]int, len(resolved))
	for _, record := range resolved {
		resolvedSet[record]++
	}
	remaining := make([]DirtyRecord, 0, len(records))
	for _, record := range records {
		if resolvedSet[record] == 0 {
			remaining = append(remaining, record)
			continue
		}
		resolvedSet[record]--
	}
	path := DirtyJournalPath(m.root)
	if len(remaining) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dirty-*.jsonl")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	for _, record := range remaining {
		line, err := json.Marshal(record)
		if err != nil {
			tmp.Close()
			return err
		}
		if _, err := tmp.Write(append(line, '\n')); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
