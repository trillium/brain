package issueops

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Event rows record a JSON copy of the issue before a change plus a JSON copy
// of the applied updates. For append-heavy fields — notably `notes`, which an
// auto-documenting ledger rewrites once per agent tool call — that makes the
// events table quadratic in the number of appends: adding one 80-byte line to a
// bead whose notes are already 320 KB writes a ~640 KB event row. One store
// reached 3.3 GB of event payload backing 5 MB of actual notes, which no amount
// of `dolt gc` can shrink because all of it is genuinely reachable.
//
// So cap how much of any single string value lands in an event payload, and
// collapse the unchanged leading bytes when a field was appended to rather than
// rewritten. The event still records which fields changed, who changed them,
// and what text was added — it just stops carrying a second full copy of a
// field that already lives in the issues table (and, on Dolt-backed stores, in
// that table's own commit history).

// EventFieldLimitEnv overrides the per-field byte cap for event payloads.
// Set it to 0 to disable elision and record full before/after copies.
const EventFieldLimitEnv = "BEADS_EVENT_FIELD_LIMIT"

// DefaultEventFieldLimit is the per-field byte cap applied when the env var is
// unset. Large enough that ordinary titles, descriptions, and single notes
// survive intact; small enough that an append-per-tool-call ledger stays linear.
const DefaultEventFieldLimit = 1024

const (
	elidedMiddleFmt = "\n…[bd: elided %d bytes]…\n"
	elidedPrefixFmt = "…[bd: elided %d unchanged leading bytes]…\n"
)

// EventFieldLimit returns the per-field byte cap for event payloads.
// A value <= 0 disables elision.
func EventFieldLimit() int {
	raw, ok := os.LookupEnv(EventFieldLimitEnv)
	if !ok {
		return DefaultEventFieldLimit
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return DefaultEventFieldLimit
	}
	return n
}

// MarshalEventPayloads renders the (old_value, new_value) pair for an update
// event, eliding oversize string fields. oldIssue may be any value that
// marshals to a JSON object (typically *types.Issue); updates is the map of
// applied changes.
func MarshalEventPayloads(oldIssue any, updates map[string]interface{}) (string, string) {
	oldRaw, err := json.Marshal(oldIssue)
	if err != nil {
		oldRaw = []byte("null")
	}
	newRaw, err := json.Marshal(updates)
	if err != nil {
		newRaw = []byte("null")
	}

	limit := EventFieldLimit()
	if limit <= 0 {
		return string(oldRaw), string(newRaw)
	}

	oldStrings := stringFields(oldRaw)
	newStrings := stringFields(newRaw)

	// New side: when the field was appended to, keep only the appended tail.
	outNew := elideStringFields(newRaw, func(key, val string) string {
		if prev, ok := oldStrings[key]; ok && len(prev) > limit && strings.HasPrefix(val, prev) {
			return fmt.Sprintf(elidedPrefixFmt, len(prev)) + elide(val[len(prev):], limit)
		}
		return elide(val, limit)
	})

	// Old side: an appended-to field is fully recoverable from the issue itself,
	// so record only its size rather than a second copy of it.
	outOld := elideStringFields(oldRaw, func(key, val string) string {
		if next, ok := newStrings[key]; ok && len(val) > limit && strings.HasPrefix(next, val) {
			return fmt.Sprintf(elidedPrefixFmt, len(val))
		}
		return elide(val, limit)
	})

	return string(outOld), string(outNew)
}

// stringFields returns the top-level string-valued fields of a JSON object.
// Non-objects and non-string values are skipped.
func stringFields(raw []byte) map[string]string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	out := make(map[string]string, len(fields))
	for k, v := range fields {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
		}
	}
	return out
}

// elideStringFields rewrites each top-level string field of a JSON object
// through transform, leaving every other field's bytes exactly as they were.
// Input that is not a JSON object is returned unchanged.
func elideStringFields(raw []byte, transform func(key, val string) string) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	changed := false
	for k, v := range fields {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		out := transform(k, s)
		if out == s {
			continue
		}
		enc, err := json.Marshal(out)
		if err != nil {
			continue
		}
		fields[k] = enc
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// elide shortens s to roughly limit bytes, keeping both the head and the tail
// so the start of the field and any freshly appended text stay readable.
func elide(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head := s[:runeStartAtOrBefore(s, limit/2)]
	tail := s[runeStartAtOrAfter(s, len(s)-(limit-len(head))):]
	return head + fmt.Sprintf(elidedMiddleFmt, len(s)-len(head)-len(tail)) + tail
}

// runeStartAtOrBefore backs n up to the nearest UTF-8 rune boundary so elision
// never splits a multi-byte character.
func runeStartAtOrBefore(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// runeStartAtOrAfter advances i forward to the nearest UTF-8 rune boundary.
func runeStartAtOrAfter(s string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
