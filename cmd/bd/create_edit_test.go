package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newComposeTestCmd builds a command carrying the flags maybeComposeCreate and
// getDescriptionFlag read, so tests exercise the real flag plumbing.
func newComposeTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().Bool("edit", false, "")
	cmd.Flags().String("file", "", "")
	cmd.Flags().String("graph", "", "")
	cmd.Flags().String("title", "", "")
	cmd.Flags().Bool("silent", false, "")
	cmd.Flags().Bool("stdin", false, "")
	cmd.Flags().String("body-file", "", "")
	cmd.Flags().String("description-file", "", "")
	cmd.Flags().StringP("description", "d", "", "")
	cmd.Flags().String("body", "", "")
	cmd.Flags().String("message", "", "")
	return cmd
}

// fakeEditor writes a script that replaces the buffer with the given contents on
// each successive invocation, and points $EDITOR at it.
func fakeEditor(t *testing.T, buffers ...string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake editor script requires a POSIX shell")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	for i, buf := range buffers {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("buf%d", i)), []byte(buf), 0o600); err != nil {
			t.Fatalf("writing buffer %d: %v", i, err)
		}
	}
	script := filepath.Join(dir, "editor.sh")
	body := fmt.Sprintf(`#!/bin/sh
n=0
[ -f %[1]q ] && n=$(cat %[1]q)
next=$((n + 1))
printf '%%s' "$next" > %[1]q
cp %[2]q/buf"$n" "$1"
`, counter, dir)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("writing editor script: %v", err)
	}
	t.Setenv("EDITOR", script)
	t.Setenv("VISUAL", "")
}

func withTerminal(t *testing.T, isTTY bool) {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func() bool { return isTTY }
	t.Cleanup(func() { stdinIsTerminal = prev })
}

func TestParseComposeBuffer(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantTitle string
		wantBody  string
	}{
		{
			name:      "title and body",
			content:   "Fix the widget\n\nThe widget breaks on Tuesdays.\n" + composeHelp,
			wantTitle: "Fix the widget",
			wantBody:  "The widget breaks on Tuesdays.",
		},
		{
			name:      "title only",
			content:   "Fix the widget\n\n" + composeHelp,
			wantTitle: "Fix the widget",
			wantBody:  "",
		},
		{
			name:      "markdown headings survive the cut",
			content:   "Design notes\n\n# Overview\n\nText\n\n## Detail\n" + composeHelp,
			wantTitle: "Design notes",
			wantBody:  "# Overview\n\nText\n\n## Detail",
		},
		{
			name:      "leading blank lines skipped",
			content:   "\n\n  Padded title  \n\nBody\n" + composeHelp,
			wantTitle: "Padded title",
			wantBody:  "Body",
		},
		{
			name:      "empty buffer aborts",
			content:   "\n\n" + composeHelp,
			wantTitle: "",
			wantBody:  "",
		},
		{
			name:      "content below scissors is dropped",
			content:   "Title\n\nKept\n" + composeHelp + "\nDiscarded prose\n",
			wantTitle: "Title",
			wantBody:  "Kept",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, body := parseComposeBuffer(tt.content)
			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestComposeBufferRoundTrips(t *testing.T) {
	buf := composeBuffer("A title", "Body line one\n\n# Heading", "")
	title, body := parseComposeBuffer(buf)
	if title != "A title" {
		t.Errorf("title = %q, want %q", title, "A title")
	}
	if body != "Body line one\n\n# Heading" {
		t.Errorf("body = %q", body)
	}
}

func TestComposeBufferWarningIsCommented(t *testing.T) {
	buf := composeBuffer("T", "B", "Title was 900 characters (max 500).")
	if !strings.Contains(buf, "# Title was 900 characters (max 500).") {
		t.Errorf("warning not rendered as a comment:\n%s", buf)
	}
	title, body := parseComposeBuffer(buf)
	if title != "T" || body != "B" {
		t.Errorf("warning leaked into content: title=%q body=%q", title, body)
	}
}

// HandleError writes the message to stderr and returns only an exit code, so
// assertions on user-facing wording read the stream (captureStderr lives in
// test_helpers_pure_test.go).
func TestTitleLengthError(t *testing.T) {
	if err := titleLengthError(strings.Repeat("a", maxTitleLen)); err != nil {
		t.Errorf("title at the limit rejected: %v", err)
	}

	var err error
	out := captureStderr(t, func() {
		err = titleLengthError(strings.Repeat("a", maxTitleLen+1))
	})
	if err == nil {
		t.Fatal("over-long title accepted")
	}
	if !strings.Contains(out, "bd create --edit") {
		t.Errorf("error does not point at the editor flow:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d characters", maxTitleLen+1)) {
		t.Errorf("error does not report the actual length:\n%s", out)
	}
}

func TestMaybeComposeCreateSkipsNonInteractive(t *testing.T) {
	withTerminal(t, false)
	cmd := newComposeTestCmd()

	args, err := maybeComposeCreate(cmd, nil)
	if err != nil {
		t.Fatalf("bare create off a tty should defer to the normal path: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want empty", args)
	}
	if cmd.Flags().Changed("title") {
		t.Error("title flag should not have been set")
	}
}

func TestMaybeComposeCreateRejectsEditWithoutTerminal(t *testing.T) {
	withTerminal(t, false)
	cmd := newComposeTestCmd()
	if err := cmd.Flags().Set("edit", "true"); err != nil {
		t.Fatal(err)
	}

	if _, err := maybeComposeCreate(cmd, nil); err == nil {
		t.Fatal("--edit off a tty should fail rather than hang")
	}
}

func TestMaybeComposeCreateLeavesExplicitTitleAlone(t *testing.T) {
	withTerminal(t, true)
	cmd := newComposeTestCmd()

	args, err := maybeComposeCreate(cmd, []string{"Just a title"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(args) != 1 || args[0] != "Just a title" {
		t.Errorf("args = %v, want [Just a title]", args)
	}
	if cmd.Flags().Changed("title") {
		t.Error("title flag should be untouched without --edit")
	}
}

func TestMaybeComposeCreateEditorFlow(t *testing.T) {
	withTerminal(t, true)
	fakeEditor(t, "Composed title\n\nComposed body\n\n# Section\n"+composeHelp)
	t.Cleanup(discardComposeDraft)

	cmd := newComposeTestCmd()
	if err := cmd.Flags().Set("edit", "true"); err != nil {
		t.Fatal(err)
	}

	args, err := maybeComposeCreate(cmd, []string{"Seed title"})
	if err != nil {
		t.Fatalf("compose failed: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want empty so the positional/flag check does not fire", args)
	}
	if got, _ := cmd.Flags().GetString("title"); got != "Composed title" {
		t.Errorf("title = %q, want %q", got, "Composed title")
	}
	desc, _, err := getDescriptionFlag(cmd)
	if err != nil {
		t.Fatalf("reading description: %v", err)
	}
	if desc != "Composed body\n\n# Section" {
		t.Errorf("description = %q", desc)
	}
}

func TestMaybeComposeCreateSeedsEditorFromFlags(t *testing.T) {
	withTerminal(t, true)
	// The editor echoes nothing back of its own; it copies buffer 0 verbatim,
	// so assert on what compose wrote by reading the draft the editor received.
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	script := filepath.Join(dir, "editor.sh")
	body := fmt.Sprintf("#!/bin/sh\ncp \"$1\" %q\n", seen)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("EDITOR", script)
	t.Setenv("VISUAL", "")
	t.Cleanup(discardComposeDraft)

	cmd := newComposeTestCmd()
	if err := cmd.Flags().Set("edit", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("description", "Existing body"); err != nil {
		t.Fatal(err)
	}

	if _, err := maybeComposeCreate(cmd, []string{"Existing title"}); err != nil {
		t.Fatalf("compose failed: %v", err)
	}

	raw, err := os.ReadFile(seen) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatalf("editor never saw a buffer: %v", err)
	}
	title, bodyText := parseComposeBuffer(string(raw))
	if title != "Existing title" || bodyText != "Existing body" {
		t.Errorf("editor buffer was not prefilled: title=%q body=%q", title, bodyText)
	}
}

func TestComposeInEditorRepromptsOnLongTitle(t *testing.T) {
	withTerminal(t, true)
	long := strings.Repeat("x", maxTitleLen+1)
	fakeEditor(t,
		long+"\n\nBody\n"+composeHelp,
		"Short title\n\nBody\n"+composeHelp,
	)
	t.Cleanup(discardComposeDraft)

	title, body, err := composeInEditor("", "")
	if err != nil {
		t.Fatalf("compose failed: %v", err)
	}
	if title != "Short title" {
		t.Errorf("title = %q, want %q", title, "Short title")
	}
	if body != "Body" {
		t.Errorf("body = %q, want %q", body, "Body")
	}
	if composeDraftPath == "" {
		t.Error("draft path should survive until the bead is created")
	}
}

func TestComposeInEditorAbortsOnEmptyTitle(t *testing.T) {
	withTerminal(t, true)
	fakeEditor(t, "\n\n"+composeHelp)

	if _, _, err := composeInEditor("", ""); err == nil {
		t.Fatal("empty title should abort the create")
	}
	if composeDraftPath != "" {
		t.Error("aborted compose should not leave a draft registered")
	}
}

func TestDiscardComposeDraftRemovesFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "draft-*")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	composeDraftPath = path

	discardComposeDraft()

	if composeDraftPath != "" {
		t.Error("draft path not cleared")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("draft file still present: %v", err)
	}
}

func TestAppendToField(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		addition string
		want     string
	}{
		{"empty field", "", "new text", "new text"},
		{"existing field", "old text", "new text", "old text\n\nnew text"},
		{"trailing newlines collapsed", "old text\n\n\n", "new text", "old text\n\nnew text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendToField(tt.current, tt.addition); got != tt.want {
				t.Errorf("appendToField(%q, %q) = %q, want %q", tt.current, tt.addition, got, tt.want)
			}
		})
	}
}
