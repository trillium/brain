package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	isarenderverb "github.com/steveyegge/beads/internal/brain/verb/isarender"
	"github.com/spf13/cobra"
)

// Flag values for `bd isa-render-all`. The single-id `bd isa-render` verb
// takes no flags — path is derived from slug + exfil root.
var (
	isaRenderAllSince string
)

var isaRenderCmd = &cobra.Command{
	Use:     "isa-render <id>",
	GroupID: "issues",
	Short:   "Render an ISA from the substrate to canonical IsaFormat v2.7 markdown on disk",
	Long: `Render an ISA-kind issue to canonical markdown at
<exfil-root>/isa/<slug>/ISA.md.

The exfil root is configurable via the BRAIN_ISA_EXFIL_ROOT environment
variable; it defaults to ${HOME}/.claude/PAI/MEMORY/WORK. The render is
atomic: a temp file is written first, then rename(2) makes the swap, so
readers never observe a half-written file.

The rendered file mirrors the IsaFormat v2.7 frontmatter (task, slug, effort,
phase, progress, mode, started, updated) and adds one new key — brain_id —
so downstream tools can trace markdown back to the substrate row.

isa-render is only valid for kind=isa. Calling it on any other kind exits 1.
Missing issue exits 1. Path-traversal-shaped slugs (someone INSERT'd a slug
with '/' or '..' directly into the DB) exit 2.

The path of the rendered file is printed to stdout. Exit 0 on success.

Examples:
  bd isa-render brain-isa-00001
  BRAIN_ISA_EXFIL_ROOT=/tmp/work bd isa-render brain-isa-00001`,
	Args: cobra.ExactArgs(1),
	Run:  runISARender,
}

var isaRenderAllCmd = &cobra.Command{
	Use:     "isa-render-all",
	GroupID: "issues",
	Short:   "Re-render every active ISA to disk (useful after upgrades or corruption)",
	Long: `Re-render every ISA in the substrate to its canonical exfil path.

The exfil root and atomicity semantics match bd isa-render. With --since,
only ISAs whose isa_updated_at is at or after the supplied RFC3339 timestamp
are re-rendered — useful for incremental refreshes.

For each ISA, one tab-separated line is printed to stdout:
  <id>\t<path>\t<status>

where status is "rendered" or "failed: <reason>". A per-ISA failure does not
stop the run; the exit code is 0 only if every render succeeded. Otherwise
exit 1 (and individual failure lines on stdout describe what went wrong).`,
	Args: cobra.NoArgs,
	Run:  runISARenderAll,
}

func init() {
	isaRenderCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(isaRenderCmd)

	isaRenderAllCmd.Flags().StringVar(&isaRenderAllSince, "since", "",
		"Only re-render ISAs with isa_updated_at >= this RFC3339 timestamp")
	rootCmd.AddCommand(isaRenderAllCmd)
}

// renderRow is the loaded shape we feed into isarender.Render. It's the
// joined view of issues + isa_sections for a single ISA.
type renderRow struct {
	ID        string
	Slug      string
	Title     string
	Phase     *string
	Effort    *string
	Mode      *string
	ProgressM int
	ProgressN int
	StartedAt *time.Time
	UpdatedAt *time.Time
	Sections  map[string]string
}

func runISARender(cmd *cobra.Command, args []string) {
	id := args[0]
	ctx := rootCtx

	db, err := openReadDB()
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	path, err := renderISAByIDWithDB(ctx, db, id)
	if err != nil {
		// Path-traversal and other input-shape errors → exit 2. errors.As
		// traverses the helper's fmt.Errorf("%w") wrapping, so typed checks
		// still fire on wrapped *PathTraversalError / *InputError.
		var pte *isarenderverb.PathTraversalError
		var ie *isarenderverb.InputError
		if errors.As(err, &pte) || errors.As(err, &ie) {
			exitRenderValidation(err)
		}
		// not-found and wrong-kind come back as plain errors with
		// "not found" / "not an ISA" substrings; the helper formats them so
		// the existing CLI contract (exit 1) is preserved.
		FatalErrorRespectJSON("%v", err)
	}

	emitRenderSuccess(id, path)
}

// loadRenderRow reads the issues row + isa_sections rows for id. Mirrors
// isa_show.loadISADoc but pulls `title` and uses *string / *time.Time for
// nullable scalars so the renderer can omit absent keys.
func loadRenderRow(ctx context.Context, db *sql.DB, id string) (*renderRow, error) {
	var (
		rowID, kind  string
		title        sql.NullString
		slug         sql.NullString
		isaPhase     sql.NullString
		isaProgressM sql.NullInt64
		isaProgressN sql.NullInt64
		isaEffort    sql.NullString
		isaMode      sql.NullString
		isaStartedAt sql.NullTime
		isaUpdatedAt sql.NullTime
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, title, slug, issue_type, isa_phase, isa_progress_m, isa_progress_n,
		       isa_effort, isa_mode, isa_started_at, isa_updated_at
		FROM issues
		WHERE id = ?`,
		id,
	).Scan(&rowID, &title, &slug, &kind, &isaPhase, &isaProgressM, &isaProgressN,
		&isaEffort, &isaMode, &isaStartedAt, &isaUpdatedAt)
	if err != nil {
		return nil, err
	}
	if kind != "isa" {
		return nil, &wrongKindError{id: rowID, kind: kind}
	}

	row := &renderRow{
		ID:        rowID,
		Slug:      slug.String,
		Title:     title.String,
		ProgressM: int(isaProgressM.Int64),
		ProgressN: int(isaProgressN.Int64),
		Sections:  map[string]string{},
	}
	if isaPhase.Valid {
		v := isaPhase.String
		row.Phase = &v
	}
	if isaEffort.Valid {
		v := isaEffort.String
		row.Effort = &v
	}
	if isaMode.Valid {
		v := isaMode.String
		row.Mode = &v
	}
	if isaStartedAt.Valid {
		t := isaStartedAt.Time
		row.StartedAt = &t
	}
	if isaUpdatedAt.Valid {
		t := isaUpdatedAt.Time
		row.UpdatedAt = &t
	}

	rows, err := db.QueryContext(ctx,
		"SELECT section_name, body FROM isa_sections WHERE issue_id = ?", rowID)
	if err != nil {
		return nil, fmt.Errorf("loading isa_sections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			return nil, fmt.Errorf("scanning isa_sections row: %w", err)
		}
		row.Sections[name] = body
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating isa_sections: %w", err)
	}

	return row, nil
}

// renderRowToDisk runs the pure-Go pipeline: resolve target, render bytes,
// atomic-write to disk. Returns the resolved target path on success.
func renderRowToDisk(row *renderRow) (string, error) {
	in := &isarenderverb.RenderInput{
		ID:        row.ID,
		Slug:      row.Slug,
		Title:     row.Title,
		Phase:     row.Phase,
		Effort:    row.Effort,
		Mode:      row.Mode,
		ProgressM: row.ProgressM,
		ProgressN: row.ProgressN,
		StartedAt: row.StartedAt,
		UpdatedAt: row.UpdatedAt,
		Sections:  row.Sections,
	}

	exfilRoot := isarenderverb.ResolveExfilRoot()
	target, err := isarenderverb.ResolveTargetPath(exfilRoot, row.Slug)
	if err != nil {
		return "", err
	}

	body, err := isarenderverb.Render(in)
	if err != nil {
		return "", err
	}

	if err := isarenderverb.WriteAtomic(target, body); err != nil {
		return "", err
	}
	return target, nil
}

// emitRenderSuccess prints the path to stdout. JSON mode emits a structured
// object so scripts can pipe the result without parsing free-form text.
func emitRenderSuccess(id, path string) {
	if jsonOutput {
		payload := map[string]interface{}{
			"id":   id,
			"path": path,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	fmt.Println(path)
}

// exitRenderValidation prints a validation error and exits 2. Matches the
// exit-2 contract used by patch and isa-section for path-traversal / shape
// errors.
func exitRenderValidation(err error) {
	msg := err.Error()
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(buildJSONError(msg, ""))
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	}
	os.Exit(2)
}

// runISARenderAll re-renders every ISA, optionally filtered by --since.
// Per-ISA failures do not abort the run; they're logged to stdout (one line
// per ISA) and the process exits 1 at the end if any failed.
func runISARenderAll(cmd *cobra.Command, args []string) {
	ctx := rootCtx

	var sinceTime *time.Time
	if isaRenderAllSince != "" {
		t, err := time.Parse(time.RFC3339, isaRenderAllSince)
		if err != nil {
			exitRenderValidation(&isarenderverb.InputError{
				Msg: fmt.Sprintf("--since must be RFC3339 (got %q): %v", isaRenderAllSince, err),
			})
		}
		sinceTime = &t
	}

	db, err := openReadDB()
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	ids, err := selectISAIDs(ctx, db, sinceTime)
	if err != nil {
		FatalErrorRespectJSON("listing ISAs: %v", err)
	}

	anyFailed := false
	for _, id := range ids {
		path, err := renderISAByIDWithDB(ctx, db, id)
		if err != nil {
			anyFailed = true
			fmt.Printf("%s\t\tfailed: %v\n", id, err)
			continue
		}
		fmt.Printf("%s\t%s\trendered\n", id, path)
	}

	if anyFailed {
		os.Exit(1)
	}
}

// selectISAIDs returns every ISA id, optionally filtered to those touched
// since the supplied timestamp. Order is by id ASC for stable output across
// runs.
func selectISAIDs(ctx context.Context, db *sql.DB, since *time.Time) ([]string, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if since != nil {
		rows, err = db.QueryContext(ctx,
			`SELECT id FROM issues
			 WHERE issue_type = 'isa' AND isa_updated_at >= ?
			 ORDER BY id ASC`,
			since.UTC(),
		)
	} else {
		rows, err = db.QueryContext(ctx,
			`SELECT id FROM issues
			 WHERE issue_type = 'isa'
			 ORDER BY id ASC`,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
