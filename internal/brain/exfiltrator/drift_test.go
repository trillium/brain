package exfiltrator

import (
	"errors"
	"testing"
)

func TestDirtyJournalRecordDirtyDrain(t *testing.T) {
	root := t.TempDir()
	m := NewMarkdownExfiltrator(root, nil)
	if ids, err := DirtyIDs(root); err != nil || len(ids) != 0 {
		t.Fatalf("empty journal: ids=%v err=%v", ids, err)
	}
	for _, id := range []string{"b-2", "a-1", "b-2"} {
		if err := m.RecordDrift(id, "render", errors.New("boom")); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := DrainDirty(root)
	if err != nil || len(ids) != 2 || ids[0] != "a-1" || ids[1] != "b-2" {
		t.Fatalf("drain: ids=%v err=%v", ids, err)
	}
	if ids, _ := DirtyIDs(root); len(ids) != 0 {
		t.Fatalf("journal not cleared: %v", ids)
	}
}
