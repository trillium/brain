package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// maxTitleLen mirrors the title bound enforced by internal/types validation.
// Compose checks it up front so an over-long first line is reported in the
// editor loop instead of destroying the text on the way to the store.
const maxTitleLen = 500

// composeScissors separates the content the user is writing from the
// instructions below it. Beads bodies are markdown, so '#' cannot mean
// "comment" the way it does in a git commit message — a scissors cut keeps
// markdown headings intact.
const composeScissors = "# ------------------------ >8 ------------------------"

const composeHelpBody = `# Everything below this line is ignored.
#
# First line above   = the title (max 500 characters).
# After a blank line = the body (markdown; '#' headings are kept).
#
# Save with an empty title to abort — no bead is created.`

// composeHelp is the full scissors block as it appears in a fresh buffer.
const composeHelp = composeScissors + "\n" + composeHelpBody

// stdinIsTerminal reports whether compose can hand the terminal to an editor.
// It is a var so tests can drive the interactive path without a real tty.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// composeDraftPath holds the editor buffer of an in-flight compose. It stays on
// disk until the bead is actually created (discardComposeDraft), so a create
// that fails downstream still leaves the user's text somewhere.
var composeDraftPath string

// discardComposeDraft removes the compose buffer once the bead exists.
func discardComposeDraft() {
	if composeDraftPath == "" {
		return
	}
	_ = os.Remove(composeDraftPath)
	composeDraftPath = ""
}

// reportComposeDraft points at the surviving buffer when create did not finish.
func reportComposeDraft() {
	if composeDraftPath == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "Your draft is preserved in: %s\n", composeDraftPath)
}

// resolveEditorCommand returns the editor to shell out to, preferring $EDITOR,
// then $VISUAL, then the first common editor on PATH.
func resolveEditorCommand() (string, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		for _, defaultEditor := range []string{"vim", "vi", "nano", "emacs"} {
			if _, err := exec.LookPath(defaultEditor); err == nil {
				editor = defaultEditor
				break
			}
		}
	}
	if editor == "" {
		return "", fmt.Errorf("no editor found. Set $EDITOR or $VISUAL environment variable")
	}
	return editor, nil
}

// runEditorOnFile opens path in the resolved editor, wired to the real terminal.
func runEditorOnFile(editor, path string) error {
	editorParts := strings.Fields(editor)
	editorArgs := append(editorParts[1:], path)
	editorCmd := exec.Command(editorParts[0], editorArgs...) //nolint:gosec // G204: editor from trusted $EDITOR/$VISUAL env or known defaults
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	return editorCmd.Run()
}

// composeBuffer renders the editor buffer: current title, blank line, current
// body, then the scissors block. Any warning goes *below* the scissors so a
// re-prompt can never bleed into the text the user is writing.
func composeBuffer(title, body, warning string) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n\n")
	if body != "" {
		b.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(composeScissors)
	b.WriteString("\n")
	if warning != "" {
		for _, line := range strings.Split(warning, "\n") {
			b.WriteString("# " + line + "\n")
		}
		b.WriteString("#\n")
	}
	b.WriteString(composeHelpBody)
	b.WriteString("\n")
	return b.String()
}

// parseComposeBuffer splits an edited buffer into title and body. Content at or
// below the scissors line is dropped; the first non-blank line is the title and
// everything after it is the body.
func parseComposeBuffer(content string) (title, body string) {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), composeScissors) {
			break
		}
		kept = append(kept, line)
	}

	idx := 0
	for idx < len(kept) && strings.TrimSpace(kept[idx]) == "" {
		idx++
	}
	if idx == len(kept) {
		return "", ""
	}
	title = strings.TrimSpace(kept[idx])
	body = strings.TrimSpace(strings.Join(kept[idx+1:], "\n"))
	return title, body
}

// composeInEditor loops the editor until the buffer yields a valid title (or the
// user aborts by clearing it). The temp file is left on disk whenever the caller
// might still lose text, so a rejected create never costs the user their words.
func composeInEditor(title, body string) (string, string, error) {
	editor, err := resolveEditorCommand()
	if err != nil {
		return "", "", HandleErrorRespectJSON("%v", err)
	}

	tmpFile, err := os.CreateTemp("", "bd-create-*.md")
	if err != nil {
		return "", "", HandleErrorRespectJSON("creating temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	warning := ""
	for {
		if err := os.WriteFile(tmpPath, []byte(composeBuffer(title, body, warning)), 0o600); err != nil {
			return "", "", HandleErrorRespectJSON("writing temp file: %v", err)
		}
		if err := runEditorOnFile(editor, tmpPath); err != nil {
			return "", "", HandleErrorRespectJSON("running editor: %v", err)
		}

		// #nosec G304 -- tmpPath was created by this function
		edited, err := os.ReadFile(tmpPath)
		if err != nil {
			return "", "", HandleErrorRespectJSON("reading edited file: %v", err)
		}

		title, body = parseComposeBuffer(string(edited))
		switch {
		case title == "":
			_ = os.Remove(tmpPath)
			return "", "", HandleErrorRespectJSON("aborting create: empty title")
		case len(title) > maxTitleLen:
			warning = fmt.Sprintf("Title was %d characters (max %d). Shorten the first line —\nthe rest belongs in the body below it. Your text is unchanged.", len(title), maxTitleLen)
			continue
		}

		// The caller can still fail (validation, store errors); keep the buffer
		// on disk until create succeeds so the text survives either way.
		composeDraftPath = tmpPath
		return title, body, nil
	}
}

// titleLengthError is the actionable form of the raw validation failure, which
// otherwise rejects a long paste without telling the user where to put it.
func titleLengthError(title string) error {
	if len(title) <= maxTitleLen {
		return nil
	}
	return HandleError("title is %d characters (max %d)\n"+
		"  Compose title + body in your editor:  bd create --edit\n"+
		"  Or keep the long text as the body:    bd create \"<short title>\" --description \"<long text>\"",
		len(title), maxTitleLen)
}

// maybeComposeCreate decides whether `bd create` should open an editor, and if
// so writes the composed title/body back onto the command's flags. It returns
// the args the rest of create should use (empty once the editor supplied the
// title, so the positional/flag conflict check does not fire).
func maybeComposeCreate(cmd *cobra.Command, args []string) ([]string, error) {
	editFlag, _ := cmd.Flags().GetBool("edit")

	if file, _ := cmd.Flags().GetString("file"); file != "" {
		if editFlag {
			return args, HandleError("cannot specify both --edit and --file")
		}
		return args, nil
	}
	if graph, _ := cmd.Flags().GetString("graph"); graph != "" {
		if editFlag {
			return args, HandleError("cannot specify both --edit and --graph")
		}
		return args, nil
	}

	hasTitle := len(args) > 0 || cmd.Flags().Changed("title")
	if !editFlag {
		// Bare `bd create` at a terminal composes instead of erroring out; every
		// non-interactive caller keeps the existing "title required" behavior.
		if hasTitle || !stdinIsTerminal() {
			return args, nil
		}
		if silent, _ := cmd.Flags().GetBool("silent"); silent {
			return args, nil
		}
	}

	if !stdinIsTerminal() {
		return args, HandleError("--edit needs an interactive terminal\n" +
			"  Non-interactive callers: bd create \"<title>\" --description \"<body>\"\n" +
			"  or pipe the body:        bd create \"<title>\" --stdin")
	}
	if stdinFlag, _ := cmd.Flags().GetBool("stdin"); stdinFlag {
		return args, HandleError("cannot specify both --edit and --stdin")
	}
	if cmd.Flags().Changed("body-file") || cmd.Flags().Changed("description-file") {
		return args, HandleError("cannot specify both --edit and --body-file")
	}

	title := ""
	if len(args) > 0 {
		title = args[0]
	} else {
		title, _ = cmd.Flags().GetString("title")
	}
	body, _, err := getDescriptionFlag(cmd)
	if err != nil {
		return args, err
	}

	title, body, err = composeInEditor(title, body)
	if err != nil {
		return args, err
	}

	if err := cmd.Flags().Set("title", title); err != nil {
		return args, HandleError("setting composed title: %v", err)
	}
	if err := cmd.Flags().Set("description", body); err != nil {
		return args, HandleError("setting composed body: %v", err)
	}
	return nil, nil
}
