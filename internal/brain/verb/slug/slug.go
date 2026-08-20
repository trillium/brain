// Package slug implements slug validation and auto-generation for brain
// docs. Slugs are stable, human-readable identifiers stored on the
// issues.slug column (added in migration 0050, unique-indexed in
// migration 0052). They are required for ISA-kind docs (so PAI hooks
// can look up the ISA by its WORK/ directory slug) and optional for
// other kinds.
//
// The regex contract `^[a-z0-9][a-z0-9-]{0,63}$` lines up with the
// kebab-case slugger PAI uses for `MEMORY/WORK/{timestamp}_{slug}/`
// directories, so a `--slug=foo-bar` passed at brain new time matches
// the WORK/ directory name PAI already chose.
package slug

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Pattern is the canonical slug regex. Lowercase letters and digits,
// optional internal hyphens; must start AND end with a letter or digit;
// 1–64 chars. Leading and trailing hyphens are rejected because the
// kebab-case slugger PAI uses for MEMORY/WORK/ directory names trims
// them — accepting them at validate time would let `brain new --slug=foo-`
// drift away from the WORK/ directory shape PAI already chose.
const Pattern = `^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`

var pattern = regexp.MustCompile(Pattern)

// ValidationError is the typed error this package raises for slug-shape
// problems (regex miss, empty input). The Cobra layer asserts on this
// type with errors.As and exits with code 2 (validation failure).
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

// Validate returns a *ValidationError if s does not match Pattern.
// An empty string is also a validation error — call Auto first if you
// want a generated default.
func Validate(s string) error {
	if s == "" {
		return &ValidationError{Msg: "slug is required"}
	}
	if !pattern.MatchString(s) {
		return &ValidationError{Msg: fmt.Sprintf(
			"invalid slug %q: must match %s", s, Pattern,
		)}
	}
	return nil
}

// Auto generates a slug from a title using a kebab-case slugger that
// mirrors the one PAI uses for MEMORY/WORK/ directory names. The
// algorithm:
//
//  1. Lowercase the title.
//  2. Replace runs of non-alphanumeric chars with single hyphens.
//  3. Trim leading/trailing hyphens.
//  4. Truncate to 64 chars; if truncation lands inside a word, walk
//     back to the last hyphen so the slug ends on a word boundary.
//  5. If the result is empty (title had no alphanumerics), return an
//     error — the caller must require an explicit --slug.
//
// Result is guaranteed to satisfy Validate when err is nil.
func Auto(title string) (string, error) {
	if title == "" {
		return "", errors.New("cannot auto-generate slug from empty title")
	}
	// Lowercase + replace non-alphanumerics with hyphens.
	var b strings.Builder
	b.Grow(len(title))
	prevHyphen := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
		} else {
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "", errors.New("title has no alphanumeric characters; supply --slug explicitly")
	}
	if len(s) > 64 {
		s = s[:64]
		// Walk back to last hyphen so we don't end mid-word. Only do this
		// if the trim leaves at least one rune — otherwise keep the 64-char
		// prefix as the slug.
		if i := strings.LastIndex(s, "-"); i > 0 {
			s = s[:i]
		}
		// After walking back we may have a trailing hyphen — trim it.
		s = strings.TrimRight(s, "-")
	}
	return s, nil
}
