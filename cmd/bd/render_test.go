package main

import "testing"

func TestFormatRenderAllSummary_Success(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(42, 42, 0, "/tmp/store")
	want := "Exfiltrated 42 / 42 beads to /tmp/store/entries/ (0 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatRenderAllSummary_PartialFailure(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(39, 42, 3, "/home/me/data/brain")
	want := "Exfiltrated 39 / 42 beads to /home/me/data/brain/entries/ (3 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatRenderAllSummary_AllFailed(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(0, 5, 5, "/var/store")
	want := "Exfiltrated 0 / 5 beads to /var/store/entries/ (5 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatRenderAllSummary_EmptyStore(t *testing.T) {
	t.Parallel()
	got := formatRenderAllSummary(0, 0, 0, "/empty")
	want := "Exfiltrated 0 / 0 beads to /empty/entries/ (0 failed)"
	if got != want {
		t.Fatalf("summary:\n got: %q\nwant: %q", got, want)
	}
}
