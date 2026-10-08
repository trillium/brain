// render_import.go — `bd render-import`.
//
// The opt-in per-store pass that lets an edit made to a rendered markdown
// file pass back into its bead, and a deletion of a rendered file mark its
// bead instead of destroying it. Two verdicts from the captain's approved
// vision (VISION.md, divergence/0027):
//
//   - "Editing a rendered file can be made to pass back into its bead, per
//     store, when that store wants it." The declaration lives in
//     ~/.config/brain/stores.yaml per store ('bd stores edit-back <name>
//     on|off'); a store that does not declare it keeps one-way rendering,
//     and this verb running there REPORTS the file-vs-row differences and
//     writes nothing.
//   - "Deleting a rendered file never deletes a bead: it marks the bead for
//     deletion, so an accident in a synced folder cannot destroy the
//     record." The mark is the label exfiltrator.DeletionMarkLabel
//     ("marked-for-deletion"); 'bd render-marks clear' is the only way it
//     leaves the bead, and renders of a marked bead are skipped until it
//     does — a re-render never silently clears it, and an unmarked render
//     never resurrects past it.
//
// The substrate stays the authority. The render-import run is the one place
// the field is allowed to win, and it wins only over the fields the import
// defines — title, status, priority, labels, description — with every change
// reported as old → new so the losing side of any disagreement is visible
// rather than silently overwritten. Everything else on the bead (id, kind,
// slug, created/updated, metadata) is the substrate's alone; the file's
// disagreement there is never a change.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/beads/internal/brain/editback"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// brainEditBackActor records why a bead changed during a render-import run,
// so the event log attributes these writes to the import rather than to a
// human edit command.
const brainEditBackActor = "brain-editback"

var renderImportCmd = &cobra.Command{
	Use:     "render-import",
	GroupID: "issues",
	Short:   "Import edits from the store's rendered markdown into their beads (opt-in per store), and mark beads whose rendered file was deleted",
	Long: `Import edits made to this store's rendered markdown files back into the
beads they name, and mark beads whose rendered file has been deleted.

The pass is opt-in per store and refuses to guess:

  - A store accepts edit-back only when its registry entry declares it:
      bd stores edit-back <name> on
    Where the declaration lives: ~/.config/brain/stores.yaml, per store.
    'bd stores list' shows which stores accept edits (one-way is the
    default). A store that does not accept edit-back keeps today's one-way
    rendering: this run reports the file-vs-row differences and writes
    nothing to the substrate.
  - WHAT AN EDIT MEANS: the fields the import defines are title, status,
    priority, labels (the frontmatter's labels/tags), and description (the
    body after the "# {title}" H1). Everything else on the bead — id, kind,
    render-key slug, created/updated timestamps, metadata — stays
    substrate-owned; the file's disagreement there is never a change. The
    substrate stays the authority: the database keeps the durable record,
    and the render-import run is the one place the field is allowed to win,
    with every change printed as old → new.
  - DELETION MARKS, NEVER DELETES: a rendered file that has disappeared
    (recorded by the render manifest, entries/.render-manifest.json) marks
    its bead with the "marked-for-deletion" label. The bead is never
    removed and its label is visible on every read. A bead wearing the
    mark is skipped by every render — a re-render never silently clears
    the mark, and an unmarked render never resurrects the deleted file —
    so an accident in the synced folder cannot destroy the record, and
    nothing resurrects it without a human. The mark is cleared only by
      bd render-marks clear <id>
    after which the normal render path re-creates the file.

Refusals are loud and named — each is a different refusal with its own
code, spelled out by the run: a file that cannot be read; frontmatter
that names no bead (no id); a file naming a bead whose id prefix belongs
to another store's namespace; a file naming a bead that does not exist in
this store (refused by name); a file whose slug disagrees with the bead it
names; a file at a path the render manifest never recorded for that bead;
tags and labels disagreeing; a body whose H1 disagrees with both the
file's and the bead's title; a priority that is not an integer 0-4; an
edit to a bead that is already marked for deletion.

Behavioral contract (see divergence/0027): each refused file names its
reason; each applied edit names old → new; each mark names the missing
file. Exit 0 only when nothing was refused.

Examples:
  bd render-import
  bd render-import --json`,
	Args: cobra.NoArgs,
	Run:  runRenderImport,
}

func init() {
	rootCmd.AddCommand(renderImportCmd)
}

type renderImportJSON struct {
	Store               string                    `json:"store"`
	Root                string                    `json:"root"`
	AcceptsEdits        bool                      `json:"accepts_edits"`
	Applied             int                       `json:"applied"`
	Ignored             int                       `json:"edit_ignored"`
	Refused             int                       `json:"refused"`
	Marked              int                       `json:"marked"`
	AlreadyMarked       int                       `json:"already_marked"`
	Clean               int                       `json:"clean"`
	DeletionUnavailable string                    `json:"deletion_unavailable,omitempty"`
	Outcomes            []renderImportOutcomeJSON `json:"outcomes"`
}

type renderImportOutcomeJSON struct {
	File    string            `json:"path"`
	Bead    string            `json:"bead,omitempty"`
	Kind    string            `json:"outcome"`
	Refusal string            `json:"refusal,omitempty"`
	Changes []editback.Change `json:"changes,omitempty"`
}

func runRenderImport(cmd *cobra.Command, args []string) {
	ctx := rootCtx

	ns := strings.TrimSpace(os.Getenv("BD_NAME"))
	if ns == "" {
		// Not a silent skip: the per-store declaration cannot be read
		// and the namespace check cannot bound the run, so the refusal
		// is the only loud outcome.
		FatalErrorRespectJSON("render-import is per-store and refusing: BD_NAME is unset, so no store namespace is pinned; run under a store wrapper")
	}

	root := brainKnowledgeRoot()
	if root == "" {
		FatalErrorRespectJSON("render-import refusing: no exfiltration root resolved (BRAIN_KNOWLEDGE_ROOT, dirname($BEADS_DIR), ~/data/brain in order)")
	}

	// The declaration lives in the federation registry, keyed by the
	// store's name. A store absent from the registry has declared
	// nothing, so it stays one-way — reported, not silent — with a note
	// so the operator knows why.
	acceptsEdits, acceptsNote := registryAcceptsEditBack(ns)

	st := store
	if st == nil {
		FatalErrorRespectJSON("no active store")
	}

	authority := namespaceAuthority(ctx, st, ns)

	opts := editback.Options{
		Root:               root,
		Namespace:          ns,
		NamespaceAuthority: authority,
		AcceptsEdits:       acceptsEdits,
		Store:              st,
		Actor:              brainEditBackActor,
		QuietPresent:       true,
	}

	report, err := editback.Run(ctx, opts)
	if err != nil {
		FatalErrorRespectJSON("%v", err)
	}

	// Sort present-file chatter out of the printed outcomes; the report
	// already collected everything.
	var printed []editback.Outcome
	for _, oc := range report.Outcomes {
		if oc.Kind == editback.OutcomeFilePresent {
			continue
		}
		printed = append(printed, oc)
	}

	counts := summarizeReport(printed)

	if jsonOutput {
		emitRenderImportJSON(ns, root, acceptsEdits, report, printed, counts)
	} else {
		emitRenderImportText(ns, root, acceptsEdits, acceptsNote, report, printed, counts)
	}

	if counts.refused > 0 {
		os.Exit(1)
	}
}

// summary counts is the shared tally behind both flushes.
type importCounts struct {
	applied, ignored, refused, marked, alreadyMarked, clean int
}

func summarizeReport(outcomes []editback.Outcome) importCounts {
	var c importCounts
	for _, oc := range outcomes {
		switch {
		case strings.HasPrefix(oc.Kind, editback.OutcomeRefused):
			c.refused++
		case oc.Kind == editback.OutcomeApplied:
			c.applied++
		case oc.Kind == editback.OutcomeIgnored:
			c.ignored++
		case oc.Kind == editback.OutcomeMarked:
			c.marked++
		case oc.Kind == editback.OutcomeAlreadyMarked:
			c.alreadyMarked++
		case oc.Kind == editback.OutcomeClean:
			c.clean++
		}
	}
	return c
}

// registryAcceptsEditBack reads the store's declaration. Record the two
// answers separately so the text mode can say WHY a store stays one-way.
func registryAcceptsEditBack(name string) (accepts bool, note string) {
	stores, err := loadStoresRegistry()
	if err != nil {
		return false, "the store registry could not be read (" + err.Error() + "), treating this store as one-way"
	}
	entry, ok := stores[name]
	if !ok {
		return false, "this store has no registry entry in ~/.config/brain/stores.yaml, so edit-back was never declared; the store stays one-way (declare it with 'bd stores edit-back " + name + " on')"
	}
	if !entry.EditBack {
		return false, "this store's registry entry does not declare edit-back, so edits are reported here and written nowhere (declare it with 'bd stores edit-back " + name + " on')"
	}
	return true, ""
}

// namespaceAuthority wires the run's namespace check to the unified
// database's brain_store_prefixes record — the same ownership answer every
// read and create on this database relies on. A store opened without a raw
// database handle (the embedded engine) cannot read that record, so the
// check falls back to the namespace's own prefix rule: an id is this
// store's only when its prefix is the store's name. That is stricter than
// the record (a runtime-claimed extra prefix is refused here), which is the
// safe direction — the file is refused by name, never guessed into a bead.
func namespaceAuthority(ctx context.Context, st storage.DoltStorage, ns string) func(id string) error {
	if inner, ok := storage.UnwrapStore(st).(storage.RawDBAccessor); ok {
		if db := inner.DB(); db != nil {
			return func(id string) error {
				return issueops.ValidateNamespaceOwnership(ctx, db, ns, id)
			}
		}
	}
	return func(id string) error {
		prefix := id
		if i := strings.IndexByte(id, '-'); i > 0 {
			prefix = id[:i]
		}
		if prefix == ns {
			return nil
		}
		return fmt.Errorf("id %q carries prefix %q, which is not store %q's namespace (the brain_store_prefixes record is unreadable on this engine, so only the store's own prefix is accepted) — the file belongs to another store", id, prefix, ns)
	}
}

func changeLine(c editback.Change) string {
	return fmt.Sprintf("%s: %s → %s", c.Field, displayPointer(c.From), c.To)
}

func displayPointer(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

func emitRenderImportText(ns, root string, acceptsEdits bool, acceptsNote string, report *editback.Report, outcomes []editback.Outcome, counts importCounts) {
	for _, oc := range outcomes {
		switch {
		case strings.HasPrefix(oc.Kind, editback.OutcomeRefused):
			fmt.Printf("refused  %s  [%s] %s: %s\n", oc.File, oc.Kind[len(editback.OutcomeRefused)+1:], oc.Bead, oc.Refusal)
		case oc.Kind == editback.OutcomeApplied:
			fmt.Printf("applied  %s  %s: %s\n", oc.File, oc.Bead, strings.Join(changeLines(oc.Changes), "; "))
		case oc.Kind == editback.OutcomeIgnored:
			fmt.Printf("ignored  %s  %s: %s\n", oc.File, oc.Bead, strings.Join(changeLines(oc.Changes), "; "))
		case oc.Kind == editback.OutcomeMarked:
			fmt.Printf("marked   %s  %s: rendered file missing — bead now carries the \"marked-for-deletion\" label; clear with 'bd render-marks clear %s'\n", oc.File, oc.Bead, oc.Bead)
		case oc.Kind == editback.OutcomeAlreadyMarked:
			fmt.Printf("still    %s  %s: already carries the deletion mark (file still missing)\n", oc.File, oc.Bead)
		case oc.Kind == editback.OutcomeMarkSkippedGone:
			fmt.Printf("skip     %s  %s: %s\n", oc.File, oc.Bead, oc.Refusal)
		case oc.Kind == editback.OutcomeClean:
			fmt.Printf("clean    %s  %s: file and bead agree\n", oc.File, oc.Bead)
		}
	}

	fmt.Fprintln(os.Stderr)
	if report.DeletionUnavailable != "" {
		fmt.Fprintln(os.Stderr, "render-import: "+report.DeletionUnavailable)
	}
	mode := "one-way"
	if acceptsEdits {
		mode = "edit-back"
	}
	if acceptsNote != "" && !acceptsEdits {
		fmt.Fprintln(os.Stderr, "render-import: "+acceptsNote)
	}
	fmt.Fprintf(os.Stderr, "render-import: store %s (%s) at %s: %d applied, %d edit-ignored (reported, not written), %d refused, %d marked, %d already-marked, %d clean\n",
		ns, mode, root, counts.applied, counts.ignored, counts.refused, counts.marked, counts.alreadyMarked, counts.clean)
}

func changeLines(changes []editback.Change) []string {
	lines := make([]string, 0, len(changes))
	for _, c := range changes {
		lines = append(lines, changeLine(c))
	}
	return lines
}

func emitRenderImportJSON(ns, root string, acceptsEdits bool, report *editback.Report, outcomes []editback.Outcome, counts importCounts) {
	rows := make([]renderImportOutcomeJSON, 0, len(outcomes))
	for _, oc := range outcomes {
		rows = append(rows, renderImportOutcomeJSON{
			File:    oc.File,
			Bead:    oc.Bead,
			Kind:    oc.Kind,
			Refusal: oc.Refusal,
			Changes: oc.Changes,
		})
	}
	payload := renderImportJSON{
		Store:               ns,
		Root:                root,
		AcceptsEdits:        acceptsEdits,
		Applied:             counts.applied,
		Ignored:             counts.ignored,
		Refused:             counts.refused,
		Marked:              counts.marked,
		AlreadyMarked:       counts.alreadyMarked,
		Clean:               counts.clean,
		DeletionUnavailable: report.DeletionUnavailable,
		Outcomes:            rows,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}
