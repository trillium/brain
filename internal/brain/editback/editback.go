// Package editback implements the run that passes an edit to a rendered
// markdown file back into its bead, and the run that marks a bead whose
// rendered file was deleted.
//
// The captain's two verdicts (VISION.md, landed as divergence/0029) govern
// every choice here:
//
//   - "Editing a rendered file can be made to pass back into its bead, per
//     store, when that store wants it." Opt-in per store: the declaration
//     lives in ~/.config/brain/stores.yaml (set it with
//     'bd stores edit-back <name> on|off'), and a store that does not
//     declare it keeps today's one-way render behaviour — with the
//     difference reported, not silent.
//   - "Deleting a rendered file never deletes a bead: it marks the bead for
//     deletion, so an accident in a synced folder cannot destroy the
//     record." The mark is the label exfiltrator.DeletionMarkLabel; clearing
//     it is an explicit operator verb ('bd render-marks clear'), and a
//     re-render never clears it while it stands.
//
// The substrate stays the authority: the database keeps the durable record,
// writing happens only where this package applies an import, and every
// change is reported with old → new so the losing side of any disagreement
// is visible rather than silently overwritten.
package editback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/brain/exfiltrator"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// Store is the write/read surface the edit-back run needs. Production
// wires it to the decorated store (the same stack a create uses, so the
// post-write render converges the file with the row after every apply).
type Store interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error
	AddLabel(ctx context.Context, issueID, label, actor string) error
	RemoveLabel(ctx context.Context, issueID, label, actor string) error
}

// OutcomeKind names what the run did with one file (or one deletion).
const (
	OutcomeClean           = "clean"             // no change between file and row
	OutcomeApplied         = "applied"           // edits written into the bead
	OutcomeIgnored         = "edit-ignored"      // store does not accept edit-back; reported, not written
	OutcomeRefused         = "refused"           // named refusal, nothing written
	OutcomeMarked          = "marked"            // file gone, bead now carries the deletion mark
	OutcomeAlreadyMarked   = "already-marked"    // file gone, mark already present
	OutcomeMarkSkippedGone = "mark-skip-no-bead" // file gone and bead gone — nothing to mark
	OutcomeFilePresent     = "file-present"      // manifest entry whose file still exists
)

// Refusals are the named refusal codes. Each is a different way to refuse,
// spelled out so an operator can grep the code and read exactly one
// paragraph about it.
const (
	RefCannotRead         = "cannot-read"
	RefCannotParse        = "cannot-parse"
	RefNoName             = "no-name"
	RefOutsideNamespace   = "outside-namespace"
	RefUnknownBead        = "unknown-bead"
	RefSlugMismatch       = "slug-mismatch"
	RefNotManifestedPath  = "not-manifested-path"
	RefAmbiguousLabels    = "ambiguous-labels"
	RefAmbiguousBody      = "ambiguous-body"
	RefPriorityUnreadable = "priority-unreadable"
	RefEditedWhileMarked  = "edited-while-marked"
	RefStaleFile          = "stale-file"
	RefUnversionedFile    = "unversioned-file"
	RefWriteFailed        = "write-failed"
	RefLookupFailed       = "lookup-failed"
	RefManifestEscape     = "manifest-escapes-root"
)

// Change is one field the file and the row disagree on. From and To are
// always both present in the report: the losing side of every disagreement
// is visible, never silently overwritten.
type Change struct {
	Field string
	From  string
	To    string
}

// Outcome is the per-file (or per-bead) result.
type Outcome struct {
	File    string
	Bead    string
	Kind    string // an OutcomeKind, or OutcomeRefused + ":" + a refusal code
	Changes []Change
	Refusal string // the refusal sentence when Kind starts with "refused:"
}

// Report is the whole run's result.
type Report struct {
	Outcomes []Outcome
	// DeletionUnavailable carries the named refusal when the deletion-mark
	// scan itself could not run (no manifest, unreadable manifest). Edit
	// handling still ran; the run cannot vouch for deletions.
	DeletionUnavailable string
}

// Options configures one run.
type Options struct {
	// Root is the store's exfiltration root (BRAIN_KNOWLEDGE_ROOT
	// resolution — the same resolution the renderer used).
	Root string
	// Namespace is the store's own namespace (BD_NAME). Empty refuses the
	// whole run: per-store edit-back cannot be scoped without it.
	Namespace string
	// NamespaceAuthority answers whether a bead id belongs to this store's
	// namespace. A non-nil error IS the refusal (it names the prefix and
	// who owns it); nil means owned. The unified database reads
	// brain_store_prefixes; a legacy store degrades to the store's own
	// name.
	NamespaceAuthority func(id string) error
	// AcceptsEdits is the store's declaration. False keeps one-way
	// behaviour: edits are REPORTED (OutcomeIgnored with the full change
	// list) and never written.
	AcceptsEdits bool
	// Store is the substrate surface.
	Store Store
	// Actor records why the bead changed, for the event log.
	Actor string
	// QuietPresent suppresses the per-file "file present" lines for
	// manifest entries whose file still exists (the default report is
	// already long; presence is the ordinary case).
	QuietPresent bool
}

// Run performs the whole pass: apply importable edits (or report them when
// the store does not accept edits), then mark deletions.
func Run(ctx context.Context, o Options) (*Report, error) {
	if o.Store == nil {
		return nil, errors.New("edit-back: refusing: no store to read or write")
	}
	if o.Namespace == "" {
		return nil, errors.New("edit-back: refusing: no store namespace pinned (BD_NAME is unset) — per-store edit-back cannot be scoped without one; run under a store wrapper")
	}
	if o.Root == "" {
		return nil, errors.New("edit-back: refusing: no exfiltration root resolved — the run needs the root the renderer wrote to")
	}

	entries := filepath.Join(o.Root, "entries")
	if _, err := os.Stat(entries); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("edit-back: refusing: %s does not exist — there are no rendered files here (nothing was ever rendered, or the root points elsewhere); never guessing what was missed", entries)
		}
		return nil, fmt.Errorf("edit-back: reading %s: %w", entries, err)
	}

	rep := &Report{}
	o.runEdits(ctx, entries, rep)
	o.scanDeletions(ctx, rep)
	return rep, nil
}

// runEdits walks the rendered directory and handles every .md file.
func (o Options) runEdits(ctx context.Context, entries string, rep *Report) {
	mf, _, err := exfiltrator.ReadManifest(o.Root)
	if err != nil && !errors.Is(err, exfiltrator.ErrNoManifest) {
		mf = nil
	}

	var paths []string
	walkErr := filepath.WalkDir(entries, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if walkErr != nil {
		rep.Outcomes = append(rep.Outcomes, Outcome{
			File: entries, Kind: OutcomeRefused,
			Refusal: "cannot walk the rendered directory: " + walkErr.Error(),
		})
		return
	}
	sort.Strings(paths)

	for _, path := range paths {
		o.oneFile(ctx, path, mf, rep)
	}
}

// oneFile handles one rendered file end to end.
func (o Options) oneFile(ctx context.Context, path string, mf *exfiltrator.ManifestFile, rep *Report) {
	raw, err := os.ReadFile(path)
	if err != nil {
		rep.Outcomes = append(rep.Outcomes, refusal(path, "", RefCannotRead,
			fmt.Sprintf("cannot read the file: %v", err)))
		return
	}
	fm, body, err := ParseFrontmatter(raw)
	if err != nil {
		rep.Outcomes = append(rep.Outcomes, refusal(path, "", RefCannotParse, err.Error()))
		return
	}
	id, ok := FileID(fm)
	if !ok {
		rep.Outcomes = append(rep.Outcomes, refusal(path, "", RefNoName,
			"frontmatter carries no id — the file names nothing, and editing a bead no file names would be database guessing; refusing"))
		return
	}

	// Namespace first: a file sitting in this store's directory that
	// names another store's bead is a misplaced file, not an edit to
	// import.
	if o.NamespaceAuthority != nil {
		if nerr := o.NamespaceAuthority(id); nerr != nil {
			rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefOutsideNamespace, nerr.Error()))
			return
		}
	}

	row, err := o.Store.GetIssue(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefUnknownBead,
				fmt.Sprintf("names %s, which does not exist in this store — refusing the file by name; the substrate record was never at risk", id)))
			return
		}
		rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefLookupFailed, err.Error()))
		return
	}
	if row == nil {
		rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefUnknownBead,
			fmt.Sprintf("names %s, which does not exist in this store — refusing the file by name", id)))
		return
	}

	// Slug consistency: the render key is the slug, and the bead's
	// persisted slug is where the filename reference lives. A file
	// claiming a different slug is plausibly another bead's file that an
	// editor retitled into ambiguity.
	if fmSlug := strings.TrimSpace(fm.Fields["slug"]); fmSlug != "" {
		persisted := MetadataSlug(row.Metadata)
		if persisted != "" && persisted != fmSlug {
			rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefSlugMismatch,
				fmt.Sprintf("frontmatter says slug %q, but %s's persisted render-key slug is %q — this may be another bead's file; refusing rather than mapping a file to the wrong bead", fmSlug, id, persisted)))
			return
		}
	}

	// Manifest cross-check: a file at a path the manifest does not record
	// for this bead is an unmanifested copy (a move, a sync artifact)
	// whose provenance is unknown.
	if mf != nil {
		if entry, ok := mf.Files[id]; ok {
			manifested := filepath.Clean(filepath.Join(o.Root, entry.Path))
			if filepath.Clean(path) != manifested {
				rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefNotManifestedPath,
					fmt.Sprintf("the manifest renders %s at %s, but this file is at %s — refusing a copy whose provenance is unknown; restore '%s' or re-render '%s'", id, manifested, path, manifested, id)))
				return
			}
		}
	}

	// A bead marked for deletion must not be edited here: the deletion
	// intent sits on the bead, and an edit re-imported past it would
	// resurrect conflicting content in the same breath as the deletion
	// scan.
	if hasDeletionMark(row.Labels) {
		rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefEditedWhileMarked,
			fmt.Sprintf("%s is marked for deletion (its rendered file was deleted) — refusing the edit until the mark is cleared with 'bd render-marks clear %s'", id, id)))
		return
	}

	// Staleness: the file's "updated" stamp is the row's updated time at the
	// moment of the render the file was edited from. If the row has changed
	// since, the file is an edit of a record that no longer exists — a
	// stale copy that a synced folder delivered late — and importing it
	// would silently revert the newer row. The substrate wins: refuse, name
	// both stamps, and let the human re-render and redo the edit.
	if code, detail := staleCheck(id, row, fm); code != "" {
		rep.Outcomes = append(rep.Outcomes, refusal(path, id, code, detail))
		return
	}

	changes, fileDesc, failure := diff(row, fm, body)
	if failure != "" {
		rep.Outcomes = append(rep.Outcomes, refusal(path, id, failureCode(failure), failure))
		return
	}
	if len(changes) == 0 {
		rep.Outcomes = append(rep.Outcomes, Outcome{File: path, Bead: id, Kind: OutcomeClean})
		return
	}

	if !o.AcceptsEdits {
		rep.Outcomes = append(rep.Outcomes, Outcome{
			File:    path,
			Bead:    id,
			Kind:    OutcomeIgnored,
			Changes: changes,
		})
		return
	}

	if err := o.apply(ctx, row, id, fm, changes, fileDesc); err != nil {
		rep.Outcomes = append(rep.Outcomes, refusal(path, id, RefWriteFailed, err.Error()))
		return
	}
	rep.Outcomes = append(rep.Outcomes, Outcome{
		File:    path,
		Bead:    id,
		Kind:    OutcomeApplied,
		Changes: changes,
	})
}

// staleSlack is how far past the file's stamp a row may sit before the file
// counts as stale. The renderer persists a bead's derived slug AFTER it
// writes the file, and that write bumps the row's updated time, so on a
// bead's first render the file is stamped up to a second older than its own
// row. Anything inside the slack is the render racing itself, not a newer
// record. The cost: a real change within the slack of the render it is
// compared against is not detected — the documented limit of this guard.
const staleSlack = 2 * time.Second

// staleCheck compares the file's "updated" stamp with the row's. Stamps are
// RFC 3339 to the second, so both sides are truncated to the second.
func staleCheck(id string, row *types.Issue, fm *Frontmatter) (code, detail string) {
	raw, present := StringOf(fm, "updated")
	raw = strings.TrimSpace(raw)
	if !present || raw == "" {
		return RefUnversionedFile, fmt.Sprintf("frontmatter carries no updated stamp, so there is no way to tell whether this file was edited from the current %s or from an older render; refusing rather than guessing — re-render with 'bd render %s' and redo the edit", id, id)
	}
	fileUpdated, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return RefUnversionedFile, fmt.Sprintf("frontmatter updated stamp %q is not RFC 3339, so the file's version cannot be established; refusing rather than guessing — re-render with 'bd render %s' and redo the edit", raw, id)
	}
	rowUpdated := row.UpdatedAt.UTC().Truncate(time.Second)
	if rowUpdated.After(fileUpdated.UTC().Truncate(time.Second).Add(staleSlack)) {
		return RefStaleFile, fmt.Sprintf("file was rendered at updated=%s but %s has changed since (updated=%s) — importing would revert the newer record; the substrate wins. Re-render with 'bd render %s' and redo the edit on the fresh file",
			fileUpdated.UTC().Format(time.RFC3339), id, rowUpdated.Format(time.RFC3339), id)
	}
	return "", ""
}

// diff failure is one message string; its code and message share the same
// construction site. Empty string means no failure.
func failureCode(failure string) string {
	if strings.Contains(failure, "tags and labels") {
		return RefAmbiguousLabels
	}
	if strings.Contains(failure, "H1 heading") || strings.Contains(failure, "H1") {
		return RefAmbiguousBody
	}
	if strings.Contains(failure, "priority") {
		return RefPriorityUnreadable
	}
	return RefCannotParse
}

// diff compares the file against the row and answers the changes the file
// carries, plus the description text to write when a description change is
// among them.
func diff(row *types.Issue, fm *Frontmatter, body []byte) (changes []Change, fileDesc string, failure string) {
	fileTitle, fileTitlePresent := FileTitle(fm)

	// Title.
	if fileTitlePresent && fileTitle != row.Title {
		changes = append(changes, Change{Field: "title", From: row.Title, To: fileTitle})
	}

	// Status. A missing key is "no status change", not "empty status".
	if fmStatus, present := StringOf(fm, "status"); present && strings.TrimSpace(fmStatus) != "" && types.Status(fmStatus) != row.Status {
		changes = append(changes, Change{Field: "status", From: string(row.Status), To: fmStatus})
	}

	// Priority.
	if n, present, err := FilePriority(fm); err != nil {
		return nil, "", "priority is not readable as an integer 0-4: " + err.Error()
	} else if present && n != row.Priority {
		changes = append(changes, Change{Field: "priority", From: fmt.Sprintf("%d", row.Priority), To: fmt.Sprintf("%d", n)})
	}

	// Labels.
	if fileLabels, present, ambiguous := FileLabels(fm); present {
		if ambiguous {
			return nil, "", "tags and labels disagree in the frontmatter — the renderer writes both with the same value, so the file itself is ambiguous about which is the edit; make them agree or edit labels with 'bd label'"
		}
		add, del := labelDiff(row.Labels, fileLabels)
		for _, l := range del {
			changes = append(changes, Change{Field: "label", From: "present", To: "absent (" + l + ")"})
		}
		for _, l := range add {
			changes = append(changes, Change{Field: "label", From: "absent", To: "present (" + l + ")"})
		}
	}

	// Description.
	fileDesc, err := SplitBody(body, fileTitle, row.Title)
	if err != nil {
		return nil, "", err.Error()
	}
	if trimDesc(fileDesc) != trimDesc(row.Description) {
		changes = append(changes, Change{Field: "description", From: trimDesc(row.Description), To: trimDesc(fileDesc)})
	}

	return changes, fileDesc, ""
}

// apply writes the changes into the substrate: one scalar-map update
// (title, status, priority, description together — one database
// transaction), then the label adds and removals. The post-write render
// happens through the store's decorator (the same stack every create and
// mutation uses), so the file converges with the row at the end.
func (o Options) apply(ctx context.Context, row *types.Issue, id string, fm *Frontmatter, changes []Change, fileDesc string) error {
	updates := map[string]interface{}{}
	for _, c := range changes {
		switch c.Field {
		case "title":
			v, present := FileTitle(fm)
			if present {
				updates["title"] = v
			}
		case "status":
			if v, present := StringOf(fm, "status"); present {
				updates["status"] = v
			}
		case "priority":
			if n, present, err := FilePriority(fm); err == nil && present {
				updates["priority"] = n
			}
		case "description":
			updates["description"] = fileDesc
		}
	}
	if len(updates) > 0 {
		if err := o.Store.UpdateIssue(ctx, id, updates, o.Actor); err != nil {
			return fmt.Errorf("the substrate refused the write: %v", err)
		}
	}

	if fileLabels, present, ambiguous := FileLabels(fm); present && !ambiguous {
		add, del := labelDiff(row.Labels, fileLabels)
		for _, l := range del {
			if err := o.Store.RemoveLabel(ctx, id, l, o.Actor); err != nil {
				return fmt.Errorf("the substrate refused the label removal: %v", err)
			}
		}
		for _, l := range add {
			if err := o.Store.AddLabel(ctx, id, l, o.Actor); err != nil {
				return fmt.Errorf("the substrate refused the label addition: %v", err)
			}
		}
	}
	return nil
}

// ── Deletion marks ────────────────────────────────────────────────

// scanDeletions consults the render manifest and marks beads whose
// rendered file is gone. It never deletes a bead and never resurrects a
// file — the mark blocks renders until it is cleared explicitly ('bd
// render-marks clear').
func (o Options) scanDeletions(ctx context.Context, rep *Report) {
	mf, manifestPath, err := exfiltrator.ReadManifest(o.Root)
	if err != nil {
		if errors.Is(err, exfiltrator.ErrNoManifest) {
			rep.DeletionUnavailable = "deletions NOT scanned: no render manifest at " + manifestPath +
				" (the manifest is written by the render path; until the next render records one, a deletion here is indistinguishable from a bead that was never rendered) — refusing to guess what was deleted"
			return
		}
		rep.DeletionUnavailable = "deletions NOT scanned: " + err.Error()
		return
	}

	ids := exfiltrator.SortedManifestIDs(mf)
	for _, id := range ids {
		entry := mf.Files[id]
		abs := filepath.Clean(filepath.Join(o.Root, entry.Path))
		if !strings.HasPrefix(abs, filepath.Clean(o.Root)+string(os.PathSeparator)) {
			rep.Outcomes = append(rep.Outcomes, refusal(abs, id, RefManifestEscape,
				"manifest entry escapes the exfil root: refusing to touch "+abs))
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			if !o.QuietPresent {
				rep.Outcomes = append(rep.Outcomes, Outcome{File: abs, Bead: id, Kind: OutcomeFilePresent})
			}
			continue
		}

		row, err := o.Store.GetIssue(ctx, id)
		if err != nil {
			rep.Outcomes = append(rep.Outcomes, refusal(abs, id, RefLookupFailed, err.Error()))
			continue
		}
		if row == nil {
			rep.Outcomes = append(rep.Outcomes, Outcome{
				File: abs, Bead: id, Kind: OutcomeMarkSkippedGone,
				Refusal: "there is no bead to mark — the bead is already gone from the substrate, so the file's absence is not a data risk",
			})
			continue
		}
		if hasDeletionMark(row.Labels) {
			rep.Outcomes = append(rep.Outcomes, Outcome{File: abs, Bead: id, Kind: OutcomeAlreadyMarked})
			continue
		}
		if err := o.Store.AddLabel(ctx, id, exfiltrator.DeletionMarkLabel, o.Actor); err != nil {
			rep.Outcomes = append(rep.Outcomes, refusal(abs, id, RefWriteFailed, err.Error()))
			continue
		}
		rep.Outcomes = append(rep.Outcomes, Outcome{File: abs, Bead: id, Kind: OutcomeMarked})
	}
}

// ── Helpers ───────────────────────────────────────────────────────

// MetadataSlug reads a bead's persisted render-key slug out of raw
// metadata. The exfiltrator owns the key name.
func MetadataSlug(raw json.RawMessage) string {
	return exfiltrator.MetadataSlug(raw)
}

// labelDiff answers (add, remove) between a bead's current labels and the
// set the file wants.
func labelDiff(current, wanted []string) (add, remove []string) {
	currentSet := map[string]bool{}
	for _, l := range current {
		currentSet[l] = true
	}
	wantedSet := map[string]bool{}
	for _, l := range wanted {
		wantedSet[l] = true
	}
	for _, l := range wanted {
		if !currentSet[l] {
			add = append(add, l)
		}
	}
	for _, l := range current {
		if !wantedSet[l] {
			remove = append(remove, l)
		}
	}
	sort.Strings(remove)
	sort.Strings(add)
	return add, remove
}

// hasDeletionMark mirrors the exfiltrator's own check (the label that
// blocks render).
func hasDeletionMark(labels []string) bool {
	return hasString(labels, exfiltrator.DeletionMarkLabel)
}

// trimDesc normalizes a description for comparison: leading/trailing
// whitespace and trailing newlines are not content (the renderer
// normalizes them away).
func trimDesc(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "\n")
}

// refusal builds an Outcome with the refusal code as the kind and the
// detail stating what was refused and why.
func refusal(path, bead, code, detail string) Outcome {
	return Outcome{
		File:    path,
		Bead:    bead,
		Kind:    OutcomeRefused + ":" + code,
		Refusal: detail,
	}
}
