package issueops

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/steveyegge/beads/internal/types"
)

func decodeField(t *testing.T, payload, field string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("payload is not a JSON object: %v (%s)", err, payload)
	}
	raw, ok := m[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %q is not a string: %v", field, err)
	}
	return s
}

// TestMarshalEventPayloadsAppendIsLinear covers the case this exists for: a
// `bd note` append to an already-large notes field must not write a second full
// copy of that field into the event row.
func TestMarshalEventPayloadsAppendIsLinear(t *testing.T) {
	t.Setenv(EventFieldLimitEnv, "1024")

	existing := strings.Repeat("timeline line\n", 20000) // ~280 KB
	appended := "14:02 Edit src/main.go"
	old := &types.Issue{ID: "ls-1", Title: "session", Notes: existing}
	updates := map[string]interface{}{"notes": existing + appended}

	oldJSON, newJSON := MarshalEventPayloads(old, updates)

	if total := len(oldJSON) + len(newJSON); total > 4096 {
		t.Errorf("event payload for a %d-byte append is %d bytes; want <= 4096", len(appended), total)
	}
	if got := decodeField(t, oldJSON, "notes"); strings.Contains(got, "timeline line") {
		t.Errorf("old notes still carries the field contents: %q", got)
	}
	if got := decodeField(t, newJSON, "notes"); !strings.HasSuffix(got, appended) {
		t.Errorf("new notes lost the appended text; got %q", got)
	}
	if got := decodeField(t, oldJSON, "title"); got != "session" {
		t.Errorf("under-limit field was altered: got %q", got)
	}
}

// TestMarshalEventPayloadsRepeatedAppendsStayBounded asserts the growth curve
// itself: N appends must cost O(N), not O(N^2).
func TestMarshalEventPayloadsRepeatedAppendsStayBounded(t *testing.T) {
	t.Setenv(EventFieldLimitEnv, "1024")

	notes := ""
	total := 0
	const appends = 500
	for i := 0; i < appends; i++ {
		line := "12:00 Bash go test ./...\n"
		updates := map[string]interface{}{"notes": notes + line}
		oldJSON, newJSON := MarshalEventPayloads(&types.Issue{ID: "ls-1", Notes: notes}, updates)
		total += len(oldJSON) + len(newJSON)
		notes += line
	}

	// Unbounded (pre-fix) behaviour for these inputs is ~3 MB. Each append should
	// cost a bounded constant once notes exceed the limit.
	if maxTotal := appends * 3 * 1024; total > maxTotal {
		t.Errorf("%d appends produced %d bytes of event payload; want <= %d", appends, total, maxTotal)
	}
}

func TestMarshalEventPayloadsPreservesSmallValues(t *testing.T) {
	t.Setenv(EventFieldLimitEnv, "1024")

	old := &types.Issue{ID: "ls-1", Title: "before", Notes: "short note"}
	updates := map[string]interface{}{"title": "after"}

	oldJSON, newJSON := MarshalEventPayloads(old, updates)

	if got := decodeField(t, oldJSON, "title"); got != "before" {
		t.Errorf("old title = %q, want %q", got, "before")
	}
	if got := decodeField(t, oldJSON, "notes"); got != "short note" {
		t.Errorf("old notes = %q, want %q", got, "short note")
	}
	if got := decodeField(t, newJSON, "title"); got != "after" {
		t.Errorf("new title = %q, want %q", got, "after")
	}
}

// TestMarshalEventPayloadsRewritePreservesBothEnds covers a non-append change:
// the field is replaced wholesale, so head and tail of each side stay visible.
func TestMarshalEventPayloadsRewritePreservesBothEnds(t *testing.T) {
	t.Setenv(EventFieldLimitEnv, "1024")

	old := &types.Issue{ID: "ls-1", Notes: "OLDHEAD" + strings.Repeat("x", 50000) + "OLDTAIL"}
	updates := map[string]interface{}{"notes": "NEWHEAD" + strings.Repeat("y", 50000) + "NEWTAIL"}

	oldJSON, newJSON := MarshalEventPayloads(old, updates)

	gotOld := decodeField(t, oldJSON, "notes")
	if !strings.HasPrefix(gotOld, "OLDHEAD") || !strings.HasSuffix(gotOld, "OLDTAIL") {
		t.Errorf("old notes lost head/tail: %q", gotOld)
	}
	gotNew := decodeField(t, newJSON, "notes")
	if !strings.HasPrefix(gotNew, "NEWHEAD") || !strings.HasSuffix(gotNew, "NEWTAIL") {
		t.Errorf("new notes lost head/tail: %q", gotNew)
	}
}

func TestMarshalEventPayloadsPreservesNonStringFields(t *testing.T) {
	t.Setenv(EventFieldLimitEnv, "1024")

	old := &types.Issue{
		ID:       "ls-1",
		Priority: 3,
		Notes:    strings.Repeat("z", 5000),
		Metadata: json.RawMessage(`{"tool":"Bash","count":9007199254740993}`),
	}
	updates := map[string]interface{}{"notes": old.Notes + "more"}

	oldJSON, _ := MarshalEventPayloads(old, updates)

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(oldJSON), &m); err != nil {
		t.Fatalf("old payload is not a JSON object: %v", err)
	}
	if got := string(m["priority"]); got != "3" {
		t.Errorf("priority = %s, want 3", got)
	}
	if got := string(m["metadata"]); !strings.Contains(got, "9007199254740993") {
		t.Errorf("metadata lost integer precision: %s", got)
	}
}

func TestMarshalEventPayloadsLimitZeroDisablesElision(t *testing.T) {
	t.Setenv(EventFieldLimitEnv, "0")

	notes := strings.Repeat("q", 50000)
	old := &types.Issue{ID: "ls-1", Notes: notes}
	updates := map[string]interface{}{"notes": notes + "tail"}

	oldJSON, newJSON := MarshalEventPayloads(old, updates)

	if got := decodeField(t, oldJSON, "notes"); got != notes {
		t.Errorf("elision applied despite limit 0 (old notes %d bytes)", len(got))
	}
	if got := decodeField(t, newJSON, "notes"); got != notes+"tail" {
		t.Errorf("elision applied despite limit 0 (new notes %d bytes)", len(got))
	}
}

func TestElideKeepsValidUTF8(t *testing.T) {
	s := strings.Repeat("日本語テキスト", 500)
	for _, limit := range []int{1, 2, 7, 63, 1024} {
		got := elide(s, limit)
		if !utf8.ValidString(got) {
			t.Errorf("elide(limit=%d) produced invalid UTF-8", limit)
		}
	}
}

func TestEventFieldLimitDefaults(t *testing.T) {
	t.Setenv(EventFieldLimitEnv, "")
	if got := EventFieldLimit(); got != DefaultEventFieldLimit {
		t.Errorf("empty value: EventFieldLimit() = %d, want %d", got, DefaultEventFieldLimit)
	}
	t.Setenv(EventFieldLimitEnv, "not-a-number")
	if got := EventFieldLimit(); got != DefaultEventFieldLimit {
		t.Errorf("garbage value: EventFieldLimit() = %d, want %d", got, DefaultEventFieldLimit)
	}
	t.Setenv(EventFieldLimitEnv, "512")
	if got := EventFieldLimit(); got != 512 {
		t.Errorf("EventFieldLimit() = %d, want 512", got)
	}
}
