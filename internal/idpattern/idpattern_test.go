package idpattern

import (
	"strings"
	"testing"
	"time"
)

func la(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(Location)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func resolver(t *testing.T, k Kind, now time.Time) Resolver {
	t.Helper()
	r, err := New(k, k.String(), now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParse(t *testing.T) {
	for in, want := range map[string]Kind{"day": Day, " Week ": Week, "month": Month, "YEAR": Year, "quarter": Quarter} {
		k, ok, err := Parse(in)
		if err != nil || !ok || k != want {
			t.Errorf("Parse(%q) = %v %v %v", in, k, ok, err)
		}
	}
	if _, ok, err := Parse(""); ok || err != nil {
		t.Errorf("empty pattern should be off without error, got ok=%v err=%v", ok, err)
	}
	if _, _, err := Parse("hour"); err == nil {
		t.Error("unknown pattern should error")
	}
}

func TestToday(t *testing.T) {
	// 2026-10-11 is a Sunday. 23:30 PDT on Oct 11 is already Oct 12 UTC.
	late := la(t, "2026-10-11 23:30").UTC()
	for k, want := range map[Kind]string{
		Day: "day-2026-10-11", Week: "week-2026-10-11", Month: "month-2026-10", Quarter: "quarter-2026-q4", Year: "year-2026",
	} {
		if got := resolver(t, k, late).Today(); got != want {
			t.Errorf("%s Today() = %s, want %s", k, got, want)
		}
	}
	for in, want := range map[string]string{
		"2026-01-01 00:00": "quarter-2026-q1", "2026-03-31 23:59": "quarter-2026-q1",
		"2026-04-01 00:00": "quarter-2026-q2", "2026-06-30 12:00": "quarter-2026-q2",
		"2026-07-01 00:00": "quarter-2026-q3", "2026-09-30 12:00": "quarter-2026-q3",
		"2026-10-01 00:00": "quarter-2026-q4", "2026-12-31 23:59": "quarter-2026-q4",
	} {
		if got := resolver(t, Quarter, la(t, in)).Today(); got != want {
			t.Errorf("quarter at %s = %s, want %s", in, got, want)
		}
	}
	// Wednesday 2026-10-14 maps to Sunday 2026-10-11.
	if got := resolver(t, Week, la(t, "2026-10-14 09:00")).Today(); got != "week-2026-10-11" {
		t.Errorf("week Today() = %s", got)
	}
	// Week crossing a year boundary: Thu 2026-01-01 -> Sun 2025-12-28.
	if got := resolver(t, Week, la(t, "2026-01-01 12:00")).Today(); got != "week-2025-12-28" {
		t.Errorf("week across year = %s", got)
	}
}

func TestFromDate(t *testing.T) {
	now := la(t, "2026-10-11 12:00")
	cases := []struct {
		k    Kind
		in   string
		want string
	}{
		{Day, "2026-03-04", "day-2026-03-04"},
		{Day, "2024-02-29", "day-2024-02-29"},
		{Week, "2026-10-14", "week-2026-10-11"},
		{Week, "2026-10-11", "week-2026-10-11"},
		{Week, "2026-10-17", "week-2026-10-11"},
		{Week, "2026-10-18", "week-2026-10-18"},
		{Month, "2026-03", "month-2026-03"},
		{Month, "2026-03-31", "month-2026-03"},
		{Quarter, "2026-q3", "quarter-2026-q3"},
		{Quarter, "2026-08", "quarter-2026-q3"},
		{Quarter, "2026-12-31", "quarter-2026-q4"},
		{Quarter, "2026-01-01", "quarter-2026-q1"},
		{Year, "2026", "year-2026"},
		{Year, "2026-03", "year-2026"},
		{Year, "2026-03-31", "year-2026"},
	}
	for _, c := range cases {
		got, err := resolver(t, c.k, now).FromDate(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s FromDate(%q) = %q, %v; want %q", c.k, c.in, got, err, c.want)
		}
	}
	bad := []struct {
		k  Kind
		in string
	}{
		{Day, "2026-02-30"}, {Day, "2026-2-3"}, {Day, "2026-03"}, {Day, "yesterday"}, {Day, "2026-13-01"},
		{Week, "2026-10"}, {Month, "2026-13"}, {Month, "2026"}, {Month, "26-03"}, {Year, "26"}, {Year, "2026-13"},
		{Quarter, "2026-q5"}, {Quarter, "2026-q0"}, {Quarter, "2026-Q1"}, {Quarter, "2026"}, {Quarter, "2026-13"},
	}
	for _, c := range bad {
		if got, err := resolver(t, c.k, now).FromDate(c.in); err == nil {
			t.Errorf("%s FromDate(%q) = %q, want error", c.k, c.in, got)
		}
	}
}

func TestFromID(t *testing.T) {
	now := la(t, "2026-10-11 12:00")
	ok := map[Kind][][2]string{
		Day:     {{"day-2026-10-11", "day-2026-10-11"}},
		Week:    {{"week-2026-10-11", "week-2026-10-11"}, {"week-2026-10-14", "week-2026-10-11"}},
		Month:   {{"month-2026-10", "month-2026-10"}},
		Quarter: {{"quarter-2026-q1", "quarter-2026-q1"}, {"quarter-2026-q4", "quarter-2026-q4"}},
		Year:    {{"year-2026", "year-2026"}},
	}
	for k, pairs := range ok {
		for _, p := range pairs {
			got, err := resolver(t, k, now).FromID(p[0])
			if err != nil || got != p[1] {
				t.Errorf("%s FromID(%q) = %q, %v; want %q", k, p[0], got, err, p[1])
			}
		}
	}
	bad := map[Kind][]string{
		Day:     {"day-dyy", "day-2026-10", "day-2026-10-11.1", "day-2026-02-30", "week-2026-10-11", "day2026-10-11", "2026-10-11"},
		Week:    {"week-d0f", "week-2026-10", "day-2026-10-11"},
		Month:   {"month-lso", "month-2026-10-11", "month-2026-13", "month-2026"},
		Quarter: {"quarter-2026-Q1", "quarter-2026-q5", "quarter-2026-10", "quarter-2026", "quarter-2026-q1.1", "quarter-abc"},
		Year:    {"year-abc", "year-2026-10", "year-26", "month-2026"},
	}
	for k, ids := range bad {
		for _, id := range ids {
			_, err := resolver(t, k, now).FromID(id)
			if err == nil {
				t.Errorf("%s FromID(%q): want error", k, id)
			} else if !strings.Contains(err.Error(), "does not match") {
				t.Errorf("%s FromID(%q): error %q should explain the pattern", k, id, err)
			}
		}
	}
}

func TestResolve(t *testing.T) {
	r := resolver(t, Week, la(t, "2026-10-11 12:00"))
	cases := []struct{ id, date, want, err string }{
		{"", "", "week-2026-10-11", ""},
		{"", "2026-10-21", "week-2026-10-18", ""},
		{"week-2026-10-18", "", "week-2026-10-18", ""},
		{"week-2026-10-20", "2026-10-22", "week-2026-10-18", ""},
		{"week-2026-10-18", "2026-10-11", "", "different periods"},
		{"week-x", "", "", "does not match"},
		{"", "nope", "", "invalid date"},
	}
	for _, c := range cases {
		got, err := r.Resolve(c.id, c.date)
		switch {
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("Resolve(%q,%q) = %q, %v; want %q", c.id, c.date, got, err, c.want)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("Resolve(%q,%q) = %q, %v; want error containing %q", c.id, c.date, got, err, c.err)
		}
	}
}
