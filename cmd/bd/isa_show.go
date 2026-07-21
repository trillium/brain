package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	isashowverb "github.com/steveyegge/beads/internal/brain/verb/isashow"
	"github.com/steveyegge/beads/internal/storage"
)

// Flag values for `bd isa-show`. Package-scoped so cobra can bind them in
// init() and the Run closure can read them.
var (
	isaShowSection string
	isaShowJSON    bool
)

var isaShowCmd = &cobra.Command{
	Use:     "isa-show <id>",
	GroupID: "issues",
	Short:   "Show an ISA document (full doc or a single section) without markdown rendering",
	Long: `Read an ISA-kind issue's full document (or a single section) directly from
the substrate.

By default isa-show emits markdown assembled from the twelve canonical
sections in spec order, suitable for piping to a pager. Use --json to emit
the stable JSON document shape consumed by tooling.

  bd isa-show isa-001                      # markdown (default)
  bd isa-show isa-001 --section=problem    # just the Problem body, no header
  bd isa-show isa-001 --json               # full JSON doc
  bd isa-show isa-001 --json --section=X   # --json wins; --section is ignored

isa-show is only valid for kind=isa. Calling it on any other kind exits 1.
Missing issue exits 1.`,
	Args: cobra.ExactArgs(1),
	Run:  runISAShow,
}

func init() {
	isaShowCmd.Flags().StringVar(&isaShowSection, "section", "",
		"Restrict output to a single canonical section name (e.g. 'problem', 'changelog')")
	isaShowCmd.Flags().BoolVar(&isaShowJSON, "json", false,
		"Emit the stable JSON document shape instead of rendered markdown")
	isaShowCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(isaShowCmd)
}

// runISAShow executes the verb. The flow:
//  1. Open the read DB.
//  2. Query the issues row, gating on issue_type='isa'. Three terminal cases:
//     not-found, wrong-kind, and the happy path.
//  3. Query the isa_sections rows.
//  4. Build the ISADoc.
//  5. Emit per the flag-precedence rule: --json wins; --section without
//     --json filters output to that section's body.
//
// Precedence note: when both --json and --section are supplied, --json wins
// and --section is ignored. A one-line stderr warning is printed so the user
// can correct their invocation. This keeps a single deterministic JSON shape.
func runISAShow(cmd *cobra.Command, args []string) {
	id := args[0]
	ctx := rootCtx

	if isaShowJSON && isaShowSection != "" {
		fmt.Fprintln(os.Stderr,
			"Warning: --section is ignored when --json is set; emitting full doc JSON")
	}

	db, err := openReadDB()
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	doc, err := loadISADoc(ctx, db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			FatalErrorRespectJSON("isa not found: %s", id)
		}
		var wk *wrongKindError
		if errors.As(err, &wk) {
			FatalErrorRespectJSON("%s is not an ISA (kind=%s)", wk.id, wk.kind)
		}
		FatalErrorRespectJSON("reading isa: %v", err)
	}

	emitISAShow(doc)
}

// openReadDB returns the embedded-Dolt sql.DB from the global store. The
// command surface is read-only so we don't go through the routing layer
// (resolveAndGetIssueWithRouting) — that layer is for writes which need to
// honor proxy routing. Reads against the local store are fine.
func openReadDB() (*sql.DB, error) {
	accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
	if !ok {
		return nil, fmt.Errorf("store does not expose raw DB access")
	}
	db := accessor.DB()
	if db == nil {
		return nil, fmt.Errorf("store DB is nil")
	}
	return db, nil
}

// wrongKindError signals that the requested id exists but is not an ISA.
// Caller maps to a stable user-facing error string.
type wrongKindError struct {
	id   string
	kind string
}

func (e *wrongKindError) Error() string {
	return fmt.Sprintf("%s is not an ISA (kind=%s)", e.id, e.kind)
}

// loadISADoc fetches the issues row + isa_sections rows for id, returning a
// fully populated ISADoc. Returns sql.ErrNoRows when the id is missing, and
// *wrongKindError when the row exists but is not kind=isa.
//
// Slug is read from issues.slug (added in F1a; F1d will auto-populate).
// Title is queried but not surfaced in the JSON doc — F1c-2 keeps the wire
// shape locked to the ISC-16 fields.
func loadISADoc(ctx context.Context, db *sql.DB, id string) (*isashowverb.ISADoc, error) {
	var (
		rowID        string
		slug         sql.NullString
		kind         string
		isaPhase     sql.NullString
		isaProgressM sql.NullInt64
		isaProgressN sql.NullInt64
		isaEffort    sql.NullString
		isaMode      sql.NullString
		isaStartedAt sql.NullTime
		isaUpdatedAt sql.NullTime
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, slug, issue_type, isa_phase, isa_progress_m, isa_progress_n,
		       isa_effort, isa_mode, isa_started_at, isa_updated_at
		FROM issues
		WHERE id = ?`,
		id,
	).Scan(&rowID, &slug, &kind, &isaPhase, &isaProgressM, &isaProgressN,
		&isaEffort, &isaMode, &isaStartedAt, &isaUpdatedAt)
	if err != nil {
		return nil, err // includes sql.ErrNoRows
	}
	if kind != "isa" {
		return nil, &wrongKindError{id: rowID, kind: kind}
	}

	doc := &isashowverb.ISADoc{
		ID:       rowID,
		Slug:     slug.String,
		Kind:     kind,
		ISAPhase: isaPhase.String,
		ISAProgress: isashowverb.ISAProgress{
			M: int(isaProgressM.Int64),
			N: int(isaProgressN.Int64),
		},
		ISAEffort: isaEffort.String,
		ISAMode:   isaMode.String,
		Sections:  map[string]string{},
	}
	if isaStartedAt.Valid {
		t := isaStartedAt.Time
		doc.ISAStartedAt = &t
	}
	if isaUpdatedAt.Valid {
		t := isaUpdatedAt.Time
		doc.ISAUpdatedAt = &t
	}

	// Load sections. A row with zero sections is valid — return the doc with
	// an empty map, not an error.
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
		doc.Sections[name] = body
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating isa_sections: %w", err)
	}

	return doc, nil
}

// emitISAShow writes the doc to stdout per the flag precedence:
//
//	--json                → full JSON doc (--section is ignored, warning above)
//	--section=<name>      → raw body of that section, exit 1 if section is
//	                        not stored on this ISA
//	neither               → rendered markdown
func emitISAShow(doc *isashowverb.ISADoc) {
	if isaShowJSON {
		out, err := doc.MarshalJSONIndent()
		if err != nil {
			FatalErrorRespectJSON("marshaling isa doc: %v", err)
		}
		fmt.Println(string(out))
		return
	}

	if isaShowSection != "" {
		body, ok := doc.Sections[isaShowSection]
		if !ok {
			FatalErrorRespectJSON("section %q not set on %s", isaShowSection, doc.ID)
		}
		fmt.Print(isashowverb.RenderSection(body))
		// Add a trailing newline if the body lacks one so terminal output
		// doesn't run into the next prompt.
		if len(body) == 0 || body[len(body)-1] != '\n' {
			fmt.Println()
		}
		return
	}

	fmt.Print(isashowverb.RenderMarkdown(doc))
}
