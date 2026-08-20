package slug

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		// Valid slugs
		{"single lowercase letter", "a", true},
		{"single digit", "5", true},
		{"lowercase word", "foo", true},
		{"hyphenated", "foo-bar", true},
		{"alphanumeric mix", "abc123", true},
		{"starts with digit", "123abc", true},
		{"multiple segments", "a-b-c-d-e", true},
		{"64 char max length", strings.Repeat("a", 64), true},

		// Invalid slugs
		{"empty", "", false},
		{"leading hyphen", "-foo", false},
		{"trailing hyphen", "foo-", false},
		{"uppercase", "FOO", false},
		{"mixed case", "Foo", false},
		{"underscore", "foo_bar", false},
		{"space", "foo bar", false},
		{"punctuation", "foo!", false},
		{"unicode", "café", false},
		{"65 chars (one over)", strings.Repeat("a", 65), false},
		{"only hyphen", "-", false},
		{"dot", "foo.bar", false},
		{"slash", "foo/bar", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.input)
			if tt.valid {
				if err != nil {
					t.Fatalf("Validate(%q) = %v, want nil", tt.input, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%q) = nil, want ValidationError", tt.input)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Validate(%q) error type = %T, want *ValidationError", tt.input, err)
			}
			if verr.Error() == "" {
				t.Fatalf("Validate(%q) ValidationError has empty message", tt.input)
			}
		})
	}
}

func TestAuto(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"empty title", "", "", true},
		{"only punctuation", "!!!", "", true},
		{"only whitespace", "   ", "", true},
		{"simple phrase", "Hello World", "hello-world", false},
		{"surrounding whitespace", "  Spaces  ", "spaces", false},
		{"already kebab", "foo-bar", "foo-bar", false},
		{"already lowercase", "abc123", "abc123", false},
		{"mixed case becomes lower", "FooBarBaz", "foobarbaz", false},
		{"collapses repeated separators", "a---b___c   d", "a-b-c-d", false},
		{"strips leading separators", "---foo", "foo", false},
		{"strips trailing separators", "foo---", "foo", false},
		// Unicode é is not [a-z0-9] so it becomes a hyphen → "caf-au-lait"
		{"unicode dropped to hyphens", "café au lait", "caf-au-lait", false},
		{"numbers preserved", "v1.2.3 release", "v1-2-3-release", false},
		{"single word", "Foo", "foo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Auto(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Auto(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Auto(%q) error = %v, want nil", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("Auto(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestAutoTruncatesToWordBoundary covers the 64-char truncation rule:
// the result must be at most 64 chars, and if truncation lands inside a
// word, Auto must walk back to the last hyphen.
func TestAutoTruncatesToWordBoundary(t *testing.T) {
	// 80 chars of alternating word + hyphen so we know a hyphen sits
	// well below the 64-char ceiling.
	long := "alpha-bravo-charlie-delta-echo-foxtrot-golf-hotel-india-juliet-kilo-lima-mike"
	got, err := Auto(long)
	if err != nil {
		t.Fatalf("Auto(long) error = %v, want nil", err)
	}
	if len(got) > 64 {
		t.Fatalf("Auto(long) length = %d, want <= 64; got %q", len(got), got)
	}
	if strings.HasSuffix(got, "-") {
		t.Fatalf("Auto(long) = %q ends in hyphen, want clean word boundary", got)
	}
	// Result must validate.
	if err := Validate(got); err != nil {
		t.Fatalf("Auto(long) -> %q does not validate: %v", got, err)
	}
}

// TestAutoSingleLongWordNoHyphens covers the edge case where the title
// is one alphanumeric run longer than 64 chars and contains no hyphens
// at all — there is no word boundary to walk back to, so Auto must
// return the 64-char prefix (still valid per the Pattern regex).
func TestAutoSingleLongWordNoHyphens(t *testing.T) {
	long := strings.Repeat("a", 100)
	got, err := Auto(long)
	if err != nil {
		t.Fatalf("Auto(100-char-aaa) error = %v, want nil", err)
	}
	if len(got) != 64 {
		t.Fatalf("Auto(100-char-aaa) length = %d, want 64; got %q", len(got), got)
	}
	if err := Validate(got); err != nil {
		t.Fatalf("Auto(100-char-aaa) -> %q does not validate: %v", got, err)
	}
}

// TestAutoRoundTripsToValidate is the property check: any non-error
// result from Auto must satisfy Validate with nil. This guards against
// future tweaks to Auto that might emit shapes the regex rejects.
func TestAutoRoundTripsToValidate(t *testing.T) {
	titles := []string{
		"Hello World",
		"foo-bar-baz",
		"v1.2.3 release",
		"  leading and trailing  ",
		"FooBarBaz",
		"café au lait",
		"a",
		"123",
		"plan: ship the thing!",
		strings.Repeat("alpha-bravo-", 20),
	}
	for _, title := range titles {
		t.Run(title, func(t *testing.T) {
			s, err := Auto(title)
			if err != nil {
				return // documented error case — not a round-trip candidate
			}
			if err := Validate(s); err != nil {
				t.Fatalf("Auto(%q) = %q failed Validate: %v", title, s, err)
			}
		})
	}
}

func TestValidationErrorIsTyped(t *testing.T) {
	err := Validate("")
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate(\"\") err = %T, want *ValidationError", err)
	}
	if verr.Msg == "" {
		t.Fatal("ValidationError.Msg empty")
	}
}
