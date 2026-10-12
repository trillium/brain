// Package idpattern resolves date-keyed issue ids for stores that hold exactly
// one bead per calendar period (a day, a week or a month).
//
// A store opts in with the `create.id-pattern` config key. The id is the
// store's issue prefix, a hyphen and the period key:
//
//	day    <prefix>-YYYY-MM-DD   the calendar day
//	week   <prefix>-YYYY-MM-DD   the Sunday that starts the week
//	month  <prefix>-YYYY-MM      the calendar month
//	quarter <prefix>-YYYY-qN     the calendar quarter (q1 Jan-Mar ... q4 Oct-Dec)
//	year   <prefix>-YYYY         the calendar year
//
// Dates are read and printed in Location (America/Los_Angeles), so "today" is
// the Los Angeles day regardless of the machine's own zone. A new period kind
// is added by extending Kind, Layout and Key.
package idpattern

import (
	"fmt"
	"strings"
	"time"
)

// Kind names one date-id pattern.
type Kind string

const (
	Day     Kind = "day"
	Week    Kind = "week"
	Month   Kind = "month"
	Quarter Kind = "quarter"
	Year    Kind = "year"
)

// ConfigKey is the config.yaml key that selects a store's pattern.
const ConfigKey = "create.id-pattern"

// Location is the zone dates are interpreted in.
const Location = "America/Los_Angeles"

const (
	dayLayout   = "2006-01-02"
	monthLayout = "2006-01"
	yearLayout  = "2006"
)

// String returns the config spelling of the kind.
func (k Kind) String() string { return string(k) }

// Parse converts a config value to a Kind. The empty string means the store
// has no pattern and yields ok=false with no error.
func Parse(s string) (k Kind, ok bool, err error) {
	switch Kind(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return "", false, nil
	case Day:
		return Day, true, nil
	case Week:
		return Week, true, nil
	case Month:
		return Month, true, nil
	case Quarter:
		return Quarter, true, nil
	case Year:
		return Year, true, nil
	}
	return "", false, fmt.Errorf("invalid %s %q (expected day, week, month, quarter or year)", ConfigKey, s)
}

// Layout is the date format of the id suffix for this kind.
func (k Kind) Layout() string {
	switch k {
	case Month:
		return monthLayout
	case Year:
		return yearLayout
	}
	return dayLayout
}

// Key renders t as the id suffix for this kind. For Week it is the Sunday on
// or before t; the time of day is ignored.
func (k Kind) Key(t time.Time) string {
	switch k {
	case Week:
		t = t.AddDate(0, 0, -int(t.Weekday()))
	case Quarter:
		return fmt.Sprintf("%04d-q%d", t.Year(), (int(t.Month())-1)/3+1)
	}
	return t.Format(k.Layout())
}

// Resolver turns inputs into the one id a store allows for a period.
type Resolver struct {
	kind   Kind
	prefix string // issue prefix, without a trailing hyphen
	now    time.Time
	loc    *time.Location
}

// New builds a Resolver; now is converted to Location.
func New(kind Kind, prefix string, now time.Time) (Resolver, error) {
	loc, err := time.LoadLocation(Location)
	if err != nil {
		return Resolver{}, fmt.Errorf("load %s: %w", Location, err)
	}
	return Resolver{kind: kind, prefix: strings.TrimSuffix(prefix, "-"), now: now.In(loc), loc: loc}, nil
}

// Today is the id for the current period in Location.
func (r Resolver) Today() string {
	return r.id(r.kind.Key(r.now))
}

func (r Resolver) id(key string) string { return r.prefix + "-" + key }

// FromDate resolves an explicit date. Day and Week take YYYY-MM-DD (a Week
// date may be any day of the week); Month takes YYYY-MM or YYYY-MM-DD; Year
// takes YYYY, YYYY-MM or YYYY-MM-DD.
func (r Resolver) FromDate(date string) (string, error) {
	key, err := r.parseKey(strings.TrimSpace(date))
	if err != nil {
		return "", fmt.Errorf("invalid date %q for a %s store: %w", date, r.kind, err)
	}
	return r.id(key), nil
}

// FromID validates an explicit id against the store's pattern and returns its
// canonical form (a Week id naming a non-Sunday maps to that week's Sunday).
func (r Resolver) FromID(id string) (string, error) {
	want := r.prefix + "-"
	if !strings.HasPrefix(id, want) {
		return "", r.mismatch(id)
	}
	key, err := r.parseKey(strings.TrimPrefix(id, want))
	if err != nil {
		return "", r.mismatch(id)
	}
	// A Week id may name any day of its week; every other id must already be
	// in the canonical form for its period.
	if suffix := strings.TrimPrefix(id, want); r.kind != Week && key != suffix {
		return "", r.mismatch(id)
	}
	return r.id(key), nil
}

func (r Resolver) mismatch(id string) error {
	return fmt.Errorf("id %q does not match this store's %s pattern %s-%s (e.g. %s)",
		id, r.kind, r.prefix, patternHint(r.kind), r.Today())
}

func patternHint(k Kind) string {
	switch k {
	case Month:
		return "YYYY-MM"
	case Quarter:
		return "YYYY-qN"
	case Year:
		return "YYYY"
	}
	return "YYYY-MM-DD"
}

// parseKey parses a user-supplied date and returns the period key.
func (r Resolver) parseKey(s string) (string, error) {
	loc := r.loc
	// Coarser kinds also accept a finer date and truncate it.
	switch r.kind {
	case Month:
		if t, err := time.ParseInLocation(monthLayout, s, loc); err == nil {
			return r.kind.Key(t), nil
		}
	case Quarter:
		if len(s) == 7 && s[4:6] == "-q" && s[6] >= '1' && s[6] <= '4' {
			if t, err := time.ParseInLocation(yearLayout, s[:4], loc); err == nil {
				return r.kind.Key(t.AddDate(0, (int(s[6]-'1'))*3, 0)), nil
			}
		}
		if t, err := time.ParseInLocation(monthLayout, s, loc); err == nil {
			return r.kind.Key(t), nil
		}
	case Year:
		for _, l := range []string{yearLayout, monthLayout} {
			if t, err := time.ParseInLocation(l, s, loc); err == nil {
				return r.kind.Key(t), nil
			}
		}
	}
	t, err := time.ParseInLocation(dayLayout, s, loc)
	if err != nil {
		return "", fmt.Errorf("expected %s", patternHint(r.kind))
	}
	return r.kind.Key(t), nil
}

// Resolve picks the id: --id (validated), --date, or the current period. When
// both id and date are given they must name the same period.
func (r Resolver) Resolve(explicitID, date string) (string, error) {
	var fromID, fromDate string
	var err error
	if explicitID != "" {
		if fromID, err = r.FromID(explicitID); err != nil {
			return "", err
		}
	}
	if date != "" {
		if fromDate, err = r.FromDate(date); err != nil {
			return "", err
		}
	}
	switch {
	case fromID != "" && fromDate != "" && fromID != fromDate:
		return "", fmt.Errorf("--id %s and --date %s name different periods (%s)", explicitID, date, fromDate)
	case fromID != "":
		return fromID, nil
	case fromDate != "":
		return fromDate, nil
	}
	return r.Today(), nil
}
