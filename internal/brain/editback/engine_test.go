package editback

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/types"
)

func newTStore() *fakeStore {
	return &fakeStore{issues: map[string]*types.Issue{
		"brain-k000a": newIssue("brain-k000a", "Accepted subject", "knowledge"),
		"brain-k000b": newIssue("brain-k000b", "Declined subject", "knowledge"),
		"brain-k000c": newIssue("brain-k000c", "Gone file subject", "knowledge"),
		"brain-k000d": newIssue("brain-k000d", "Pre-marked subject", "knowledge"),
	}}
}

func baseOptions(t *testing.T, st *fakeStore, accepts bool) Options {
	t.Helper()
	return Options{
		Root:               t.TempDir(),
		Namespace:          "brain",
		NamespaceAuthority: ownedNamespace,
		AcceptsEdits:       accepts,
		Store:              st,
		Actor:              "test",
	}
}

// TestAcceptedEdit_PassesIntoBead — the headline: an edit to a rendered
// file updates the bead it names when the store accepts edit-back, and the
// run reports old → new so the losing side of the disagreement is visible.
func TestAcceptedEdit_PassesIntoBead(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	path := renderFile(t, st, o.Root, "brain-k000a", func(got string) string {
		return strings.Replace(got, "title: \"Accepted subject\"", "title: \"Edited subject\"", 1)
	})
	seedManifest(t, o.Root, "brain-k000a", relEntry(t, o.Root, path))

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "applied")
	if oc.Bead != "brain-k000a" {
		t.Fatalf("applied outcome targets %q", oc.Bead)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.issues["brain-k000a"].Title != "Edited subject" {
		t.Fatalf("bead title did not change: %q", st.issues["brain-k000a"].Title)
	}
	if len(st.updates) != 1 {
		t.Fatalf("unexpected update calls: %+v", st.updates)
	}
	var titleChange *Change
	for i, c := range oc.Changes {
		if c.Field == "title" {
			titleChange = &oc.Changes[i]
		}
	}
	if titleChange == nil || titleChange.From != "Accepted subject" || titleChange.To != "Edited subject" {
		t.Fatalf("change trail wrong: %+v", oc.Changes)
	}
}

// TestDeclinedStore_ReportsAndLeavesAlone — the same edit in a store that
// does not declare edit-back is reported (full old → new trail) and written
// nowhere.
func TestDeclinedStore_ReportsAndLeavesAlone(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, false)
	path := renderFile(t, st, o.Root, "brain-k000b", func(got string) string {
		return strings.Replace(got, "title: \"Declined subject\"", "title: \"User-edit subject\"", 1)
	})
	seedManifest(t, o.Root, "brain-k000b", relEntry(t, o.Root, path))

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "edit-ignored")
	if oc.Bead != "brain-k000b" || len(oc.Changes) == 0 {
		t.Fatalf("report shape wrong: %+v", oc)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.issues["brain-k000b"].Title != "Declined subject" {
		t.Fatalf("substrate was written in a one-way store: %q", st.issues["brain-k000b"].Title)
	}
	if len(st.updates) != 0 {
		t.Fatalf("unexpected update calls: %+v", st.updates)
	}
}

// TestApplied_FullFieldEdit — description, status, priority ride the same apply.
func TestApplied_FullFieldEdit(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	path := renderFile(t, st, o.Root, "brain-k000a", func(got string) string {
		return strings.NewReplacer(
			"title: \"Accepted subject\"", "title: \"Full edit\"",
			"status: open", "status: in-progress",
			"priority: 2", "priority: 3",
			"# Accepted subject", "# Full edit",
		).Replace(got)
	})
	seedManifest(t, o.Root, "brain-k000a", relEntry(t, o.Root, path))

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "applied")
	fields := map[string]bool{}
	for _, c := range oc.Changes {
		fields[strings.Split(c.Field, " ")[0]] = true
	}
	for _, want := range []string{"title", "status", "priority"} {
		if !fields[want] {
			t.Errorf("expected a %s change in %+v", want, oc.Changes)
		}
	}
}

// TestDeletion_MarksNeverDeletes — deleting a rendered file marks its bead
// and never removes it.
func TestDeletion_MarksNeverDeletes(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, false)

	// Seed the manifest as though the file was once rendered, then present
	// a directory where it is gone.
	seedManifest(t, o.Root, "brain-k000c", "entries/knowledge/"+testSlug("brain-k000c")+".md")

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "marked")
	if oc.Bead != "brain-k000c" {
		t.Fatalf("marked the wrong bead: %q", oc.Bead)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !slicesContains(st.issues["brain-k000c"].Labels, exfiltrator.DeletionMarkLabel) {
		t.Fatal("bead does not carry the deletion mark")
	}
	if len(st.adds) != 1 || st.adds[0].Label != exfiltrator.DeletionMarkLabel {
		t.Fatalf("label writes wrong: %+v", st.adds)
	}
	if st.getAny("brain-k000c") == nil {
		t.Fatal("the substrate lost the bead — deletion marks never delete")
	}
}

func (f *fakeStore) getAny(id string) *types.Issue {
	issue, _ := f.issues[id]
	return issue
}

// TestDeletion_AlreadyMarkedIsIdempotent — a second pass after a mark
// reports already-marked rather than re-labelling.
func TestDeletion_AlreadyMarkedIsIdempotent(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, false)
	seedManifest(t, o.Root, "brain-k000d", "entries/knowledge/"+testSlug("brain-k000d")+".md")

	// Pre-mark via the mechanism itself once.
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(byKind(second, "marked")) != 0 {
		t.Fatalf("double marked: %+v", byKind(second, "marked"))
	}
	singleOutcome(t, second, "already-marked")
}

// TestDeletion_MissingManifestRefuses — with no manifest the deletion scan
// is available-at-nothing and says so, rather than guessing.
func TestDeletion_MissingManifestRefuses(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	if err := os.MkdirAll(filepath.Join(o.Root, "entries"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.DeletionUnavailable == "" {
		t.Fatal("expected the deletion scan to report itself unavailable")
	}
	if !strings.Contains(rep.DeletionUnavailable, "refusing") || !strings.Contains(rep.DeletionUnavailable, "manifest") {
		t.Fatalf("refusal does not name the mechanism: %q", rep.DeletionUnavailable)
	}
}

// TestRefused_UnknownBeadNamed — a file naming a bead that does not exist
// is refused, by name.
func TestRefused_UnknownBeadNamed(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	dir := filepath.Join(o.Root, "entries", "knowledge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "orphan.md")
	body := "---\nid: brain-kzzz99\nslug: \"orphan\"\ntitle: \"Orphan\"\n---\n\n# Orphan\n\nbody\n"
	if err := os.WriteFile(orphan, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	seedManifest(t, o.Root, "brain-k000a", "entries/knowledge/"+testSlug("brain-k000a")+".md")

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "refused:unknown-bead")
	if oc.Bead != "brain-kzzz99" {
		t.Fatalf("refusal should name the file's bead: %+v", oc)
	}
}

// TestRefused_NoName — frontmatter that names nothing is a named refusal.
func TestRefused_NoName(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	dir := filepath.Join(o.Root, "entries", "knowledge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	nameless := filepath.Join(dir, "nameless.md")
	body := "---\ntitle: \"No name\"\nslug: \"nameless\"\n---\n\nbody\n"
	if err := os.WriteFile(nameless, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	seedManifest(t, o.Root, "brain-k000a", "entries/knowledge/"+testSlug("brain-k000a")+".md")

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "refused:no-name")
	if !strings.Contains(oc.Refusal, "names nothing") {
		t.Fatalf("refusal should say the file names nothing: %q", oc.Refusal)
	}
}

// TestRefused_OutsideNamespace — a file whose bead is in another store's
// namespace is refused with the prefix's owner.
func TestRefused_OutsideNamespace(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	dir := filepath.Join(o.Root, "entries", "knowledge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	alien := filepath.Join(dir, "alien.md")
	body := "---\nid: task-x1234\nslug: \"alien\"\ntitle: \"Alien\"\n---\n\n# Alien\n\nbody\n"
	if err := os.WriteFile(alien, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	seedManifest(t, o.Root, "brain-k000a", "entries/knowledge/"+testSlug("brain-k000a")+".md")

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "refused:outside-namespace")
	if !strings.Contains(oc.Refusal, "task-x1234") {
		t.Fatalf("refusal should name the file's bead and its owner: %+v", oc)
	}
}

// TestRefused_SlugMismatch — a file whose slug disagrees with the bead it
// names is refused rather than guessed.
func TestRefused_SlugMismatch(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	dir := filepath.Join(o.Root, "entries", "knowledge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mismatch := filepath.Join(dir, "wrong-slug.md")
	body := "---\nid: brain-k000a\nslug: \"wrong-slug\"\ntitle: \"Accepted subject\"\n---\n\n# Accepted subject\n\n" + st.issues["brain-k000a"].Description + "\n"
	if err := os.WriteFile(mismatch, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	seedManifest(t, o.Root, "brain-k000a", "entries/knowledge/"+testSlug("brain-k000a")+".md")

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	oc := singleOutcome(t, rep, "refused:slug-mismatch")
	if !strings.Contains(oc.Refusal, "wrong-slug") {
		t.Fatalf("refusal should name the wrong slug: %+v", oc)
	}
}

// TestRefused_EditedWhileMarked — an edit must not slip inside a pending
// deletion intent, the two human signals would collide.
func TestRefused_EditedWhileMarked(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	path := renderFile(t, st, o.Root, "brain-k000d", func(got string) string {
		return strings.Replace(got, "title: \"Pre-marked subject\"", "title: \"Raced subject\"", 1)
	})
	if err := os.MkdirAll(filepath.Dir(filepath.Join(o.Root, "entries", exfiltrator.ManifestFilename)), 0o755); err != nil {
		t.Fatal(err)
	}
	seedManifest(t, o.Root, "brain-k000d", relEntry(t, o.Root, path))
	mI, _ := st.GetIssue(context.Background(), "brain-k000d")
	if mI == nil {
		t.Fatal("setup: missing bead")
	}
	mI.Labels = []string{exfiltrator.DeletionMarkLabel}

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	singleOutcome(t, rep, "refused:edited-while-marked")
}

// TestIdempotentSecondRun — after the apply, the row equals the file, so a
// second run finds nothing to change.
func TestIdempotentSecondRun(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	renderFile(t, st, o.Root, "brain-k000a", nil)
	root := o.Root
	path := filepath.Join(root, "entries", "knowledge", testSlug("brain-k000a")+".md")
	seedManifest(t, root, "brain-k000a", relEntry(t, root, path))

	first, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	for _, oc := range first.Outcomes {
		if strings.HasPrefix(oc.Kind, "refused") || oc.Kind == "applied" {
			t.Logf("first pass: %s %s %+v", oc.Kind, oc.Bead, oc.Changes)
		}
	}
	second, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := byKind(second, "applied"); len(got) != 0 {
		t.Fatalf("second pass was not idempotent: %+v", got)
	}
	singleOutcome(t, second, "clean")
}

// TestFilePresentSkippedQuiet — the default report doesn't flood with
// manifest entries whose file still exists.
func TestFilePresentSkippedQuiet(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	o.QuietPresent = true
	renderFile(t, st, o.Root, "brain-k000a", nil)
	path := filepath.Join(o.Root, "entries", "knowledge", testSlug("brain-k000a")+".md")
	seedManifest(t, o.Root, "brain-k000a", relEntry(t, o.Root, path))

	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(byKind(rep, "file-present")) != 0 {
		t.Fatal("quiet mode should suppress file-present chatter")
	}
	o.QuietPresent = false
	rep2, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	singleOutcome(t, rep2, "file-present")
}

// ── Frontmatter parser ────────────────────────────────────────────

func TestParseFrontmatter_RenderedShape(t *testing.T) {
	body := "---\ntitle: \"Hello, world\"\nlabels: [\"one\", \"two-words\"]\ntags: [\"one\", \"two-words\"]\n---\n\n# Hello, world\n\nbody text\n"
	fm, rest, err := ParseFrontmatter([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fm.Fields["title"] != "Hello, world" {
		t.Fatalf("title = %q", fm.Fields["title"])
	}
	got, present, ambiguous := FileLabels(fm)
	if !present || ambiguous {
		t.Fatalf("labels present/ambiguous = %v/%v", present, ambiguous)
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two-words" {
		t.Fatalf("labels = %v", got)
	}
	restStr := string(rest)
	if !strings.HasPrefix(restStr, "\n# Hello, world") {
		t.Fatalf("body = %q", restStr)
	}
}

func TestParseFrontmatter_UnclosedFenceRefused(t *testing.T) {
	_, _, err := ParseFrontmatter([]byte("---\ntitle: no closing fence\n"))
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "closing fence") {
		t.Fatalf("refusal should name the defect: %v", err)
	}
}

func TestParseFrontmatter_DuplicatedDisagreeingKey(t *testing.T) {
	_, _, err := ParseFrontmatter([]byte("---\nid: one\nid: two\n---\n"))
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "twice with different values") {
		t.Fatalf("refusal should name the ambiguity: %v", err)
	}
}

func TestSplitBody_H1MatchesFileTitleAfterEdit(t *testing.T) {
	// User changed both frontmatter title and H1 consistently.
	desc, err := SplitBody([]byte("\n\n# New title\n\nBody\n"), "New title", "Old title")
	if err != nil {
		t.Fatalf("split body: %v", err)
	}
	if desc != "Body" {
		t.Fatalf("desc = %q", desc)
	}
}

func TestSplitBody_H1MatchesRowTitleWhenOnlyDescEdited(t *testing.T) {
	desc, err := SplitBody([]byte("\n\n# Old title\n\nNew body\n"), "Old title", "Old title")
	if err != nil {
		t.Fatalf("split body: %v", err)
	}
	if desc != "New body" {
		t.Fatalf("desc = %q", desc)
	}
}

func TestSplitBody_AmbiguousH1Refused(t *testing.T) {
	_, err := SplitBody([]byte("\n\n# Something else\n\nBody\n"), "File title", "Row title")
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "matches neither") {
		t.Fatalf("refusal should name the ambiguity: %v", err)
	}
}

func TestSplitBody_DeletedHeadingRefused(t *testing.T) {
	_, err := SplitBody([]byte("no heading, just body"), "File title", "Row title")
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "no H1") {
		t.Fatalf("refusal should name the defect: %v", err)
	}
}

func TestFilePriority_Unreadable(t *testing.T) {
	if _, _, err := FilePriority(map2FM(map[string]string{"priority": "high"})); err == nil {
		t.Fatal("expected refusal")
	}
}

func TestFilePriority_OutOfRange(t *testing.T) {
	if _, _, err := FilePriority(map2FM(map[string]string{"priority": "9"})); err == nil {
		t.Fatal("expected a 0-4 range refusal")
	}
}

func TestFilePriority_MissingKeyMeansNoChange(t *testing.T) {
	_, present, err := FilePriority(map2FM(map[string]string{}))
	if err != nil || present {
		t.Fatalf("missing key should mean no priority change: present=%v err=%v", present, err)
	}
}

func TestRefusalCodes_AreStableStrings(t *testing.T) {
	// Named refusals are part of the contract; pin them so a rename is a
	// conscious act.
	for _, code := range []string{RefUnknownBead, RefNoName, RefOutsideNamespace, RefCannotParse, RefCannotRead, RefSlugMismatch, RefAmbiguousLabels, RefAmbiguousBody, RefEditedWhileMarked, RefNotManifestedPath, RefStaleFile, RefUnversionedFile} {
		if code == "" || !strings.Contains(code, "-") {
			t.Errorf("refusal code %q lost its shape", code)
		}
	}
}

func _unusedTime(t time.Time) { _ = t }

func map2FM(fields map[string]string) *Frontmatter {
	return &Frontmatter{Fields: fields, Lists: map[string][]string{}}
}

func relEntry(t *testing.T, root, abs string) string {
	t.Helper()
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	_ = os.PathSeparator
	return rel
}

// TestRefused_StaleFile — the substrate stays the authority: a file edited
// from an older render than the row's current state is refused, never
// imported over the newer record, and the refusal names both stamps. A
// store that declines edit-back refuses it too (the stale edit is not
// even worth reporting as a change).
func TestRefused_StaleFile(t *testing.T) {
	for _, accepts := range []bool{true, false} {
		st := newTStore()
		o := baseOptions(t, st, accepts)
		path := renderFile(t, st, o.Root, "brain-k000a", func(got string) string {
			return strings.Replace(got, "title: \"Accepted subject\"", "title: \"Edited from old render\"", 1)
		})
		seedManifest(t, o.Root, "brain-k000a", relEntry(t, o.Root, path))
		// The row moved on after the render the file was edited from.
		st.mu.Lock()
		st.issues["brain-k000a"].UpdatedAt = st.issues["brain-k000a"].UpdatedAt.Add(2 * time.Hour)
		st.mu.Unlock()

		rep, err := Run(context.Background(), o)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		oc := singleOutcome(t, rep, "refused:stale-file")
		if !strings.Contains(oc.Refusal, "2026-10-01T12:00:00Z") || !strings.Contains(oc.Refusal, "2026-10-01T14:00:00Z") {
			t.Errorf("refusal must name both stamps, got %q", oc.Refusal)
		}
		st.mu.Lock()
		if st.issues["brain-k000a"].Title != "Accepted subject" || len(st.updates) != 0 {
			t.Errorf("stale file reached the substrate (accepts=%v): %q %+v", accepts, st.issues["brain-k000a"].Title, st.updates)
		}
		st.mu.Unlock()
	}
}

// TestRefused_UnversionedFile — a file with no (or unreadable) updated
// stamp cannot establish which render it was edited from.
func TestRefused_UnversionedFile(t *testing.T) {
	cases := map[string]func(string) string{
		"missing":  func(got string) string { return removeLinePrefix(got, "updated:") },
		"unparsed": func(got string) string { return replaceLinePrefix(got, "updated:", "updated: yesterday") },
	}
	for name, mutate := range cases {
		st := newTStore()
		o := baseOptions(t, st, true)
		path := renderFile(t, st, o.Root, "brain-k000a", func(got string) string {
			return strings.Replace(mutate(got), "title: \"Accepted subject\"", "title: \"Edited\"", 1)
		})
		seedManifest(t, o.Root, "brain-k000a", relEntry(t, o.Root, path))
		rep, err := Run(context.Background(), o)
		if err != nil {
			t.Fatalf("%s: run: %v", name, err)
		}
		singleOutcome(t, rep, "refused:unversioned-file")
		st.mu.Lock()
		if st.issues["brain-k000a"].Title != "Accepted subject" {
			t.Errorf("%s: unversioned file reached the substrate", name)
		}
		st.mu.Unlock()
	}
}

func removeLinePrefix(s, prefix string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func replaceLinePrefix(s, prefix, with string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			lines[i] = with
		}
	}
	return strings.Join(lines, "\n")
}

// TestStaleSlack_SlugPersistRaceIsNotStale — the renderer persists the slug
// after writing the file, bumping the row's stamp by up to a second; that
// must not read as a newer record.
func TestStaleSlack_SlugPersistRaceIsNotStale(t *testing.T) {
	st := newTStore()
	o := baseOptions(t, st, true)
	path := renderFile(t, st, o.Root, "brain-k000a", func(got string) string {
		return strings.Replace(got, "title: \"Accepted subject\"", "title: \"Edited\"", 1)
	})
	seedManifest(t, o.Root, "brain-k000a", relEntry(t, o.Root, path))
	st.mu.Lock()
	st.issues["brain-k000a"].UpdatedAt = st.issues["brain-k000a"].UpdatedAt.Add(time.Second)
	st.mu.Unlock()
	rep, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	singleOutcome(t, rep, "applied")
}
