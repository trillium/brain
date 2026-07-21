// Package isarender implements the pure-Go core of the `bd isa-render` verb:
// reverse exfiltration from the Dolt substrate to canonical IsaFormat v2.7
// markdown on disk.
//
// Direction: Dolt → markdown. Brain is the source of truth; the rendered file
// at `<exfil-root>/{slug}/ISA.md` is a derived view that downstream tools
// (PAI hooks, editors, grep) can read.
//
// This package is intentionally fully pure-Go and has zero Dolt / cgo
// dependencies so the byte-exact rendering, path resolution, and atomic-write
// semantics can be exercised by fast unit tests. The cobra command in
// cmd/bd/isa_render.go is the thin shell that loads the row from the embedded
// store and calls into this package.
//
// IsaFormat v2.7 contract (mirrors PAI/DOCUMENTATION/IsaFormat.md):
//
//	---
//	task: "<title>"
//	slug: "<slug>"
//	effort: <effort>           # standard|extended|advanced|deep|comprehensive
//	phase: <phase lowercased>  # observe|think|plan|build|execute|verify|learn|complete
//	progress: "<m>/<n>"
//	mode: <mode>               # interactive|loop|optimize
//	started: <RFC3339>
//	updated: <RFC3339>
//	brain_id: <id>             # F2 addition: trace markdown → brain
//	---
//
//	# <title>
//
//	## Problem
//	...
//	## Vision
//	...
//	(twelve sections in spec order; empty sections skipped)
package isarender

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/brain/verb/isashow"
)

// DefaultExfilRoot is the on-disk root used when BRAIN_ISA_EXFIL_ROOT is
// unset. Mirrors the PAI work-memory layout so a freshly rendered ISA lands
// where the rest of the algorithm already looks.
const DefaultExfilRoot = ".claude/PAI/MEMORY/WORK"

// EnvExfilRoot is the environment variable that overrides DefaultExfilRoot.
// Documented for tests and ops; the verb reads this name and only this name.
const EnvExfilRoot = "BRAIN_ISA_EXFIL_ROOT"

// sectionTitles maps canonical lower_snake_case section names to the
// human-facing markdown headings. Kept local because isashow.sectionTitles is
// unexported and this package needs the same mapping; the canonical order
// itself is reused from isashow.SpecOrderedSectionNames() so the two packages
// can never drift.
var sectionTitles = map[string]string{
	"problem":       "Problem",
	"vision":        "Vision",
	"out_of_scope":  "Out of Scope",
	"principles":    "Principles",
	"constraints":   "Constraints",
	"goal":          "Goal",
	"criteria":      "Criteria",
	"test_strategy": "Test Strategy",
	"features":      "Features",
	"decisions":     "Decisions",
	"changelog":     "Changelog",
	"verification":  "Verification",
}

// RenderInput is the pure-Go input shape for Render. The cobra layer builds
// this from a Dolt row + isa_sections rows; the package never touches Dolt.
//
// Nullable scalars use *string / *time.Time so the renderer can distinguish
// "field is unset, omit the frontmatter key" from "field is empty string".
// ISC-38 specifies the eight required IsaFormat v2.7 fields plus brain_id;
// NULL rules from the ISA spec require those keys to be omitted, not emitted
// as `phase: null`.
type RenderInput struct {
	ID        string  // issues.id (required) — surfaces as brain_id
	Slug      string  // issues.slug (required, non-empty) — keys the target dir
	Title     string  // issues.title (required) — H1 header and `task:` frontmatter
	Phase     *string // issues.isa_phase, lowercased on emit; nil → omit
	Effort    *string // issues.isa_effort; nil → omit
	Mode      *string // issues.isa_mode; nil → omit
	ProgressM int     // issues.isa_progress_m
	ProgressN int     // issues.isa_progress_n
	StartedAt *time.Time
	UpdatedAt *time.Time
	Sections  map[string]string // section_name → body, verbatim
}

// PathTraversalError is the typed error Render-related path resolution
// raises when the target path would escape the configured exfil root. The
// cobra layer asserts on this type and exits with code 2 (ISC-37).
type PathTraversalError struct {
	Slug       string
	ExfilRoot  string
	ResolvedTo string
}

func (e *PathTraversalError) Error() string {
	return fmt.Sprintf("slug %q resolves outside exfil root %q (would land at %q)",
		e.Slug, e.ExfilRoot, e.ResolvedTo)
}

// InputError is the typed error Render raises for input-shape problems
// (empty slug, empty id). cobra maps this to exit 2.
type InputError struct {
	Msg string
}

func (e *InputError) Error() string { return e.Msg }

// ResolveExfilRoot returns the on-disk root where rendered ISAs live.
//
//	BRAIN_ISA_EXFIL_ROOT set    → that value, verbatim (no $HOME expansion).
//	BRAIN_ISA_EXFIL_ROOT unset  → ${HOME}/.claude/PAI/MEMORY/WORK.
//
// When HOME is also unset (extremely rare; CI without env), the default
// collapses to the relative DefaultExfilRoot — the caller will hit a
// path-traversal or write error downstream and surface a clear message.
func ResolveExfilRoot() string {
	if v := os.Getenv(EnvExfilRoot); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return DefaultExfilRoot
	}
	return filepath.Join(home, DefaultExfilRoot)
}

// ResolveTargetPath returns the canonical render target for an ISA:
//
//	<exfilRoot>/<slug>/ISA.md
//
// The slug must not contain path separators or `..` segments — a single,
// flat directory name. F1d's slug regex (`^[a-z0-9-]+$`) already enforces
// this at the write side, but the v0.3 substrate is a SQL DB and someone
// can INSERT a bogus slug directly. ISC-37 requires this guard regardless.
//
// Defense layers:
//  1. Slug must be non-empty.
//  2. Slug must equal filepath.Base(slug) (no slashes, no dot/dotdot).
//  3. The resolved path must stay under filepath.Clean(exfilRoot) +
//     separator (belt + suspenders).
//
// Returns *InputError on empty/missing inputs, *PathTraversalError on
// any of the three guard failures.
func ResolveTargetPath(exfilRoot, slug string) (string, error) {
	if slug == "" {
		return "", &InputError{Msg: "cannot render ISA with empty slug"}
	}
	if exfilRoot == "" {
		return "", &InputError{Msg: "cannot render ISA with empty exfil root"}
	}

	cleanRoot := filepath.Clean(exfilRoot)

	// Layer 1: structural slug guard. A legal slug is exactly one path
	// component with no separators, no `.`, no `..`. filepath.Base collapses
	// trailing separators and returns "." for empty / pure-slash inputs, so
	// the equality check catches `/etc/passwd` (Base → "passwd", != slug),
	// `..` (Base → "..", but we explicitly reject), `a/b` (Base → "b",
	// != slug), and `ok/../../etc` (Base → "etc", != slug).
	if slug == "." || slug == ".." ||
		strings.ContainsAny(slug, "/"+string(filepath.Separator)) ||
		filepath.Base(slug) != slug {
		return "", &PathTraversalError{
			Slug:       slug,
			ExfilRoot:  cleanRoot,
			ResolvedTo: filepath.Join(cleanRoot, slug, "ISA.md"),
		}
	}

	candidate := filepath.Join(cleanRoot, slug, "ISA.md")
	cleanCandidate := filepath.Clean(candidate)

	// Layer 3: belt + suspenders. The Clean'd candidate must live strictly
	// under cleanRoot. The `+ separator` suffix avoids the /foo vs /foobar
	// false-positive.
	prefix := cleanRoot + string(filepath.Separator)
	if !strings.HasPrefix(cleanCandidate, prefix) {
		return "", &PathTraversalError{
			Slug:       slug,
			ExfilRoot:  cleanRoot,
			ResolvedTo: cleanCandidate,
		}
	}

	return cleanCandidate, nil
}

// Render builds the full markdown document for an ISA. Pure function: no I/O,
// no env, no clock. The output ends with exactly one trailing newline.
//
// Returns *InputError when slug/id/title are missing — the rendered document
// is meaningless without them and we refuse to write a corrupt file.
func Render(in *RenderInput) (string, error) {
	if in == nil {
		return "", &InputError{Msg: "nil RenderInput"}
	}
	if in.ID == "" {
		return "", &InputError{Msg: "cannot render ISA with empty id"}
	}
	if in.Slug == "" {
		return "", &InputError{Msg: "cannot render ISA with empty slug"}
	}
	// Title may be empty in extreme edge cases (an ISA created directly via
	// SQL bypassing `bd brain new`). We tolerate this — emit empty `task: ""`
	// and `# ` header — because the renderer's job is to faithfully reflect
	// the substrate, not to refuse legitimate but odd rows.

	var buf bytes.Buffer
	writeFrontmatter(&buf, in)
	buf.WriteString("\n")
	buf.WriteString("# ")
	buf.WriteString(in.Title)
	buf.WriteString("\n\n")
	writeSections(&buf, in.Sections)

	// Guarantee a single trailing newline. writeSections may already have left
	// one; collapse any run of trailing newlines to exactly one.
	out := buf.String()
	out = strings.TrimRight(out, "\n") + "\n"
	return out, nil
}

// writeFrontmatter emits the YAML block with the eight IsaFormat v2.7 keys
// plus brain_id. NULL keys are omitted per the spec. The serializer is
// hand-rolled (rather than gopkg.in/yaml.v3) because the surface is fixed and
// tiny — nine scalar keys — and avoiding the dep keeps the substrate package
// lightweight.
//
// Key order is the IsaFormat v2.7 contract order: task, slug, effort, phase,
// progress, mode, started, updated, brain_id. Tests pin this byte-for-byte.
func writeFrontmatter(buf *bytes.Buffer, in *RenderInput) {
	buf.WriteString("---\n")

	// task — always quoted because titles routinely contain colons, hashes,
	// and other YAML metacharacters.
	buf.WriteString("task: ")
	buf.WriteString(yamlQuote(in.Title))
	buf.WriteString("\n")

	// slug — quoted for the same reason, even though slugs are constrained.
	buf.WriteString("slug: ")
	buf.WriteString(yamlQuote(in.Slug))
	buf.WriteString("\n")

	if in.Effort != nil {
		buf.WriteString("effort: ")
		buf.WriteString(*in.Effort)
		buf.WriteString("\n")
	}

	if in.Phase != nil {
		buf.WriteString("phase: ")
		buf.WriteString(strings.ToLower(*in.Phase))
		buf.WriteString("\n")
	}

	// progress is emitted unconditionally, even when both halves are zero —
	// the spec rule is `progress: "0/0"` literal, not omitted.
	buf.WriteString(fmt.Sprintf("progress: \"%d/%d\"\n", in.ProgressM, in.ProgressN))

	if in.Mode != nil {
		buf.WriteString("mode: ")
		buf.WriteString(*in.Mode)
		buf.WriteString("\n")
	}

	if in.StartedAt != nil {
		buf.WriteString("started: ")
		buf.WriteString(in.StartedAt.UTC().Format(time.RFC3339))
		buf.WriteString("\n")
	}

	if in.UpdatedAt != nil {
		buf.WriteString("updated: ")
		buf.WriteString(in.UpdatedAt.UTC().Format(time.RFC3339))
		buf.WriteString("\n")
	}

	// brain_id — the F2 addition that lets downstream tools trace
	// markdown → substrate row. Always present.
	buf.WriteString("brain_id: ")
	buf.WriteString(in.ID)
	buf.WriteString("\n")

	buf.WriteString("---\n")
}

// writeSections walks the twelve canonical sections in spec order and emits
// `## <Title>` + body for every section with a non-empty body. Empty or
// missing sections are skipped entirely — no blank heading. Bodies are
// emitted verbatim (no trim, no normalization) so changelog timestamps,
// code-fence indentation, and trailing whitespace round-trip cleanly.
//
// Between sections, exactly one blank line. If the body lacks a trailing
// newline, one is added so the next `## ` heading starts on its own line.
func writeSections(buf *bytes.Buffer, sections map[string]string) {
	first := true
	for _, name := range isashow.SpecOrderedSectionNames() {
		body, ok := sections[name]
		if !ok {
			continue
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		if !first {
			buf.WriteString("\n")
		}
		first = false
		buf.WriteString("## ")
		buf.WriteString(sectionTitles[name])
		buf.WriteString("\n\n")
		buf.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			buf.WriteString("\n")
		}
	}
}

// yamlQuote wraps a string in double quotes and escapes the two characters
// that change YAML scalar parsing inside a double-quoted scalar: backslash
// and double-quote. Newlines and other control chars are not expected in
// titles or slugs; if a future caller passes one, YAML's flow scalar grammar
// will still parse the result, just rendered on one line with an explicit
// `\n` escape.
func yamlQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// CheckWriteAllowed decides whether a render may write over targetPath. It
// enforces the "disk-canonical" doctrine (PAI 2026-06-25): an ISA.md that a
// human/agent authored directly on disk is the source of truth, and the
// IsaLifter hook lifts it INTO brain — a render must never clobber it.
//
// The discriminator is the `brain_id: <id>` frontmatter line. Every rendered
// file carries it (writeFrontmatter emits it unconditionally); agent-authored
// disk files do not. So:
//
//   - target does not exist                      → allowed (nothing to clobber)
//   - target exists, frontmatter brain_id == id  → allowed (refreshing a prior
//     render of this same row)
//   - target exists, no brain_id in frontmatter  → BLOCKED (hand-authored,
//     disk-canonical)
//   - target exists, brain_id != id              → BLOCKED (slug collision:
//     another row already owns this path; do not clobber it)
//
// The brain_id must appear inside the leading `---`-delimited frontmatter
// block; a `brain_id:` line elsewhere in the body does not count.
//
// A stat/read error other than not-exist is returned as err (fail closed — the
// caller must not silently overwrite a file it could not inspect). When allowed
// is false, reason is a human-readable divergence description and err is nil.
func CheckWriteAllowed(targetPath, brainID string) (allowed bool, reason string, err error) {
	data, readErr := os.ReadFile(targetPath)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return true, "", nil
		}
		return false, "", fmt.Errorf("inspecting existing render target %s: %w", targetPath, readErr)
	}

	existingID, ok := frontmatterBrainID(data)
	if ok && existingID == brainID {
		return true, "", nil
	}
	if !ok {
		return false, fmt.Sprintf(
			"disk-canonical file present at %s (no brain_id frontmatter — hand-authored, not overwriting)",
			targetPath), nil
	}
	return false, fmt.Sprintf(
		"disk-canonical file present at %s (brain_id %q belongs to a different row, not %q — slug collision, not overwriting)",
		targetPath, existingID, brainID), nil
}

// frontmatterBrainID extracts the `brain_id` value from the leading
// `---`-delimited YAML frontmatter block. Returns (value, true) when a
// `brain_id:` line is found before the closing delimiter; ("", false) when the
// file has no valid leading frontmatter block or no brain_id line within it.
//
// The match is deliberately strict: the file must open with a `---` line
// (rendered files always do), and the key must be at the start of the line
// (rendered files emit `brain_id: <id>` with no indentation). This avoids a
// `brain_id:` reference in prose or an indented mapping being mistaken for the
// frontmatter key. CRLF line endings are tolerated.
func frontmatterBrainID(data []byte) (string, bool) {
	const key = "brain_id:"
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", false
	}
	for _, raw := range lines[1:] {
		line := strings.TrimRight(raw, "\r")
		if line == "---" {
			// Closing delimiter: end of frontmatter block, no brain_id found.
			return "", false
		}
		if strings.HasPrefix(line, key) {
			return strings.TrimSpace(line[len(key):]), true
		}
	}
	// No closing delimiter — treat as no valid frontmatter (fail closed).
	return "", false
}

// WriteAtomic writes content to targetPath using a temp-file + rename(2)
// sequence so readers never observe a half-written file. The temp file lives
// next to targetPath (same directory, same filesystem) so rename(2) is
// guaranteed atomic on POSIX.
//
//  1. Ensure the parent directory exists (0755).
//  2. Write content to <targetPath>.tmp.<pid>.<unixnanos> with mode 0644.
//  3. fsync the temp file so the data hits disk before the rename publishes it.
//     (We are deliberately conservative — render is rare and durability matters
//     more than write throughput.)
//  4. os.Rename(temp, target).
//
// On any failure before rename, the temp file is removed; on rename failure
// we also remove it. This means a failed render leaves the previous target
// untouched (atomicity) and no temp turds (cleanliness).
func WriteAtomic(targetPath, content string) error {
	if targetPath == "" {
		return &InputError{Msg: "WriteAtomic: empty targetPath"}
	}

	parentDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", parentDir, err)
	}

	tmpName := fmt.Sprintf("%s.tmp.%d.%d",
		filepath.Base(targetPath), os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(parentDir, tmpName)

	// Open with O_EXCL so a colliding temp file (impossible in practice with
	// pid+nanos but cheap to guarantee) surfaces as an error rather than a
	// silent overwrite.
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create temp %s: %w", tmpPath, err)
	}

	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("write temp %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("fsync temp %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, targetPath, err)
	}
	return nil
}
