package exfiltrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEntry is a test helper that writes a fixture entry markdown file with
// a frontmatter `title` at path (creating parent dirs).
func writeEntry(t *testing.T, path, title string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nid: bd-x\ntitle: " + yamlString(title) + "\nkind: knowledge\ntype: knowledge\n---\n\n# " + title + "\n\nbody\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildFixtureTree lays down a small kinded entries/ tree:
//
//	entries/
//	  knowledge/
//	    gamma.md   (title "Gamma note")
//	    alpha.md   (title "Alpha note")
//	  task/
//	    beta.md    (title "Beta task")
//	  log.md            (reserved — must be excluded)
//	  .checkpoint.json  (dotfile — must be excluded)
//
// It returns the store root (the parent of entries/).
func buildFixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	entries := filepath.Join(root, "entries")

	writeEntry(t, filepath.Join(entries, "knowledge", "gamma.md"), "Gamma note")
	writeEntry(t, filepath.Join(entries, "knowledge", "alpha.md"), "Alpha note")
	writeEntry(t, filepath.Join(entries, "task", "beta.md"), "Beta task")

	// Reserved / bookkeeping files that must never appear in a listing.
	if err := os.WriteFile(filepath.Join(entries, "log.md"), []byte("# Log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entries, ".checkpoint.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestWriteIndexes_RootHasOKFVersion asserts the bundle-root index.md carries
// the okf_version frontmatter (ISC-9) followed by the listing, and that the
// listing links the kind subdirectories with a trailing slash.
func TestWriteIndexes_RootHasOKFVersion(t *testing.T) {
	root := buildFixtureTree(t)
	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes: %v", err)
	}

	got := readFile(t, filepath.Join(root, "entries", "index.md"))

	wantPrefix := "---\nokf_version: \"0.1\"\n---\n\n# Entries\n\n"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("root index.md prefix mismatch.\n got: %q\nwant prefix: %q", got, wantPrefix)
	}
	// Subdirs listed with a trailing slash, sorted by name.
	if !strings.Contains(got, "* [knowledge](knowledge/)\n") {
		t.Errorf("root index missing knowledge subdir bullet; got:\n%s", got)
	}
	if !strings.Contains(got, "* [task](task/)\n") {
		t.Errorf("root index missing task subdir bullet; got:\n%s", got)
	}
	// knowledge must sort before task.
	if strings.Index(got, "knowledge/)") > strings.Index(got, "task/)") {
		t.Errorf("subdirs not sorted (knowledge should precede task); got:\n%s", got)
	}
}

// TestWriteIndexes_NestedNoFrontmatter asserts nested <kind>/index.md files
// carry NO frontmatter (ISC-8 / OKF), use the capitalized kind as heading,
// list entries by frontmatter title sorted alphabetically, and exclude the
// reserved files.
func TestWriteIndexes_NestedNoFrontmatter(t *testing.T) {
	root := buildFixtureTree(t)
	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes: %v", err)
	}

	knowledge := readFile(t, filepath.Join(root, "entries", "knowledge", "index.md"))

	if strings.HasPrefix(knowledge, "---") {
		t.Errorf("nested knowledge/index.md must not carry frontmatter; got:\n%s", knowledge)
	}
	if !strings.HasPrefix(knowledge, "# Knowledge\n\n") {
		t.Errorf("nested index heading mismatch; got:\n%s", knowledge)
	}
	// Titles come from frontmatter, sorted: Alpha note before Gamma note.
	wantBody := "# Knowledge\n\n* [Alpha note](alpha.md)\n* [Gamma note](gamma.md)\n"
	if knowledge != wantBody {
		t.Errorf("nested knowledge/index.md body mismatch.\n got: %q\nwant: %q", knowledge, wantBody)
	}
}

// TestWriteIndexes_ExcludesReservedFiles asserts log.md, the dotfile
// checkpoint, and the generated index.md itself never appear in any listing.
func TestWriteIndexes_ExcludesReservedFiles(t *testing.T) {
	root := buildFixtureTree(t)
	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes: %v", err)
	}

	rootIdx := readFile(t, filepath.Join(root, "entries", "index.md"))
	for _, forbidden := range []string{"log.md", ".checkpoint.json", "(index.md)"} {
		if strings.Contains(rootIdx, forbidden) {
			t.Errorf("root index must not list %q; got:\n%s", forbidden, rootIdx)
		}
	}
}

// TestWriteIndexes_ByteStable asserts two consecutive runs produce
// byte-identical index.md files everywhere (ISC-3 idempotence extended to
// the scaffolding pass) — including that the second run does not list the
// index.md written by the first run.
func TestWriteIndexes_ByteStable(t *testing.T) {
	root := buildFixtureTree(t)

	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes (run 1): %v", err)
	}
	first := snapshotIndexes(t, root)

	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes (run 2): %v", err)
	}
	second := snapshotIndexes(t, root)

	if len(first) != len(second) {
		t.Fatalf("index count changed between runs: %d → %d", len(first), len(second))
	}
	for path, b1 := range first {
		b2, ok := second[path]
		if !ok {
			t.Errorf("index %s present in run 1 but missing in run 2", path)
			continue
		}
		if b1 != b2 {
			t.Errorf("index %s not byte-stable.\n run1: %q\n run2: %q", path, b1, b2)
		}
	}
}

// snapshotIndexes reads every index.md under <root>/entries/ into a map keyed
// by path.
func snapshotIndexes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries := filepath.Join(root, "entries")
	err := filepath.WalkDir(entries, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "index.md" {
			out[path] = readFile(t, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestWriteIndexes_FlatStore asserts a flat store (BRAIN_EXFIL_FLAT-style
// layout: entries/ holds slug files directly, no kind subdirs) gets exactly
// one index — the root — carrying okf_version and listing the flat files.
func TestWriteIndexes_FlatStore(t *testing.T) {
	root := t.TempDir()
	entries := filepath.Join(root, "entries")
	writeEntry(t, filepath.Join(entries, "one.md"), "First")
	writeEntry(t, filepath.Join(entries, "two.md"), "Second")

	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes: %v", err)
	}

	idx := readFile(t, filepath.Join(entries, "index.md"))
	if !strings.HasPrefix(idx, "---\nokf_version: \"0.1\"\n---\n\n# Entries\n\n") {
		t.Errorf("flat root index missing okf_version/heading; got:\n%s", idx)
	}
	want := "* [First](one.md)\n* [Second](two.md)\n"
	if !strings.HasSuffix(idx, want) {
		t.Errorf("flat root index listing mismatch; got:\n%s", idx)
	}
	// No nested index files should exist in a flat store.
	all := snapshotIndexes(t, root)
	if len(all) != 1 {
		t.Errorf("flat store should have exactly 1 index.md, got %d: %v", len(all), keys(all))
	}
}

// TestWriteIndexes_MissingEntriesRootIsNoop asserts a store that has never
// rendered (no entries/ dir) is a no-op, not an error.
func TestWriteIndexes_MissingEntriesRootIsNoop(t *testing.T) {
	root := t.TempDir()
	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes on a store with no entries/ should be a no-op, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "entries", "index.md")); !os.IsNotExist(err) {
		t.Errorf("expected no index.md to be created when entries/ is absent")
	}
}

// TestWriteIndexes_TitleFallbackToSlug asserts that an entry whose
// frontmatter has no usable title falls back to the slug (filename without
// .md) for its link text.
func TestWriteIndexes_TitleFallbackToSlug(t *testing.T) {
	root := t.TempDir()
	entries := filepath.Join(root, "entries", "knowledge")
	if err := os.MkdirAll(entries, 0o755); err != nil {
		t.Fatal(err)
	}
	// No title key in frontmatter.
	notitle := "---\nid: bd-x\nkind: knowledge\ntype: knowledge\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(entries, "no-title-note.md"), []byte(notitle), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteIndexes(root); err != nil {
		t.Fatalf("WriteIndexes: %v", err)
	}
	idx := readFile(t, filepath.Join(entries, "index.md"))
	if !strings.Contains(idx, "* [no-title-note](no-title-note.md)\n") {
		t.Errorf("expected slug fallback link text; got:\n%s", idx)
	}
}

// TestFrontmatterTitle exercises the small frontmatter title parser directly.
func TestFrontmatterTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"double-quoted", "---\ntitle: \"Hello World\"\n---\n", "Hello World"},
		{"escaped-quote", "---\ntitle: \"She said \\\"hi\\\"\"\n---\n", `She said "hi"`},
		{"single-quoted", "---\ntitle: 'Plain'\n---\n", "Plain"},
		{"bare", "---\ntitle: bare value\n---\n", "bare value"},
		{"absent", "---\nid: x\n---\n", ""},
		{"no-frontmatter", "# just a heading\n", ""},
		{"empty", "", ""},
		{"opening-no-closing", "---\ntitle: \"X\"\nno close", "X"},
		{"bom", "\xEF\xBB\xBF---\ntitle: \"B\"\n---\n", "B"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := frontmatterTitle([]byte(tc.in)); got != tc.want {
				t.Errorf("frontmatterTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSortIndexEntries confirms case-insensitive alphabetical ordering by
// title with a link-target tie-break.
func TestSortIndexEntries(t *testing.T) {
	in := []indexEntry{
		{Title: "banana", LinkTarget: "b.md"},
		{Title: "Apple", LinkTarget: "a.md"},
		{Title: "apple", LinkTarget: "a2.md"},
		{Title: "Cherry", LinkTarget: "c/"},
	}
	sortIndexEntries(in)
	gotTitles := []string{in[0].Title, in[1].Title, in[2].Title, in[3].Title}
	// "Apple"/"apple" tie on lowercase title; capital sorts first via the
	// case-sensitive secondary key. Then banana, then Cherry.
	want := []string{"Apple", "apple", "banana", "Cherry"}
	for i := range want {
		if gotTitles[i] != want[i] {
			t.Errorf("sort order mismatch at %d: got %q want %q (full: %v)", i, gotTitles[i], want[i], gotTitles)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
