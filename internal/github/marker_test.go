package github

import "testing"

func TestMarkerRoundTrip(t *testing.T) {
	body := WithMarker("first line\nsecond line", "brain", "brain-7k2")
	m, ok := ParseMarker(body)
	if !ok || m.Store != "brain" || m.ID != "brain-7k2" {
		t.Fatalf("ParseMarker(%q) = %+v, %v", body, m, ok)
	}
	if got := StripMarker(body); got != "first line\nsecond line" {
		t.Fatalf("StripMarker = %q", got)
	}
}

func TestWithMarkerReplacesInsteadOfStacking(t *testing.T) {
	once := WithMarker("text", "brain", "brain-1")
	twice := WithMarker(once, "brain", "brain-1")
	if once != twice {
		t.Fatalf("marker stacked:\n%q\n%q", once, twice)
	}
	if again := WithMarker(once, "brain", "brain-2"); StripMarker(again) != "text" {
		t.Fatalf("rewriting the marker changed the text: %q", again)
	}
	if m, _ := ParseMarker(WithMarker(once, "brain", "brain-2")); m.ID != "brain-2" {
		t.Fatalf("marker not replaced: %+v", m)
	}
}

func TestMarkerOnEmptyBody(t *testing.T) {
	body := WithMarker("", "brain", "brain-1")
	if body != BuildMarker("brain", "brain-1") {
		t.Fatalf("body = %q", body)
	}
	if StripMarker(body) != "" {
		t.Fatalf("strip left %q", StripMarker(body))
	}
}

func TestParseMarkerAbsentOrMalformed(t *testing.T) {
	for _, body := range []string{"", "no marker here", "<!-- brain-sync store=brain -->", "<!-- other store=x id=y -->"} {
		if _, ok := ParseMarker(body); ok {
			t.Errorf("ParseMarker(%q) found a marker", body)
		}
	}
}

func TestParseMarkerLastWins(t *testing.T) {
	// A person pasted another item's body, marker included, into this one.
	body := "pasted:\n" + BuildMarker("brain", "brain-old") + "\n\n" + BuildMarker("brain", "brain-new")
	if m, _ := ParseMarker(body); m.ID != "brain-new" {
		t.Fatalf("last marker should win, got %+v", m)
	}
}

func TestStripMarkerKeepsText(t *testing.T) {
	body := "Keep this.\r\n\r\nAnd this.\n\n" + BuildMarker("s", "s-1") + "\n"
	if got := StripMarker(body); got != "Keep this.\r\n\r\nAnd this." {
		t.Fatalf("StripMarker = %q", got)
	}
}
