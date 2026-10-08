package hooksdef

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// WarningKeyPrefix is the config-table key prefix under which warning records
// live: one row per warning, key WarningKeyPrefix+ID, value the record as
// JSON. The config table is the store's own durable, versioned, syncable
// key-value table, so a warning is addressable by key, survives the process,
// travels with the database, and can be read by anything that can read the
// store (`bd hook warnings`, `bd sql`, `bd config list`) — no new table, no
// side file, and the store remains the only writer.
const WarningKeyPrefix = "hookwarning."

// NewWarningID returns a sortable, unique warning ID: hw-<utc time>-<random>.
func NewWarningID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; fall back to the
		// nanosecond clock rather than refusing to record the warning.
		return fmt.Sprintf("hw-%s-%08x", now.UTC().Format("20060102T150405Z"), now.UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("hw-%s-%s", now.UTC().Format("20060102T150405Z"), hex.EncodeToString(b[:]))
}

// WarningKey is the config key of a warning record.
func WarningKey(id string) string { return WarningKeyPrefix + id }

// Encode renders the record as the JSON stored in the config row.
func (w Warning) Encode() (string, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("encoding hook warning: %w", err)
	}
	return string(raw), nil
}

// DecodeWarning parses a stored warning record. An unparseable record is an
// error naming its key: a warning that cannot be read is exactly the kind of
// degradation that must not be stepped over.
func DecodeWarning(key, value string) (Warning, error) {
	var w Warning
	if err := json.Unmarshal([]byte(value), &w); err != nil {
		return w, fmt.Errorf("hook warning record %s is unreadable: %w", key, err)
	}
	if w.ID == "" || WarningKey(w.ID) != key {
		return w, fmt.Errorf("hook warning record %s is inconsistent: its id is %q", key, w.ID)
	}
	return w, nil
}

// WarningsFromConfig extracts and decodes every warning record from a full
// config map, oldest first. Unreadable records are returned as errors alongside
// the readable ones.
func WarningsFromConfig(cfg map[string]string) ([]Warning, []error) {
	var out []Warning
	var errs []error
	for k, v := range cfg {
		if !strings.HasPrefix(k, WarningKeyPrefix) {
			continue
		}
		w, err := DecodeWarning(k, v)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs
}
