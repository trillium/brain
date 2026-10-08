// Package editback — frontmatter.go — parsing a rendered markdown file
// back into its fields.
//
// The parser reads exactly the shape the exfiltrator writes (Open Knowledge
// Format frontmatter with the legacy brain fields, plus the "# {title}" H1)
// and is deliberately tolerant of hand edits *within* that shape. Anything
// outside it — unclosed frontmatter, two id lines that disagree, an H1 that
// matches neither the file's nor the bead's title — is a named refusal, never
// a guess. See divergence/0027 for the full conflict rule.

package editback

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Frontmatter is the parsed key/value shape of one rendered file.
// Scalars land in Fields (single values); the two list keys the renderer
// emits (tags, labels) land in Lists. First-class keys the engine reads are
// declared as constants below.
type Frontmatter struct {
	Fields map[string]string
	Lists  map[string][]string
}

// Frontmatter keys the engine has opinions about. Every other key
// (type, resource, timestamp, created, updated, …) is either decoration
// the substrate owns or is simply ignored.
const (
	keyTitle    = "title"
	keyID       = "id"
	keySlug     = "slug"
	keyKind     = "kind"
	keyStatus   = "status"
	keyPriority = "priority"
	keyLabels   = "labels"
	keyTags     = "tags"
	keyDesc     = "description"
)

// ParseFrontmatter splits raw into frontmatter and body, parses the
// frontmatter block's scalars and lists, and refuses — never guesses — when
// the block is not what a render (or a faithful hand edit of one) produces.
func ParseFrontmatter(raw []byte) (fm *Frontmatter, body []byte, err error) {
	text := string(raw)
	lines := strings.Split(text, "\n")

	// Opening fence.
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, nil, fmt.Errorf("file does not start with a frontmatter fence (---): refusing to guess which lines are metadata")
	}

	// Closing fence. Unquoted "---" is also the YAML value that opens no
	// block we care about; the renderer never writes it, so the first
	// fence line is the close.
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return nil, nil, fmt.Errorf("frontmatter has no closing fence (---): refusing to treat arbitrary lines as metadata")
	}

	fm = &Frontmatter{
		Fields: map[string]string{},
		Lists:  map[string][]string{},
	}
	for i := 1; i < closeIdx; i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, nil, fmt.Errorf("frontmatter line %d (%q) has no key — refusing to guess what it is", i+1, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, nil, fmt.Errorf("frontmatter line %d has no key name", i+1)
		}
		value = strings.TrimSpace(value)

		if strings.HasPrefix(value, "[") {
			items, perr := parseList(value)
			if perr != nil {
				return nil, nil, fmt.Errorf("frontmatter key %q is not a readable list: %v", key, perr)
			}
			if prev, dup := fm.Lists[key]; dup && !equalLists(prev, items) {
				return nil, nil, fmt.Errorf("frontmatter key %q appears twice with different values (%v and %v) — refusing to guess which is the edit", key, prev, items)
			}
			fm.Lists[key] = items
			continue
		}

		value, perr := parseScalar(value)
		if perr != nil {
			return nil, nil, fmt.Errorf("frontmatter key %q: %v", key, perr)
		}
		if prev, dup := fm.Fields[key]; dup && prev != value {
			return nil, nil, fmt.Errorf("frontmatter key %q appears twice with different values (%q and %q) — refusing to guess which is the edit", key, prev, value)
		}
		fm.Fields[key] = value
	}

	return fm, []byte(strings.Join(lines[closeIdx+1:], "\n")), nil
}

// parseScalar decodes one scalar value. Rendered values are JSON-quoted
// (the renderer reuses encoding/json so escaping is deterministic); hand
// edits may have dropped the quotes, so unquoted values pass through after
// whitespace trim.
func parseScalar(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if v[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(v), &s); err != nil {
			return "", fmt.Errorf("quoted value is not valid JSON: %v", err)
		}
		return s, nil
	}
	// A multi-line YAML block (| or >) is OUTSIDE the render shape and is
	// refused rather than narrowed.
	if v[0] == '|' || v[0] == '>' {
		return "", fmt.Errorf("multi-line YAML block scalars are outside the render shape; edit the field with 'bd patch' instead")
	}
	return v, nil
}

// parseList decodes [a, "b, c"]-style lists. Rendered lists are
// JSON-quoted items separated by ", " so a quoted item may contain commas;
// the splitter tracks quotes and never splits inside one.
func parseList(v string) ([]string, error) {
	if !strings.HasSuffix(v, "]") {
		return nil, fmt.Errorf("list does not close")
	}
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return nil, nil
	}
	var (
		items    []string
		current  strings.Builder
		inQuote  bool
		escaping bool
	)
	flush := func() error {
		part := strings.TrimSpace(current.String())
		if part == "" {
			return fmt.Errorf("empty list item")
		}
		item, err := parseScalar(part)
		if err != nil {
			return err
		}
		items = append(items, item)
		current.Reset()
		return nil
	}
	for _, r := range inner {
		switch {
		case escaping:
			current.WriteRune(r)
			escaping = false
		case r == '\\':
			current.WriteRune(r)
			escaping = true
		case r == '"':
			// Quotes inside an outer-quoted string are literal; track
			// only quotes that open an item.
			if strings.TrimSpace(current.String()) == "" {
				inQuote = !inQuote
				current.WriteRune(r)
			} else if inQuote {
				current.WriteRune(r)
				inQuote = false
			} else {
				current.WriteRune(r)
			}
		case r == ',' && !inQuote:
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			current.WriteRune(r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("list item quote does not close")
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return items, nil
}

// SplitBody answers the description a rendered file's body carries, after
// stripping the "# {title}" H1. The rules are the headline of the
// edit-back conflict rule:
//
//   - the H1 mirrors the title, so it must equal the file's title or the
//     bead's current title. Something else means two writers disagree about
//     the same text — refusal, never a guess;
//   - the H1 is only missing when the rendered title was empty, so a
//     missing H1 on a titled bead is the editor deleting a line — refusal;
//   - trailing newlines are not content; they are stripped on both sides
//     before the description compares equal.
//
// fileTitle is the frontmatter title being imported; beadTitle is the
// bead's current (pre-import) title.
func SplitBody(body []byte, fileTitle, beadTitle string) (string, error) {
	text := strings.TrimRight(string(body), "\n")
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	lines := strings.Split(text, "\n")
	first := -1
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			first = i
			break
		}
	}
	if first < 0 {
		return "", nil
	}
	if h1, ok := strings.CutPrefix(lines[first], "# "); !ok && strings.HasPrefix(lines[first], "#") {
		// "#x" or "## x": not the H1 shape the renderer writes.
		if beadTitle != "" || fileTitle != "" {
			return "", fmt.Errorf("body does not start with an H1 heading (# title): the shape is outside a render; refusing to treat it as the description")
		}
		return strings.TrimSpace(strings.TrimRight(strings.Join(lines[first:], "\n"), "\n")), nil
	} else if ok {
		h1 = strings.TrimSpace(h1)
		if h1 != fileTitle && h1 != beadTitle {
			return "", fmt.Errorf("H1 heading %q matches neither the file's title %q nor the bead's current title %q — one of the two was edited and refusing to guess which is the change; edit both or use 'bd patch'",
				h1, fileTitle, beadTitle)
		}
		return strings.TrimRight(strings.TrimLeft(strings.Join(lines[first+1:], "\n"), "\n"), "\n"), nil
	}
	// No H1 at all on a titled bead: the heading line was deleted, and
	// everything after it would silently become the description.
	if beadTitle != "" || fileTitle != "" {
		return "", fmt.Errorf("body has no H1 heading (\"# <title>\") though the bead has a title — a deleted heading is treated as a possibly-destructive edit, not a description change; refusing")
	}
	return strings.TrimSpace(strings.TrimRight(strings.Join(lines[first:], "\n"), "\n")), nil
}

// FilePriority parses the file's priority scalar into an int. Empty means
// "not present" (no priority change).
func FilePriority(fm *Frontmatter) (int, bool, error) {
	raw, ok := fm.Fields[keyPriority]
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false, fmt.Errorf("priority %q is not an integer (0-4)", raw)
	}
	if n < 0 || n > 4 {
		return 0, false, fmt.Errorf("priority %d is outside 0-4", n)
	}
	return n, true, nil
}

// FileTitle answers the frontmatter title and whether the key was present
// at all (a missing key is "no title change", not "empty title").
func FileTitle(fm *Frontmatter) (string, bool) {
	v, ok := fm.Fields[keyTitle]
	return v, ok
}

// FileFileLabels merges the OKF `tags` and legacy `labels` keys. The
// renderer writes both with the same value, so when they disagree the file
// itself is ambiguous about which is the edit — the caller refuses.
//
// present reports whether either key was present; ambiguous reports the
// disagreement above.
func FileLabels(fm *Frontmatter) (merged []string, present, ambiguous bool) {
	labels, labelOK := fm.Lists[keyLabels]
	tags, tagOK := fm.Lists[keyTags]
	if !labelOK && !tagOK {
		return nil, false, false
	}
	if labelOK && tagOK && !equalLists(labels, tags) {
		return nil, true, true
	}
	if labelOK {
		return dedupe(labels), true, false
	}
	return dedupe(tags), true, false
}

// FileFileID answers the bead id the file names. Not found = unnamed file.
func FileID(fm *Frontmatter) (string, bool) {
	v, ok := fm.Fields[keyID]
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return strings.TrimSpace(v), true
}

// StringOf returns a scalar or "" when the key is absent.
func StringOf(fm *Frontmatter, key string) (string, bool) {
	v, ok := fm.Fields[key]
	return v, ok
}

func equalLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// hasString answers whether s is in list.
func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
