package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	patchverb "github.com/steveyegge/beads/internal/brain/verb/patch"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/spf13/cobra"
)

// Flag values for `bd patch`. Kept package-scoped so cobra can bind them in
// init() and the Run closure can read them.
var (
	patchField string
	patchValue string
)

var patchCmd = &cobra.Command{
	Use:     "patch <id>",
	GroupID: "issues",
	Short:   "Patch a single field on an issue (non-interactive)",
	Long: `Patch a single field on an issue, non-interactively.

bd patch is the primary mutation path for ISA-substrate fields:

  isa_phase, isa_progress_m, isa_progress_n,
  isa_effort, isa_mode, isa_started_at, isa_updated_at, slug

ISA fields are only valid on issues with kind=isa. Patching an ISA field on
any other kind exits with code 2.

Slug is special: it is patchable on any kind and does NOT touch
isa_updated_at.

Non-ISA, non-slug fields are routed to 'bd update' validation; if your
field is not in the patch allowlist, use 'bd update' instead.

Examples:
  bd patch isa-001 --field isa_phase --value BUILD
  bd patch isa-001 --field isa_progress_m --value 7
  bd patch bd-001  --field slug --value my-new-slug`,
	Args: cobra.ExactArgs(1),
	Run:  runPatch,
}

func init() {
	patchCmd.Flags().StringVar(&patchField, "field", "", "Field name to patch (required)")
	patchCmd.Flags().StringVar(&patchValue, "value", "", "New value for the field (required)")
	_ = patchCmd.MarkFlagRequired("field")
	_ = patchCmd.MarkFlagRequired("value")
	patchCmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(patchCmd)
}

// runPatch executes a single-field patch. The flow:
//  1. Resolve the issue and its store (via the routed-result helper).
//  2. If --field is an ISA field, run direct SQL UPDATE keyed on
//     issue_type='isa'. Use rows-affected to detect wrong-kind vs not-found.
//  3. If --field is "slug", run direct SQL UPDATE keyed on the id alone
//     (slug is kind-agnostic). Skip the isa_updated_at touch.
//  4. Otherwise, delegate to storage.UpdateIssue (the same path bd update
//     uses), so we inherit its allowlist and audit-trail behavior.
func runPatch(cmd *cobra.Command, args []string) {
	CheckReadonly("patch")

	id := args[0]
	field := patchField
	value := patchValue

	if field == "" {
		FatalErrorRespectJSON("--field is required")
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

	switch {
	case patchverb.IsISAField(field):
		runPatchISA(ctx, result, field, value)
	case field == patchverb.FieldSlug:
		runPatchSlug(ctx, result, value)
	default:
		runPatchPassthrough(ctx, result, field, value)
	}
}

// runPatchISA handles the seven ISA substrate fields. It enforces enum
// validity, the progress invariant, and the kind=isa precondition. The kind
// check is fused into the UPDATE statement (WHERE issue_type='isa') so we
// never racily UPDATE-then-SELECT.
func runPatchISA(ctx context.Context, result *RoutedResult, field, value string) {
	db, kind, err := openWriteDB(result)
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	// Enum validation.
	switch field {
	case patchverb.FieldISAPhase, patchverb.FieldISAEffort, patchverb.FieldISAMode:
		if vErr := patchverb.ValidateEnum(field, value); vErr != nil {
			exitValidation(vErr)
		}
	}

	// Progress validation needs the current row state. Fetch only when needed.
	var (
		sqlValue interface{} = value
	)
	switch field {
	case patchverb.FieldISAProgressM:
		curN, fetchErr := fetchCurrentProgressN(ctx, db, result.ResolvedID)
		if fetchErr != nil {
			FatalErrorRespectJSON("fetching current isa_progress_n: %v", fetchErr)
		}
		m, vErr := patchverb.ValidateProgressM(value, curN)
		if vErr != nil {
			exitValidation(vErr)
		}
		sqlValue = m
	case patchverb.FieldISAProgressN:
		curM, fetchErr := fetchCurrentProgressM(ctx, db, result.ResolvedID)
		if fetchErr != nil {
			FatalErrorRespectJSON("fetching current isa_progress_m: %v", fetchErr)
		}
		n, vErr := patchverb.ValidateProgressN(value, curM)
		if vErr != nil {
			exitValidation(vErr)
		}
		sqlValue = n
	}

	// Build and execute the gated UPDATE. We also bump isa_updated_at unless
	// the field IS isa_updated_at (in which case the caller's value wins).
	var (
		setClause string
		execArgs  []interface{}
	)
	if field == patchverb.FieldISAUpdatedAt {
		setClause = fmt.Sprintf("%s = ?", field)
		execArgs = []interface{}{sqlValue, result.ResolvedID}
	} else {
		setClause = fmt.Sprintf("%s = ?, isa_updated_at = NOW()", field)
		execArgs = []interface{}{sqlValue, result.ResolvedID}
	}
	stmt := fmt.Sprintf("UPDATE issues SET %s WHERE id = ? AND issue_type = 'isa'", setClause)

	res, execErr := db.ExecContext(ctx, stmt, execArgs...)
	if execErr != nil {
		FatalErrorRespectJSON("updating %s: %v", field, execErr)
	}
	affected, raErr := res.RowsAffected()
	if raErr != nil {
		FatalErrorRespectJSON("checking rows affected: %v", raErr)
	}
	if affected == 0 {
		// Either the row does not exist or it exists with a different kind.
		// Look up the row to disambiguate so the error message tells the user
		// which it is.
		actualKind, lookupErr := fetchIssueKind(ctx, db, result.ResolvedID)
		if lookupErr != nil {
			if errors.Is(lookupErr, sql.ErrNoRows) {
				FatalErrorRespectJSON("issue %s not found", result.ResolvedID)
			}
			FatalErrorRespectJSON("verifying issue kind: %v", lookupErr)
		}
		// Row exists with a non-isa kind — emit the validation message.
		exitValidation(patchverb.WrongKindError(field, actualKind))
	}

	commandDidWrite.Store(true)
	SetLastTouchedID(result.ResolvedID)
	emitSuccess(result.ResolvedID, field, value, kind)
}

// runPatchSlug writes the slug field. Slug is patchable on any kind and does
// NOT touch isa_updated_at. We use direct SQL (rather than UpdateIssue) so the
// behavior — including the no-touch on isa_updated_at — is explicit and
// auditable.
func runPatchSlug(ctx context.Context, result *RoutedResult, value string) {
	db, kind, err := openWriteDB(result)
	if err != nil {
		FatalErrorRespectJSON("opening database: %v", err)
	}

	res, execErr := db.ExecContext(ctx,
		"UPDATE issues SET slug = ? WHERE id = ?",
		value, result.ResolvedID,
	)
	if execErr != nil {
		FatalErrorRespectJSON("updating slug: %v", execErr)
	}
	affected, raErr := res.RowsAffected()
	if raErr != nil {
		FatalErrorRespectJSON("checking rows affected: %v", raErr)
	}
	if affected == 0 {
		FatalErrorRespectJSON("issue %s not found", result.ResolvedID)
	}

	commandDidWrite.Store(true)
	SetLastTouchedID(result.ResolvedID)
	emitSuccess(result.ResolvedID, patchverb.FieldSlug, value, kind)
}

// runPatchPassthrough delegates a non-ISA, non-slug field to UpdateIssue, the
// same path bd update uses. This inherits IsAllowedUpdateField gating, audit
// trail, and exfiltration for free.
func runPatchPassthrough(ctx context.Context, result *RoutedResult, field, value string) {
	issueStore := result.Store
	updates := map[string]interface{}{field: value}
	if err := issueStore.UpdateIssue(ctx, result.ResolvedID, updates, actor); err != nil {
		FatalErrorRespectJSON("updating %s: %v", field, err)
	}
	commandDidWrite.Store(true)
	SetLastTouchedID(result.ResolvedID)

	// Re-fetch for kind in JSON output.
	updated, _ := issueStore.GetIssue(ctx, result.ResolvedID)
	kind := ""
	if updated != nil {
		kind = string(updated.IssueType)
	}
	emitSuccess(result.ResolvedID, field, value, kind)
}

// openWriteDB extracts the *sql.DB from a routed store and returns it
// alongside the row's current issue_type (so success output can carry it).
// We grab kind here once to avoid an extra round-trip on the happy path.
func openWriteDB(result *RoutedResult) (*sql.DB, string, error) {
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

// fetchCurrentProgressN reads isa_progress_n for the row. COALESCE protects
// against NULL (uninitialized ISA rows can have NULL progress columns; the
// invariant treats NULL as 0).
func fetchCurrentProgressN(ctx context.Context, db *sql.DB, id string) (int, error) {
	var n sql.NullInt64
	err := db.QueryRowContext(ctx,
		"SELECT isa_progress_n FROM issues WHERE id = ?", id,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	if !n.Valid {
		return 0, nil
	}
	return int(n.Int64), nil
}

// fetchCurrentProgressM reads isa_progress_m for the row. NULL → 0.
func fetchCurrentProgressM(ctx context.Context, db *sql.DB, id string) (int, error) {
	var m sql.NullInt64
	err := db.QueryRowContext(ctx,
		"SELECT isa_progress_m FROM issues WHERE id = ?", id,
	).Scan(&m)
	if err != nil {
		return 0, err
	}
	if !m.Valid {
		return 0, nil
	}
	return int(m.Int64), nil
}

// fetchIssueKind reads issue_type for the row. Returns sql.ErrNoRows when the
// row does not exist; the caller distinguishes wrong-kind (exit 2) from
// not-found (exit 1) on that basis.
func fetchIssueKind(ctx context.Context, db *sql.DB, id string) (string, error) {
	var kind string
	err := db.QueryRowContext(ctx,
		"SELECT issue_type FROM issues WHERE id = ?", id,
	).Scan(&kind)
	return kind, err
}

// exitValidation prints a ValidationError to stderr (or stdout-JSON in JSON
// mode) and exits with code 2. This is the documented contract for invalid
// enum values, the progress invariant, and wrong-kind ISA patches. Accepts
// any error so callers can pass either *patchverb.ValidationError (the
// expected case) or a wrapped error without explicit type-assertion at every
// call site. We only reach this on validation failures, so a non-validation
// error is a programming bug — surface its Error() and still exit 2.
func exitValidation(err error) {
	msg := err.Error()
	if jsonOutput {
		// Use the same JSON envelope as FatalErrorRespectJSON so callers can
		// parse uniformly, but route to stdout per existing pattern.
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(buildJSONError(msg, ""))
	} else {
		fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	}
	os.Exit(2)
}

// emitSuccess prints the success envelope. JSON mode emits the documented
// {"id","field","value","kind"} object; plain mode prints a one-line summary.
func emitSuccess(id, field, value, kind string) {
	if jsonOutput {
		payload := map[string]interface{}{
			"id":    id,
			"field": field,
			"value": value,
			"kind":  kind,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	if kind != "" {
		fmt.Printf("Patched %s: %s = %s (kind=%s)\n", id, field, displayValue(value), kind)
	} else {
		fmt.Printf("Patched %s: %s = %s\n", id, field, displayValue(value))
	}
}

// displayValue trims excessive whitespace for human display while preserving
// the exact value in the JSON envelope. Long values are truncated with an
// ellipsis so success lines stay on a single terminal row.
func displayValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 80 {
		return v[:77] + "..."
	}
	return v
}
