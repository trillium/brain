package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// reservedFiles are OKF-reserved filenames that legitimately carry no
// frontmatter, so they are exempt from the #1/#2 `type` requirements.
// Compared case-insensitively against the base filename.
//
// index.md is reserved for #1/#2 but is STILL subject to the #3 check
// (it must not carry a frontmatter block) — that is handled explicitly in
// checkFile, not by this map.
var reservedFiles = map[string]bool{
	"index.md": true,
	"log.md":   true,
}

// Violation is a single conformance failure: the offending file and the
// human-readable reason it failed.
type Violation struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// Report is the aggregate result of a Check run.
type Report struct {
	Checked    int         `json:"checked"`
	Passed     int         `json:"passed"`
	Failed     int         `json:"failed"`
	Violations []Violation `json:"violations"`
}

// Check walks each of the given paths for `.md` files and validates OKF
// v0.1 conformance. A path may be a directory (walked recursively) or a
// single `.md` file. It returns a Report collecting ALL violations.
//
// A non-nil error is returned only for I/O failures that prevent the walk
// from completing (e.g. a path that does not exist). Per-file parse/shape
// failures are recorded as Violations, not returned as errors — that is
// what lets the caller collect every violation and report them together.
func Check(paths []string) (*Report, error) {
	rep := &Report{Violations: []Violation{}}
	seen := make(map[string]bool)

	files, err := collectMarkdownFiles(paths)
	if err != nil {
		return nil, err
	}

	for _, path := range files {
		if seen[path] {
			// A file reachable via two overlapping input paths (e.g. a
			// store root and its own entries/ dir) is checked once.
			continue
		}
		seen[path] = true

		rep.Checked++
		if v, ok := checkFile(path); !ok {
			rep.Failed++
			rep.Violations = append(rep.Violations, v)
		} else {
			rep.Passed++
		}
	}

	// Deterministic ordering so text/JSON output and tests are stable
	// regardless of filesystem walk order.
	sort.Slice(rep.Violations, func(i, j int) bool {
		return rep.Violations[i].File < rep.Violations[j].File
	})

	return rep, nil
}

// collectMarkdownFiles resolves the input paths to a deduplicated list of
// `.md` files to check. Directories are walked recursively; a single `.md`
// file path is included directly. Dotfiles/dot-dirs and `.checkpoint.json`
// are skipped during the walk.
func collectMarkdownFiles(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}

		if !info.IsDir() {
			// An explicitly named single file: include it if it's a
			// markdown file, otherwise it's a user error worth surfacing.
			if strings.EqualFold(filepath.Ext(p), ".md") {
				out = append(out, p)
				continue
			}
			return nil, fmt.Errorf("not a markdown file or directory: %s", p)
		}

		walkErr := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			// Skip dotfiles and dot-directories (e.g. .git, .obsidian,
			// .checkpoint.json is a dotfile so it's covered here too).
			if name != "." && strings.HasPrefix(name, ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if strings.EqualFold(filepath.Ext(name), ".md") {
				out = append(out, path)
			}
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk %s: %w", p, walkErr)
		}
	}
	return out, nil
}

// checkFile validates one `.md` file against the OKF v0.1 requirements this
// checker covers. It returns a zero-value Violation and ok=true when the
// file conforms, or a populated Violation and ok=false when it does not.
//
// Rules:
//   - index.md (reserved): must NOT carry a frontmatter block (#3). It is
//     exempt from the type check.
//   - log.md (reserved): exempt from all checks (no frontmatter expected).
//   - every other .md: must have a parseable YAML frontmatter block (#1)
//     with a non-empty `type` (#2).
func checkFile(path string) (Violation, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Violation{File: path, Reason: fmt.Sprintf("unreadable: %v", err)}, false
	}

	base := strings.ToLower(filepath.Base(path))
	fmBytes, hasFrontmatter := extractFrontmatter(data)

	// #3: a present index.md must NOT carry a frontmatter block.
	if base == "index.md" {
		if hasFrontmatter {
			return Violation{
				File:   path,
				Reason: "OKF #3: index.md is reserved and must not carry a frontmatter block",
			}, false
		}
		return Violation{}, true
	}

	// Other reserved files (log.md) are exempt from the type check.
	if reservedFiles[base] {
		return Violation{}, true
	}

	// #1: a parseable frontmatter block must be present.
	if !hasFrontmatter {
		return Violation{
			File:   path,
			Reason: "OKF #1: missing YAML frontmatter block (--- ... --- at file start)",
		}, false
	}

	var fm map[string]any
	if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
		return Violation{
			File:   path,
			Reason: fmt.Sprintf("OKF #1: frontmatter does not parse as YAML: %v", err),
		}, false
	}

	// #2: `type` must be present and non-empty.
	if !hasNonEmptyType(fm) {
		return Violation{
			File:   path,
			Reason: "OKF #2: frontmatter is missing a non-empty `type` field",
		}, false
	}

	return Violation{}, true
}

// extractFrontmatter returns the bytes between the leading `---` fence and
// the closing `---` fence at the start of a markdown file, and whether a
// well-formed block was found.
//
// A frontmatter block must:
//   - begin on the very first line with a `---` fence, and
//   - be terminated by a subsequent `---` fence line.
//
// A leading UTF-8 BOM is tolerated. An opening fence with no closing fence
// is treated as "no frontmatter" (hasFrontmatter=false) — the file has no
// valid block, which #1 will flag for non-reserved files.
func extractFrontmatter(data []byte) (frontmatter []byte, ok bool) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Frontmatter blocks are small; the default 64KB buffer is ample, but
	// raise the max to be safe against a pathological single long line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if !scanner.Scan() {
		return nil, false // empty file
	}
	if strings.TrimRight(scanner.Text(), " \t\r") != "---" {
		return nil, false // first line is not an opening fence
	}

	var body bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimRight(line, " \t\r") == "---" {
			return body.Bytes(), true // closing fence found
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	// Opening fence but no closing fence: not a valid block.
	return nil, false
}

// hasNonEmptyType reports whether the parsed frontmatter carries a `type`
// key whose value is a non-empty, non-whitespace string. A `type` that
// parses to a non-string (e.g. a list) or to an empty/whitespace string is
// treated as absent — OKF requires a non-empty type value.
func hasNonEmptyType(fm map[string]any) bool {
	v, present := fm["type"]
	if !present || v == nil {
		return false
	}
	s, isString := v.(string)
	if !isString {
		return false
	}
	return strings.TrimSpace(s) != ""
}

// WriteText renders a human-readable report to w.
func (r *Report) WriteText(w io.Writer) {
	if r.Failed == 0 {
		fmt.Fprintf(w, "okf-check: OK — %d file(s) checked, all conformant\n", r.Checked)
		return
	}
	fmt.Fprintf(w, "okf-check: FAIL — %d/%d file(s) violate OKF v0.1 conformance:\n", r.Failed, r.Checked)
	for _, v := range r.Violations {
		fmt.Fprintf(w, "  ✗ %s\n      %s\n", v.File, v.Reason)
	}
}

// WriteJSON renders the machine-readable report to w.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
