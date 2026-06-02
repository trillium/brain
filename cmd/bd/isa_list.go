package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	isashowverb "github.com/steveyegge/beads/internal/brain/verb/isashow"
	"github.com/spf13/cobra"
)

// Flag values for `bd isa-list`. Package-scoped so cobra can bind them in
// init() and the Run closure can read them.
var (
	isaListActive bool
	isaListJSON   bool
)

var isaListCmd = &cobra.Command{
	Use:     "isa-list",
	GroupID: "issues",
	Short:   "List every ISA (or only active ISAs)",
	Long: `List all ISA-kind issues with their phase, progress, effort, and last
update. With --active, filters out ISAs whose phase is LEARN (i.e. completed
runs that are in the learning/post-mortem phase).

  bd isa-list              # text table, every ISA
  bd isa-list --active     # text table, ISAs not in LEARN phase
  bd isa-list --json       # JSON array of compact objects

Empty result: exit 0 with no output.`,
	Args: cobra.NoArgs,
	Run:  runISAList,
}

func init() {
	isaListCmd.Flags().BoolVar(&isaListActive, "active", false,
		"Filter to ISAs whose phase is not LEARN")
	isaListCmd.Flags().BoolVar(&isaListJSON, "json", false,
		"Emit a JSON array of compact ISA objects")
	rootCmd.AddCommand(isaListCmd)
}

// isaListRow is the compact wire shape for one ISA in `bd isa-list --json`.
// Field order matches the struct definition — that's the JSON key order
// (encoding/json preserves struct order).
type isaListRow struct {
	ID           string                 `json:"id"`
	Slug         string                 `json:"slug"`
	ISAPhase     string                 `json:"isa_phase"`
	ISAProgress  isashowverb.ISAProgress `json:"isa_progress"`
	ISAEffort    string                 `json:"isa_effort"`
	ISAUpdatedAt *time.Time             `json:"isa_updated_at"`
	Title        string                 `json:"title"`
}

func runISAList(cmd *cobra.Command, args []string) {
	ctx := rootCtx

	db, err := openReadDB()
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	rows, err := loadISAList(ctx, db, isaListActive)
	if err != nil {
		FatalErrorRespectJSON("listing isas: %v", err)
	}

	if isaListJSON {
		emitISAListJSON(rows)
		return
	}
	emitISAListText(rows)
}

// loadISAList runs the SELECT against issues, filtered to kind='isa' and
// optionally excluding phase='LEARN'. Returns a slice of compact rows in a
// deterministic order: most-recently-updated first, then id ascending for
// rows with the same (or NULL) timestamp.
func loadISAList(ctx context.Context, db *sql.DB, activeOnly bool) ([]isaListRow, error) {
	q := `SELECT id, slug, title, isa_phase, isa_progress_m, isa_progress_n,
	             isa_effort, isa_updated_at
	      FROM issues
	      WHERE issue_type = 'isa'`
	if activeOnly {
		// COALESCE so a NULL phase still passes the active filter — an ISA
		// without a phase set hasn't started LEARN.
		q += ` AND COALESCE(isa_phase, '') != 'LEARN'`
	}
	q += ` ORDER BY isa_updated_at DESC, id ASC`

	dbRows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer dbRows.Close()

	var out []isaListRow
	for dbRows.Next() {
		var (
			id           string
			slug         sql.NullString
			title        sql.NullString
			phase        sql.NullString
			progressM    sql.NullInt64
			progressN    sql.NullInt64
			effort       sql.NullString
			isaUpdatedAt sql.NullTime
		)
		if err := dbRows.Scan(&id, &slug, &title, &phase, &progressM, &progressN,
			&effort, &isaUpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		row := isaListRow{
			ID:       id,
			Slug:     slug.String,
			ISAPhase: phase.String,
			ISAProgress: isashowverb.ISAProgress{
				M: int(progressM.Int64),
				N: int(progressN.Int64),
			},
			ISAEffort: effort.String,
			Title:     title.String,
		}
		if isaUpdatedAt.Valid {
			t := isaUpdatedAt.Time
			row.ISAUpdatedAt = &t
		}
		out = append(out, row)
	}
	if err := dbRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating: %w", err)
	}
	return out, nil
}

// emitISAListJSON writes a JSON array. Nil/empty slice → "[]" (not "null") so
// downstream JSON consumers don't choke. encoding/json emits "null" for nil
// slices — we materialize an empty slice to force "[]".
func emitISAListJSON(rows []isaListRow) {
	if rows == nil {
		rows = []isaListRow{}
	}
	out, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		FatalErrorRespectJSON("marshaling isa list: %v", err)
	}
	fmt.Println(string(out))
}

// emitISAListText writes a tab-aligned table to stdout. Empty result → no
// output, exit 0. Headers are included only when there's at least one row so
// scripts that grep for ids on stdout don't get a header surprise.
func emitISAListText(rows []isaListRow) {
	if len(rows) == 0 {
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSLUG\tPHASE\tPROGRESS\tEFFORT\tUPDATED\tTITLE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID,
			emptyDash(r.Slug),
			emptyDash(r.ISAPhase),
			formatProgress(r.ISAProgress),
			emptyDash(r.ISAEffort),
			formatTime(r.ISAUpdatedAt),
			truncateISATitle(r.Title, 60),
		)
	}
	_ = w.Flush()
}

// emptyDash returns "-" for the empty string so the columnar output stays
// aligned and the absence of a value is visible at a glance.
func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatProgress emits "M/N" or "-" when both fields are zero, which is the
// usual unset state for a freshly inserted ISA row.
func formatProgress(p isashowverb.ISAProgress) string {
	if p.M == 0 && p.N == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", p.M, p.N)
}

// formatTime renders a *time.Time as date-only UTC, or "-" when nil.
// Date-only keeps the column narrow while still being scannable.
func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}

// truncateISATitle clips the title to max runes with a trailing ellipsis when
// truncated. Operates on bytes — sufficient for ASCII titles which dominate
// ISA usage; multibyte titles get a slightly tighter visual cap.
func truncateISATitle(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
