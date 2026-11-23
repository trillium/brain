package exfiltrator

import (
	"errors"
	"testing"
)

func TestDirtyJournalResolvesOnlySuccessfullyRepairedRecords(t *testing.T) {
	root := t.TempDir()
	m := NewMarkdownExfiltrator(root, nil)
	if ids, err := DirtyIDs(root); err != nil || len(ids) != 0 {
		t.Fatalf("empty journal: ids=%v err=%v", ids, err)
	}
	var records []DirtyRecord
	for _, id := range []string{"b-2", "a-1", "b-2"} {
		record := DirtyRecord{ID: id, Op: "render", Error: errors.New("boom").Error()}
		if err := m.RecordDrift(record); err != nil {
			t.Fatal(err)
		}
		records, _ = DirtyRecords(root)
	}
	if err := m.ResolveDirty([]DirtyRecord{records[0], records[2]}); err != nil {
		t.Fatal(err)
	}
	remaining, err := DirtyRecords(root)
	if err != nil || len(remaining) != 1 || remaining[0].ID != "a-1" {
		t.Fatalf("remaining = %#v, err=%v", remaining, err)
	}
}
