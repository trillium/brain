// Federated search across all registered PAI stores on the same Dolt server.
//
// Implements `bd search --federated <query>` (also reachable as
// `brain search --federated <query>` via the brain wrapper). Reads the store
// registry at ~/.config/pai/stores.yaml, resolves each store's Dolt database
// name from its <beadsDir>/metadata.json, and runs a single-table query
// against every database on the already-open *sql.DB connection.
//
// Satisfies ISC-38..44 from /Users/trilliumsmith/code/brain/ISA.md:
//   - ISC-38: sectioned output, primary store first
//   - ISC-39: secondary sections only appear when they have results
//   - ISC-40: consistent "<id>  P<n>  <title>" row format
//   - ISC-41: --federated is an opt-in flag (default off; existing
//             single-store search is unchanged)
//   - ISC-42: exits 0 even when no secondary results
//   - ISC-43: unreachable / errored secondary databases are silently skipped
//   - ISC-44: any registered store can be the primary; "brain" is just
//             the entry point today

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
)

// primaryStoreName is the brain entry point. It is special only because the
// brain CLI wrapper opens that store as the connected `store` global; the
// federated walker treats it as ordinary once results are gathered (ISC-44).
const primaryStoreName = "brain"

// federatedRowLimit caps results per secondary store. The primary store is
// limited by the existing search command's --limit flag.
const federatedRowLimit = 10

// federatedRow is the canonical shape we display + serialise per matching
// issue, in both the primary and secondary store sections.
type federatedRow struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	IssueType string `json:"issue_type"`
	Priority  int    `json:"priority,omitempty"`
}

// federatedSection is one store's slice of results.
type federatedSection struct {
	Store  string         `json:"store"`
	Issues []federatedRow `json:"issues"`
}

// runFederatedSearch executes the federated search and prints results.
//
// Contract:
//   - Always runs the primary brain store query first.
//   - For each registered secondary store, attempts a cross-database SELECT
//     against the same *sql.DB. Errors per store are silently swallowed
//     (ISC-43) so one missing/unreachable database cannot fail the whole
//     command. The function never returns; it writes to stdout and lets the
//     caller `return`.
//   - Honors the global --json flag.
func runFederatedSearch(cmd *cobra.Command, query string) {
	ctx := rootCtx
	if ctx == nil {
		ctx = context.Background()
	}

	if store == nil {
		FatalError("no database connection available (%s)", diagHint())
	}

	// 1. Primary store — use the existing storage layer so the brain section
	//    matches what `bd search <query>` would return for the connected
	//    store (consistent semantics, ISC-38). We re-apply the same default
	//    "exclude closed" rule as the standard search.
	primary := collectPrimarySection(ctx, query, cmd)

	// 2. Secondary stores — load registry, resolve dbname per store, run a
	//    parameterised SELECT against the shared *sql.DB. Skip silently on
	//    any per-store error (ISC-43).
	secondaries := collectSecondarySections(ctx, query)

	// 3. Order: primary first, then secondaries alphabetical by store name.
	sections := make([]federatedSection, 0, 1+len(secondaries))
	if len(primary.Issues) > 0 {
		sections = append(sections, primary)
	}
	sort.Slice(secondaries, func(i, j int) bool {
		return secondaries[i].Store < secondaries[j].Store
	})
	for _, s := range secondaries {
		if len(s.Issues) > 0 {
			sections = append(sections, s)
		}
	}

	if jsonOutput {
		// --json: always return an array, even if empty. Callers parse a
		// stable shape; empty result is a valid answer (ISC-42).
		outputJSON(sections)
		return
	}

	if len(sections) == 0 {
		fmt.Printf("No issues found matching '%s' in any registered store\n", query)
		return
	}

	for i, s := range sections {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("=== %s ===\n", s.Store)
		for _, row := range s.Issues {
			fmt.Printf("%s %s  P%d  %s\n",
				ui.RenderStatusIcon(row.Status),
				row.ID,
				row.Priority,
				row.Title,
			)
		}
	}
}

// collectPrimarySection runs the standard search against the connected store
// and converts the result to federatedRow shape.
func collectPrimarySection(ctx context.Context, query string, cmd *cobra.Command) federatedSection {
	limit, _ := cmd.Flags().GetInt("limit")
	if limit <= 0 {
		limit = federatedRowLimit
	}

	// Match the default search behaviour: exclude closed issues.
	filter := types.IssueFilter{
		Limit:         limit,
		ExcludeStatus: []types.Status{types.StatusClosed},
	}

	section := federatedSection{Store: primaryStoreName, Issues: nil}

	issues, err := store.SearchIssues(ctx, query, filter)
	if err != nil {
		// Primary store failure is surfaced to stderr but does not abort —
		// the user may still want secondary results. ISC-43 applies broadly.
		fmt.Fprintf(os.Stderr, "warning: primary store search failed: %v\n", err)
		return section
	}

	section.Issues = make([]federatedRow, 0, len(issues))
	for _, iss := range issues {
		if iss == nil {
			continue
		}
		section.Issues = append(section.Issues, federatedRow{
			ID:        iss.ID,
			Title:     iss.Title,
			Status:    string(iss.Status),
			IssueType: string(iss.IssueType),
			Priority:  iss.Priority,
		})
	}
	return section
}

// collectSecondarySections walks the store registry and queries each
// non-primary store's `issues` table on the shared Dolt server. Errors on
// any individual store are dropped (ISC-43); the function only returns the
// successful sections.
func collectSecondarySections(ctx context.Context, query string) []federatedSection {
	registry, err := loadStoresRegistry()
	if err != nil {
		// Registry-load failure ≠ secondary-store failure. Surface once on
		// stderr and return no secondaries; we still emit the primary
		// section.
		fmt.Fprintf(os.Stderr, "warning: loading store registry: %v\n", err)
		return nil
	}
	if len(registry) == 0 {
		return nil
	}

	// We need a raw *sql.DB on the already-authenticated connection. Without
	// it federation is impossible — bail out silently (we already printed
	// the primary).
	accessor, ok := storage.UnwrapStore(store).(storage.RawDBAccessor)
	if !ok {
		return nil
	}
	db := accessor.UnderlyingDB()
	if db == nil {
		return nil
	}

	sections := make([]federatedSection, 0, len(registry))
	for name, beadsDir := range registry {
		if strings.EqualFold(name, primaryStoreName) {
			continue
		}
		dbName, err := resolveDoltDatabase(beadsDir)
		if err != nil || dbName == "" {
			continue // ISC-43: unreachable / missing metadata → skip silently
		}

		rows, err := queryStoreIssues(ctx, db, dbName, query)
		if err != nil || len(rows) == 0 {
			continue // ISC-39 + ISC-43: skip empty + skip errored, silently
		}

		sections = append(sections, federatedSection{
			Store:  name,
			Issues: rows,
		})
	}
	return sections
}

// resolveDoltDatabase reads <beadsDir>/metadata.json and returns the
// dolt_database field, falling back to the file's `database` field when
// dolt_database is absent. Returns an empty string + error when the file
// is missing or unreadable so the caller can skip the store cleanly.
//
// We deliberately do NOT use configfile.Load here — that helper performs
// legacy-file migration and other side effects we do not want during a
// read-only federation walk.
func resolveDoltDatabase(beadsDir string) (string, error) {
	path := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(path) //nolint:gosec // path is registry-sourced
	if err != nil {
		return "", err
	}
	// Minimal struct: only the fields we need. Unknown fields are ignored.
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
		Database     string `json:"database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", err
	}
	if meta.DoltDatabase != "" {
		return meta.DoltDatabase, nil
	}
	return meta.Database, nil
}

// queryStoreIssues runs the per-secondary SELECT against the shared Dolt
// connection. We use fully-qualified `<db>.issues` so a single connection
// pool can serve every store without `USE` statements (which are unreliable
// under connection reuse with database/sql pooling).
//
// The dbName is validated against a strict identifier rule before being
// interpolated into SQL — it is NOT a bind parameter (database names cannot
// be parameterised in MySQL). Validation prevents injection from a
// hand-edited registry file.
func queryStoreIssues(ctx context.Context, db *sql.DB, dbName, query string) ([]federatedRow, error) {
	if !isSafeIdentifier(dbName) {
		return nil, fmt.Errorf("invalid dolt database name %q", dbName)
	}

	// LIKE pattern with leading + trailing wildcards for case-insensitive
	// substring match on title. Dolt's default collation is case-insensitive,
	// matching the standard search behaviour.
	pattern := "%" + query + "%"

	sqlStmt := fmt.Sprintf(
		"SELECT id, title, status, issue_type, priority "+
			"FROM `%s`.issues "+
			"WHERE title LIKE ? AND status <> 'closed' "+
			"ORDER BY priority ASC, id ASC "+
			"LIMIT %d",
		dbName, federatedRowLimit,
	)

	rows, err := db.QueryContext(ctx, sqlStmt, pattern)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]federatedRow, 0, federatedRowLimit)
	for rows.Next() {
		var (
			id, title, status, issueType string
			priority                     int
		)
		if err := rows.Scan(&id, &title, &status, &issueType, &priority); err != nil {
			return nil, err
		}
		out = append(out, federatedRow{
			ID:        id,
			Title:     title,
			Status:    status,
			IssueType: issueType,
			Priority:  priority,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// isSafeIdentifier enforces a conservative whitelist for Dolt database
// names: letters, digits, and underscore. Database names cannot be bound
// as parameters in MySQL/Dolt, so this guards the only interpolated value
// in queryStoreIssues against injection from a tampered metadata.json.
func isSafeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

func init() {
	// Flag registration lives in this file so the federated surface stays
	// in one place (the standard search command does not need to know how
	// federation is wired). The Run handler in search.go checks this flag
	// at the top and delegates to runFederatedSearch.
	searchCmd.Flags().Bool(
		"federated",
		false,
		"Search across all registered PAI stores on the same Dolt server "+
			"(brain + secondaries). Sectioned output, primary store first.",
	)
}
