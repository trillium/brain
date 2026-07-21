package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	isarenderverb "github.com/steveyegge/beads/internal/brain/verb/isarender"
)

// Flag values for `bd isa-render-pending`.
var (
	isaRenderPendingJSON bool
)

var isaRenderPendingCmd = &cobra.Command{
	Use:     "isa-render-pending",
	GroupID: "issues",
	Short:   "List ISAs whose on-disk markdown is stale relative to the substrate",
	Long: `List every ISA whose on-disk markdown is stale relative to the
substrate.

Pending state is derived, not persisted — there is no separate "needs render"
column. An ISA is pending when:

  - the target ISA.md file does not exist on disk, OR
  - the file's mtime is older than the row's isa_updated_at.

Auto-render hooks on bd patch and bd isa-section attempt a synchronous
post-commit render. If that render fails (disk full, permission denied,
filesystem unavailable), the brain write is NOT rolled back — the brain row
is canonical, the markdown is a shadow — and a warning is logged to stderr.
This verb surfaces every shadow that has fallen behind, so the operator can
re-run 'bd isa-render <id>' or 'bd isa-render-all' to bring the disk back
into sync.

Text output (one line per stale ISA):
  <id>\t<slug>\t<reason>

where reason is one of:
  - "missing"
  - "stale (file: <RFC3339>, db: <RFC3339>)"
  - "path error: <message>"   (slug somehow contains '..' or '/' despite
                                 the F1d regex)

JSON output (--json):
  [{ "id", "slug", "reason", "file_mtime", "isa_updated_at" }, ...]

Exit codes:
  0 — pending list emitted (empty if nothing stale)
  1 — listing failed catastrophically (DB error, etc.)

Examples:
  bd isa-render-pending
  bd isa-render-pending --json | jq '.[].id'`,
	Args: cobra.NoArgs,
	Run:  runISARenderPending,
}

func init() {
	isaRenderPendingCmd.Flags().BoolVar(&isaRenderPendingJSON, "json", false,
		"Emit results as a JSON array")
	rootCmd.AddCommand(isaRenderPendingCmd)
}

// renderPendingEntry is one row in the output. file_mtime is nullable in
// JSON ("missing" reason ⇒ no file ⇒ null mtime); the text serializer omits
// the file_mtime when it's not meaningful.
type renderPendingEntry struct {
	ID           string  `json:"id"`
	Slug         string  `json:"slug"`
	Reason       string  `json:"reason"`
	FileMtime    *string `json:"file_mtime"`
	ISAUpdatedAt string  `json:"isa_updated_at"`
}

func runISARenderPending(cmd *cobra.Command, args []string) {
	ctx := rootCtx

	db, err := openReadDB()
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	pending, err := collectRenderPending(ctx, db)
	if err != nil {
		FatalErrorRespectJSON("listing isa rows: %v", err)
	}

	emitRenderPending(pending)
}

// collectRenderPending walks every ISA row, resolves its target path, stats
// the file, and emits an entry for every row whose on-disk markdown is
// behind the substrate.
//
// Per the design note: path-resolution failures on individual rows are
// reported with reason="path error: ..." rather than aborting the whole
// listing. This protects the operator's ability to SEE that a malformed
// slug exists (so they can fix it via `bd patch <id> --field=slug
// --value=<good>`).
func collectRenderPending(ctx context.Context, db *sql.DB) ([]renderPendingEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, slug, isa_updated_at
		 FROM issues
		 WHERE issue_type = 'isa'
		   AND slug IS NOT NULL
		   AND slug != ''
		 ORDER BY id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	exfilRoot := isarenderverb.ResolveExfilRoot()

	var out []renderPendingEntry
	for rows.Next() {
		var (
			id           string
			slug         string
			isaUpdatedAt sql.NullTime
		)
		if err := rows.Scan(&id, &slug, &isaUpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning isa row: %w", err)
		}
		entry, ok := checkRenderPending(exfilRoot, id, slug, isaUpdatedAt)
		if ok {
			out = append(out, entry)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating isa rows: %w", err)
	}
	return out, nil
}

// checkRenderPending classifies a single ISA row. Returns (entry, true) when
// the row is pending (stale, missing, or has a path error); returns
// (_, false) when the row is fresh.
//
// Time comparison rule: filesystem mtimes are commonly truncated to second
// precision on macOS HFS+/APFS. We compare with second-truncation on both
// sides so a millisecond-level skew (DB precision ~microseconds vs FS ~1s)
// doesn't flap a freshly-rendered ISA into "stale".
func checkRenderPending(
	exfilRoot, id, slug string, isaUpdatedAt sql.NullTime,
) (renderPendingEntry, bool) {
	// isa_updated_at NULL is a brand-new row that hasn't been touched. We
	// can't compare against a missing timestamp, so we report it as
	// pending-missing if the file isn't there; if a file exists, treat as
	// fresh (we have no signal it's stale).
	dbTimeStr := ""
	var dbTimeTruncated time.Time
	dbTimeKnown := isaUpdatedAt.Valid
	if dbTimeKnown {
		dbTimeTruncated = isaUpdatedAt.Time.Truncate(time.Second)
		dbTimeStr = dbTimeTruncated.UTC().Format(time.RFC3339)
	}

	target, err := isarenderverb.ResolveTargetPath(exfilRoot, slug)
	if err != nil {
		return renderPendingEntry{
			ID:           id,
			Slug:         slug,
			Reason:       fmt.Sprintf("path error: %v", err),
			FileMtime:    nil,
			ISAUpdatedAt: dbTimeStr,
		}, true
	}

	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return renderPendingEntry{
				ID:           id,
				Slug:         slug,
				Reason:       "missing",
				FileMtime:    nil,
				ISAUpdatedAt: dbTimeStr,
			}, true
		}
		// Any other stat error (permission denied, etc.) — treat as a path
		// error so the operator sees something is wrong without losing the
		// rest of the listing.
		return renderPendingEntry{
			ID:           id,
			Slug:         slug,
			Reason:       fmt.Sprintf("path error: stat %s: %v", target, err),
			FileMtime:    nil,
			ISAUpdatedAt: dbTimeStr,
		}, true
	}

	fileMtime := info.ModTime().Truncate(time.Second)
	fileMtimeStr := fileMtime.UTC().Format(time.RFC3339)

	// File exists, DB timestamp unknown — assume fresh. Nothing to report.
	if !dbTimeKnown {
		return renderPendingEntry{}, false
	}

	// Stale: file is older than the DB row.
	if fileMtime.Before(dbTimeTruncated) {
		return renderPendingEntry{
			ID:           id,
			Slug:         slug,
			Reason:       fmt.Sprintf("stale (file: %s, db: %s)", fileMtimeStr, dbTimeStr),
			FileMtime:    &fileMtimeStr,
			ISAUpdatedAt: dbTimeStr,
		}, true
	}
	return renderPendingEntry{}, false
}

// emitRenderPending writes the pending list to stdout in the requested format.
// Empty input is fine — JSON mode emits "[]\n" (so jq pipelines don't break),
// text mode emits nothing (so shell loops don't fire on noise).
func emitRenderPending(entries []renderPendingEntry) {
	if isaRenderPendingJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if entries == nil {
			entries = []renderPendingEntry{}
		}
		if err := enc.Encode(entries); err != nil {
			FatalErrorRespectJSON("encoding json: %v", err)
		}
		return
	}
	// Text mode.
	for _, e := range entries {
		fmt.Printf("%s\t%s\t%s\n", e.ID, e.Slug, e.Reason)
	}
}
