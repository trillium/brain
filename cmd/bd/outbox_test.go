package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/config"
)

// setOutboxConfig applies config overrides for a test and restores defaults.
func setOutboxConfig(t *testing.T, kv map[string]interface{}) {
	t.Helper()
	config.ResetForTesting()
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	t.Cleanup(config.ResetForTesting)
	for k, v := range kv {
		config.Set(k, v)
	}
}

// ── Subscriber parsing: an unreadable definition is a named failure ─────

func TestParseOutboxSubscriber_OK(t *testing.T) {
	cases := []struct {
		spec    string
		url     string
		command string
	}{
		{"name=pulse;url=http://127.0.0.1:8099/events", "http://127.0.0.1:8099/events", ""},
		{"name=archive;command=/usr/local/bin/archive-event", "", "/usr/local/bin/archive-event"},
		{"name=a.b-c_d1;url=https://x.example/e?a=1,2", "https://x.example/e?a=1,2", ""}, // commas survive
		{" name = x ; command = /bin/sh ", "", "/bin/sh"},
	}
	for _, tc := range cases {
		s, err := parseOutboxSubscriber(tc.spec)
		if err != nil {
			t.Fatalf("parseOutboxSubscriber(%q): unexpected error: %v", tc.spec, err)
		}
		if s.URL != tc.url || s.Command != tc.command {
			t.Fatalf("parseOutboxSubscriber(%q) = %+v, want url=%q command=%q", tc.spec, s, tc.url, tc.command)
		}
	}
	// explicit name checks for the field-reordered case
	s, err := parseOutboxSubscriber(" name = x ; command = /bin/sh ")
	if err != nil || s.Name != "x" || s.Command != "/bin/sh" {
		t.Fatalf("spaced spec: got %+v err %v", s, err)
	}
}

func TestParseOutboxSubscriber_NamedRefusals(t *testing.T) {
	cases := []struct {
		spec  string
		match string
	}{
		{"url=http://x", "missing name"},
		{"name=", "missing name"},
		{"", "missing name"},
		{"name=x", "exactly one of url"},
		{"name=x;url=http://y;command=/bin/z", "exactly one of url"},
		{"name=x;command=/a;command=/b", "duplicate field"},
		{"name=x;port=80;command=/a", "unknown field"},
		{"noequals-sign", "has no =value"},
		{"name=bad name;url=http://y", "may only contain"},
	}
	for _, tc := range cases {
		_, err := parseOutboxSubscriber(tc.spec)
		if err == nil {
			t.Fatalf("parseOutboxSubscriber(%q): expected a refusal", tc.spec)
		}
		if !strings.Contains(err.Error(), tc.match) {
			t.Fatalf("parseOutboxSubscriber(%q) error %q does not mention %q", tc.spec, err, tc.match)
		}
	}
}

func TestReadOutboxConfig_DuplicateNamesRefused(t *testing.T) {
	setOutboxConfig(t, map[string]interface{}{
		"change-events.outbox.subscribers": []string{"name=a;command=/bin/x", "name=a;command=/bin/y"},
	})
	_, err := readOutboxConfig()
	if err == nil {
		t.Fatal("expected duplicate subscriber name refusal")
	}
	if !strings.Contains(err.Error(), "duplicate subscriber name") {
		t.Fatalf("wrong error: %v", err)
	}
}

// ── Backoff schedule: bounded, documented ────────────────────────────────

func TestOutboxAttemptDelay(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{5, 16 * time.Second},
		{10, 512 * time.Second},  // 8m32s
		{12, 2048 * time.Second}, // 34m8s — last un-capped step
		{13, outboxBackoffMax},   // 4096s > 1h → cap
		{99, outboxBackoffMax},
	}
	for _, tc := range cases {
		if got := outboxAttemptDelay(tc.attempts); got != tc.want {
			t.Fatalf("outboxAttemptDelay(%d) = %s, want %s", tc.attempts, got, tc.want)
		}
	}
}

// ── Retirement / awaiting bookkeeping (pure row state) ──────────────────

func TestOutboxRowRetirement(t *testing.T) {
	// No subscribers configured: retires at emission, nothing to wait for.
	r := &outboxRow{SubscribersRaw: "[]", AcksRaw: "{}"}
	if err := r.parseOutboxJSON(); err != nil {
		t.Fatal(err)
	}
	if !r.retired() {
		t.Fatal("row with empty subscriber snapshot must retire immediately")
	}

	r = &outboxRow{SubscribersRaw: `["a","b"]`, AcksRaw: `{"a":"2026-01-01T00:00:00Z"}`}
	if err := r.parseOutboxJSON(); err != nil {
		t.Fatal(err)
	}
	if r.retired() {
		t.Fatal("partially-acknowledged row must not retire")
	}
	if got := strings.Join(r.awaiting(), ","); got != "b" {
		t.Fatalf("awaiting = %q, want b", got)
	}
	// Double acknowledgement is idempotent: first timestamp wins.
	if _, dup := r.Acks["a"]; !dup {
		t.Fatal("precondition")
	}
	r.Acks["b"] = r.Acks["a"]
	if !r.retired() {
		t.Fatal("all-acknowledged row must retire")
	}

	// Delivered already: retired regardless of ack map state.
	r = &outboxRow{SubscribersRaw: `["z"]`, AcksRaw: `{}`, DeliveredAt: "2026-01-01T00:00:00Z"}
	if err := r.parseOutboxJSON(); err != nil {
		t.Fatal(err)
	}
	if !r.retired() {
		t.Fatal("delivered row must report retired")
	}
}

// ── Corruption is named, never silent ───────────────────────────────────

func TestOutboxRowCorruptionNamed(t *testing.T) {
	r := &outboxRow{SubscribersRaw: "", AcksRaw: "{}"}
	err := r.parseOutboxJSON()
	if err == nil || !strings.Contains(err.Error(), "corrupt outbox row") {
		t.Fatalf("want corrupt-outbox-row refusal, got %v", err)
	}
	r = &outboxRow{SubscribersRaw: "not-json", AcksRaw: "{}"}
	err = r.parseOutboxJSON()
	if err == nil || !strings.Contains(err.Error(), "subscribers column") {
		t.Fatalf("want subscribers-column refusal, got %v", err)
	}
}

// ── Event payload shape: readable by any subscriber ─────────────────────

func TestOutboxEventPayloadShape(t *testing.T) {
	b, err := json.Marshal(outboxEvent{TS: "2026-10-07T19:00:00Z", Store: "task", Command: "create", ID: "task-1", Seq: 7})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "store", "command", "id", "seq"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("payload missing %q: %s", k, b)
		}
	}
}
