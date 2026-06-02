package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	isasectionverb "github.com/steveyegge/beads/internal/brain/verb/isasection"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/spf13/cobra"
)

// Flag values for `bd isa-section`. Package-scoped so cobra can bind them in
// init() and the Run closure can read them.
var (
	isaSectionValueFromFile string
	isaSectionValueStdin    bool
)

var isaSectionCmd = &cobra.Command{
	Use:     "isa-section <id> <section-name>",
	GroupID: "issues",
	Short:   "Upsert one of the twelve canonical ISA sections on an isa-kind issue",
	Long: `Set one of the twelve canonical ISA document sections on an isa-kind issue.

bd isa-section writes to the isa_sections table (issue_id, section_name, body)
as an UPSERT, and atomically touches issues.isa_updated_at = NOW() so the row's
"last touched" semantics match the rest of the ISA substrate.

Valid section names (lower_snake_case, case-sensitive):
  changelog, constraints, criteria, decisions, features, goal,
  out_of_scope, principles, problem, test_strategy, verification, vision

Input source is required; exactly one of:

  --value-from-file <path>   read the section body from a file
  --value-stdin              read the section body from stdin

isa-section is only valid for kind=isa. Calling it on any other kind exits 2.
Missing issue exits 1.

Examples:
  bd isa-section isa-001 problem    --value-from-file ./sections/problem.md
  cat changelog.md | bd isa-section isa-001 changelog --value-stdin`,
	Args: cobra.ExactArgs(2),
	Run:  runISASection,
}

func init() {
	isaSectionCmd.Flags().StringVar(&isaSectionValueFromFile, "value-from-file", "",
		"Path to a file whose contents become the section body")
	isaSectionCmd.Flags().BoolVar(&isaSectionValueStdin, "value-stdin", false,
		"Read the section body from stdin")
	isaSectionCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(isaSectionCmd)
}

// runISASection executes the verb. The flow:
//  1. Validate section name and input-source flags (no DB work yet).
//  2. Read the body bytes from --value-from-file OR stdin.
//  3. Resolve the issue via the same routing helper bd patch uses, so we
//     reuse local/remote routing for free.
//  4. Open a write DB handle and run the two SQL statements in a single
//     transaction:
//       - UPSERT into isa_sections
//       - UPDATE issues SET isa_updated_at = NOW() WHERE id=? AND issue_type='isa'
//     If the UPDATE affects zero rows, we look up the row's issue_type to
//     distinguish wrong-kind (exit 2) from not-found (exit 1), then roll back.
func runISASection(cmd *cobra.Command, args []string) {
	CheckReadonly("isa-section")

	id := args[0]
	sectionName := args[1]

	// Pure-Go validation first — never open the DB on a guaranteed-failing call.
	if err := isasectionverb.ValidateSectionName(sectionName); err != nil {
		exitISASectionValidation(err)
	}

	// Exactly one input source must be supplied. Both-missing or both-set
	// are user errors — exit 2 with a clear message.
	switch {
	case isaSectionValueFromFile != "" && isaSectionValueStdin:
		exitISASectionValidation(&isasectionverb.ValidationError{
			Msg: "--value-from-file and --value-stdin are mutually exclusive; pick one",
		})
	case isaSectionValueFromFile == "" && !isaSectionValueStdin:
		exitISASectionValidation(&isasectionverb.ValidationError{
			Msg: "one of --value-from-file or --value-stdin is required",
		})
	}

	body, err := readISASectionBody()
	if err != nil {
		FatalErrorRespectJSON("reading section body: %v", err)
	}

	ctx := rootCtx

	result, err := resolveAndGetIssueWithRouting(ctx, store, id)
	if err != nil {
		if result != nil {
			result.Close()
		}
		FatalErrorRespectJSON("resolving %s: %v", id, err)
	}
	if result == nil || result.Issue == nil {
		if result != nil {
			result.Close()
		}
		FatalErrorRespectJSON("issue %s not found", id)
	}
	defer result.Close()

	if err := validateIssueUpdatable(id, result.Issue); err != nil {
		FatalErrorRespectJSON("%s", err)
	}

	db, kind, err := openISASectionWriteDB(result)
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	if err := upsertISASection(ctx, db, result.ResolvedID, sectionName, body); err != nil {
		// Validation-shaped errors (wrong kind, not found post-resolve) come
		// back as *isasectionverb.ValidationError or sql.ErrNoRows.
		var vErr *isasectionverb.ValidationError
		if errors.As(err, &vErr) {
			exitISASectionValidation(err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			FatalErrorRespectJSON("issue %s not found", result.ResolvedID)
		}
		FatalErrorRespectJSON("writing isa-section: %v", err)
	}

	commandDidWrite.Store(true)
	SetLastTouchedID(result.ResolvedID)
	emitISASectionSuccess(result.ResolvedID, sectionName, len(body), kind)
}

// readISASectionBody returns the section body bytes from whichever input
// source was selected. The flag-parser has already guaranteed exactly one
// source is set. Returns the raw bytes — no trimming, no normalization —
// because changelog and code-fence sections care about exact whitespace.
func readISASectionBody() ([]byte, error) {
	if isaSectionValueFromFile != "" {
		body, err := os.ReadFile(isaSectionValueFromFile)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", isaSectionValueFromFile, err)
		}
		return body, nil
	}
	// stdin path.
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("reading stdin: %w", err)
	}
	return body, nil
}

// openISASectionWriteDB extracts the raw *sql.DB from a routed store and the
// row's current issue_type for the success envelope. Mirrors openWriteDB in
// patch.go; kept separate so the two verbs can evolve independently.
func openISASectionWriteDB(result *RoutedResult) (*sql.DB, string, error) {
	accessor, ok := storage.UnwrapStore(result.Store).(storage.RawDBAccessor)
	if !ok {
		return nil, "", fmt.Errorf("store does not expose raw DB access")
	}
	db := accessor.DB()
	if db == nil {
		return nil, "", fmt.Errorf("store DB is nil")
	}
	kind := ""
	if result.Issue != nil {
		kind = string(result.Issue.IssueType)
	}
	return db, kind, nil
}

// upsertISASection runs the two-statement write in a single transaction:
//  1. UPSERT into isa_sections.
//  2. UPDATE issues SET isa_updated_at = NOW() WHERE id=? AND issue_type='isa'.
//
// If the UPDATE affects zero rows, we look up the row's issue_type. A row
// with a non-isa kind surfaces as *ValidationError (caller maps to exit 2).
// A missing row surfaces as sql.ErrNoRows (caller maps to exit 1). In both
// failure modes the transaction is rolled back, so no isa_sections row is
// left behind.
func upsertISASection(ctx context.Context, db *sql.DB, id, section string, body []byte) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Always attempt rollback on the unhappy paths. A successful commit makes
	// rollback a no-op (sql.ErrTxDone), which we deliberately swallow.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO isa_sections (issue_id, section_name, body, updated_at)
		 VALUES (?, ?, ?, NOW())
		 ON DUPLICATE KEY UPDATE body = VALUES(body), updated_at = NOW()`,
		id, section, string(body),
	); err != nil {
		return fmt.Errorf("upsert isa_sections: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE issues SET isa_updated_at = NOW()
		 WHERE id = ? AND issue_type = 'isa'`,
		id,
	)
	if err != nil {
		return fmt.Errorf("touch isa_updated_at: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		// Disambiguate wrong-kind vs not-found via a separate lookup. This
		// runs inside the same transaction so the read sees the same snapshot
		// we just (failed to) write against.
		var actualKind string
		lookupErr := tx.QueryRowContext(ctx,
			"SELECT issue_type FROM issues WHERE id = ?", id,
		).Scan(&actualKind)
		if lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return sql.ErrNoRows
			}
			return fmt.Errorf("verifying issue kind: %w", lookupErr)
		}
		return &isasectionverb.ValidationError{
			Msg: fmt.Sprintf("bd isa-section requires kind=isa, got issue_type=%s", actualKind),
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// exitISASectionValidation prints a ValidationError to stderr (or JSON to
// stdout in JSON mode) and exits 2. Same contract as exitValidation in
// patch.go; kept separate to avoid cross-verb coupling.
func exitISASectionValidation(err error) {
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

// emitISASectionSuccess prints the success envelope. JSON mode emits a
// structured object with the byte length (callers asked for value semantics,
// not a full echo of the body — which can be megabytes).
func emitISASectionSuccess(id, section string, bytesWritten int, kind string) {
	if jsonOutput {
		payload := map[string]interface{}{
			"id":            id,
			"section":       section,
			"bytes_written": bytesWritten,
			"kind":          kind,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	if kind != "" {
		fmt.Printf("Wrote isa-section %s.%s (%d bytes, kind=%s)\n",
			id, section, bytesWritten, kind)
	} else {
		fmt.Printf("Wrote isa-section %s.%s (%d bytes)\n",
			id, section, bytesWritten)
	}
}
