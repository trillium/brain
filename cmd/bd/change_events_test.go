package main

import (
	"reflect"
	"testing"
)

// TestRecordChangedID_DedupAndOrder locks the accumulator behavior the
// change-event fix relies on (robots-bnn): first-seen order preserved, exact
// duplicates ignored, empty ids skipped.
func TestRecordChangedID_DedupAndOrder(t *testing.T) {
	resetChangedIDs()
	t.Cleanup(resetChangedIDs)

	recordChangedID("a-1")
	recordChangedID("") // ignored
	recordChangedID("a-2")
	recordChangedID("a-1") // dup ignored
	recordChangedID("a-3")
	recordChangedID("a-2") // dup ignored

	want := []string{"a-1", "a-2", "a-3"}
	if !reflect.DeepEqual(commandChangedIDs, want) {
		t.Fatalf("commandChangedIDs = %v, want %v", commandChangedIDs, want)
	}
}

// TestResetChangedIDs_ClearsAccumulator ensures ids never leak across commands
// sharing a process.
func TestResetChangedIDs_ClearsAccumulator(t *testing.T) {
	resetChangedIDs()
	t.Cleanup(resetChangedIDs)

	recordChangedID("b-1")
	recordChangedID("b-2")
	if len(commandChangedIDs) != 2 {
		t.Fatalf("precondition: expected 2 ids, got %d", len(commandChangedIDs))
	}

	resetChangedIDs()
	if len(commandChangedIDs) != 0 {
		t.Fatalf("after reset: expected 0 ids, got %d (%v)", len(commandChangedIDs), commandChangedIDs)
	}
}

// TestSetLastTouchedID_FeedsAccumulator verifies the single choke point that
// makes every existing write path record its id automatically. SetLastTouchedID
// writes a file under FindBeadsDir(); with no workspace it no-ops the file write
// but must still populate the accumulator.
func TestSetLastTouchedID_FeedsAccumulator(t *testing.T) {
	resetChangedIDs()
	t.Cleanup(resetChangedIDs)

	SetLastTouchedID("c-1")
	SetLastTouchedID("c-1") // dup
	SetLastTouchedID("c-2")

	want := []string{"c-1", "c-2"}
	if !reflect.DeepEqual(commandChangedIDs, want) {
		t.Fatalf("after SetLastTouchedID: commandChangedIDs = %v, want %v", commandChangedIDs, want)
	}
}
