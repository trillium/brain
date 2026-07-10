package exfiltrator

// Vault scaffolding (isa-6zq ISC-8..9): after render-all has written every
// entry, WriteIndexes walks the `<root>/entries/` tree and (re)generates a
// reserved `index.md` in each directory for OKF progressive disclosure.
//
// OKF structure rules this file implements:
//   - Every directory (the entries/ root and every nested <kind>/ dir) gets
//     an `index.md` listing that directory's contents.
//   - Non-root index.md files carry NO frontmatter (OKF: index.md has no
//     frontmatter).
//   - The bundle-root index.md (`<root>/entries/index.md`) is the ONE
//     exception: it carries minimal frontmatter declaring `okf_version`
//     (ISC-9), then the same listing.
//   - Listings are deterministic (sorted) so output is byte-stable across
//     runs (ISC-3 idempotence extended to the scaffolding pass).
//
// WriteIndexes is a pure filesystem function: given a root, it reads the
// on-disk `entries/` tree and writes index.md files. It performs no Dolt
// access, so it is fast and trivially testable against a temp directory.

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// OKFVersion is the OKF spec version declared in the bundle-root index.md
// frontmatter (ISC-9). Bumped only when the emitted vault shape targets a
// newer OKF revision.
const OKFVersion = "0.1"

// indexReservedFiles are the reserved / bookkeeping filenames that must be
// excluded from a directory's index.md listing. Compared case-insensitively
// against the base filename. `.checkpoint.json` and dotfiles are handled
// separately by the dotfile skip in the walk, but `.checkpoint.json` is
// listed here defensively as well.
var indexReservedFiles = map[string]bool{
	"index.md":         true,
	"log.md":           true,
	".checkpoint.json": true,
}

// WriteIndexes walks `<root>/entries/` and writes a reserved `index.md` in
// every directory it contains (including the `entries/` root itself). The
// root index.md carries `okf_version` frontmatter (ISC-9); every nested
// index.md is frontmatter-free (ISC-8 / OKF).
//
// If `<root>/entries/` does not exist, WriteIndexes is a no-op and returns
// nil — a store that has never rendered has nothing to scaffold.
//
// Errors from reading or writing any directory abort the walk and are
// returned wrapped; partial progress (indexes already written for earlier
// directories) is left in place, which is safe because the operation is
// idempotent — a re-run regenerates every index deterministically.
func WriteIndexes(root string) error {
	entriesRoot := filepath.Join(expandHome(root), "entries")

	info, err := os.Stat(entriesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("exfiltrator: stat entries root %s: %w", entriesRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("exfiltrator: entries root %s is not a directory", entriesRoot)
	}

	// Collect every directory under entriesRoot (inclusive) so we can emit
	// one index.md per directory. Directories are gathered up-front and
	// sorted so the walk order is deterministic (does not affect output
	// bytes, but keeps error surfacing stable).
	dirs, err := collectIndexDirs(entriesRoot)
	if err != nil {
		return err
	}

	for _, dir := range dirs {
		isRoot := dir == entriesRoot
		if err := writeDirIndex(dir, isRoot); err != nil {
			return err
		}
	}
	return nil
}

// collectIndexDirs returns entriesRoot plus every subdirectory beneath it,
// skipping dot-directories (e.g. `.git`, `.obsidian`). The result is sorted
// for deterministic error ordering.
func collectIndexDirs(entriesRoot string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(entriesRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// Skip dot-directories entirely (do not descend, do not index). The
		// entries root itself never begins with a dot.
		if path != entriesRoot && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("exfiltrator: walk entries tree: %w", err)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// indexEntry is one listable child of a directory: either a markdown file or
// a subdirectory. Title is the display text for the link; LinkTarget is the
// relative URL (a `<slug>.md` for files, a `<name>/` for subdirs).
type indexEntry struct {
	Title      string
	LinkTarget string
	IsDir      bool
}

// writeDirIndex generates and writes `<dir>/index.md`. When isRoot is true the
// file carries `okf_version` frontmatter (ISC-9); otherwise it is
// frontmatter-free (ISC-8).
func writeDirIndex(dir string, isRoot bool) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("exfiltrator: read dir %s: %w", dir, err)
	}

	var listed []indexEntry
	for _, e := range ents {
		name := e.Name()
		// Skip dotfiles and dot-directories (covers `.checkpoint.json`,
		// `.git`, `.obsidian`, etc.).
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			listed = append(listed, indexEntry{
				Title:      name,
				LinkTarget: name + "/",
				IsDir:      true,
			})
			continue
		}
		// Only markdown files are listed; skip reserved bookkeeping files.
		if !strings.EqualFold(filepath.Ext(name), ".md") {
			continue
		}
		if indexReservedFiles[strings.ToLower(name)] {
			continue
		}
		title := titleForEntry(filepath.Join(dir, name))
		listed = append(listed, indexEntry{
			Title:      title,
			LinkTarget: name,
			IsDir:      false,
		})
	}

	sortIndexEntries(listed)

	body := renderIndex(dir, listed, isRoot)
	path := filepath.Join(dir, "index.md")
	if err := atomicWrite(path, []byte(body)); err != nil {
		return fmt.Errorf("exfiltrator: write index %s: %w", path, err)
	}
	return nil
}

// sortIndexEntries orders the listing deterministically: alphabetically by
// title (case-insensitive), then by link target as a stable tie-break. This
// is what makes WriteIndexes byte-stable across runs (ISC-3). Subdirs and
// files interleave by title rather than being segregated — simple and
// deterministic; OKF does not mandate a section split.
func sortIndexEntries(entries []indexEntry) {
	sort.Slice(entries, func(i, j int) bool {
		li := strings.ToLower(entries[i].Title)
		lj := strings.ToLower(entries[j].Title)
		if li != lj {
			return li < lj
		}
		if entries[i].Title != entries[j].Title {
			return entries[i].Title < entries[j].Title
		}
		return entries[i].LinkTarget < entries[j].LinkTarget
	})
}

// renderIndex builds the index.md body for a directory. The root index gets
// `okf_version` frontmatter; nested indexes get none. The heading is derived
// from the directory's base name (`Entries` for the root, the capitalized
// kind for a nested dir).
func renderIndex(dir string, entries []indexEntry, isRoot bool) string {
	var b strings.Builder

	if isRoot {
		b.WriteString("---\n")
		b.WriteString(fmt.Sprintf("okf_version: %q\n", OKFVersion))
		b.WriteString("---\n\n")
	}

	b.WriteString("# ")
	b.WriteString(indexHeading(dir, isRoot))
	b.WriteString("\n\n")

	if len(entries) == 0 {
		// An empty directory still gets a valid, listing-less index so the
		// vault has a landing note everywhere. No trailing bullet lines.
		return b.String()
	}

	for _, e := range entries {
		b.WriteString("* [")
		b.WriteString(indexLinkText(e.Title))
		b.WriteString("](")
		b.WriteString(e.LinkTarget)
		b.WriteString(")")
		if desc := indexDescription(e); desc != "" {
			b.WriteString(" - ")
			b.WriteString(desc)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// indexHeading returns the `# <heading>` text for a directory's index.
// The bundle root reads "Entries"; a nested <kind>/ dir reads the kind name
// with a leading capital (e.g. "Knowledge").
func indexHeading(dir string, isRoot bool) string {
	if isRoot {
		return "Entries"
	}
	base := filepath.Base(dir)
	if base == "" {
		return "Index"
	}
	return capitalizeFirst(base)
}

// indexDescription returns the optional trailing description for a listing
// bullet. For subdirectories it is empty (the trailing slash already conveys
// "directory"); for files it is empty too — keeping the listing minimal and
// maximally conformant. Kept as a seam so a future revision can add a short
// kind/description without touching the render loop.
func indexDescription(_ indexEntry) string {
	return ""
}

// indexLinkText escapes the two characters that would break a markdown link
// label — `]` (terminates the label) and `\` (the escape char). Titles are
// single-line by construction.
func indexLinkText(s string) string {
	if !strings.ContainsAny(s, "]\\") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `]`, `\]`)
	return r.Replace(s)
}

// titleForEntry derives the display title for a markdown file by parsing its
// frontmatter `title` field. Falls back to the slug (filename without the
// `.md` extension) when the title is absent, empty, or the file is
// unreadable — the listing must never fail on one bad file.
func titleForEntry(path string) string {
	slug := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

	data, err := os.ReadFile(path)
	if err != nil {
		return slug
	}
	if title := frontmatterTitle(data); title != "" {
		return title
	}
	return slug
}

// frontmatterTitle extracts and unquotes the `title:` value from a markdown
// file's leading YAML frontmatter block. Returns "" when there is no
// frontmatter, no `title` key, or an empty value.
//
// The parser is intentionally small — it reads only the top-level `title:`
// scalar from the fenced block (the shape renderMarkdown emits: a quoted
// JSON string via yamlString). It does not pull in a full YAML dependency,
// keeping this package free of external imports.
func frontmatterTitle(data []byte) string {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if !scanner.Scan() {
		return "" // empty file
	}
	if strings.TrimRight(scanner.Text(), " \t\r") != "---" {
		return "" // no opening fence → no frontmatter
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimRight(line, " \t\r") == "---" {
			return "" // closing fence reached without a title
		}
		trimmed := strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(trimmed, "title:")
		if !ok {
			continue
		}
		return unquoteYAMLScalar(strings.TrimSpace(rest))
	}
	return "" // opening fence but no closing fence: treat as no title
}

// unquoteYAMLScalar unwraps a YAML scalar as renderMarkdown emits it. Titles
// are written via yamlString (encoding/json), so a quoted value is a JSON
// string literal — unwrap it by trimming the surrounding double quotes and
// undoing the JSON escapes we care about (`\"` and `\\`). Single-quoted and
// bare scalars are returned trimmed. A malformed value degrades to the raw
// trimmed text rather than erroring.
func unquoteYAMLScalar(s string) string {
	if s == "" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		// Undo the escapes yamlString (encoding/json) can introduce for a
		// single-line title: escaped quote and escaped backslash. json.Marshal
		// of a plain title produces only these two for our inputs.
		r := strings.NewReplacer(`\"`, `"`, `\\`, `\`)
		return r.Replace(inner)
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		inner := s[1 : len(s)-1]
		// YAML single-quote escaping doubles an embedded quote.
		return strings.ReplaceAll(inner, "''", "'")
	}
	return s
}

// capitalizeFirst upper-cases the first rune of s and leaves the rest
// unchanged. Empty string returns empty.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
