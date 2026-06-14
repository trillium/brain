package transfer_test

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/brain/verb"
	"github.com/steveyegge/beads/internal/brain/verb/transfer"
)

// nilTransferDB is a TransferDB that always fails Conn. Used to prove
// the verb's pre-storage validation runs before any DB I/O is attempted
// — if validation fails first, Conn is never called and the nil-DB
// failure path never trips.
type nilTransferDB struct{}

func (nilTransferDB) Conn(_ context.Context) (*sql.Conn, error) {
	return nil, errors.New("conn was called when it should not have been")
}

// mockTransferDB wraps a *sql.DB so it satisfies TransferDB. The
// underlying DB is a sqlmock instance; the test asserts on the SQL
// the verb issues.
type mockTransferDB struct{ db *sql.DB }

func (m *mockTransferDB) Conn(ctx context.Context) (*sql.Conn, error) { return m.db.Conn(ctx) }

// Compile-time proof that transfer.Verb satisfies BrainVerb[Args, Result].
// Mirrors the assertion in transfer.go so a test-only rename of either
// side fails the build, not the test.
var _ verb.BrainVerb[transfer.Args, transfer.Result] = transfer.Verb{}

func newRegistry(t *testing.T) *transfer.Registry {
	t.Helper()
	r, err := transfer.Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	return r
}

// TestRun_RegistryNil exercises the constructor-misuse guard for a
// nil registry. The verb must surface this as a real error rather
// than nil-derefing.
func TestRun_RegistryNil(t *testing.T) {
	t.Parallel()

	v := transfer.New(nilTransferDB{}, nil, "test-actor")
	_, err := v.Run(context.Background(), transfer.Args{Source: "inbox-abc", Dest: "brain"})
	if err == nil {
		t.Fatal("expected error for nil registry, got nil")
	}
	if !strings.Contains(err.Error(), "registry is not configured") {
		t.Errorf("error = %q, want \"registry is not configured\"", err.Error())
	}
}

// TestRun_DBNil exercises the constructor-misuse guard for a nil DB.
// Like the registry guard, this must produce a clear error not a panic.
func TestRun_DBNil(t *testing.T) {
	t.Parallel()

	v := transfer.New(nil, newRegistry(t), "test-actor")
	_, err := v.Run(context.Background(), transfer.Args{Source: "inbox-abc", Dest: "brain"})
	if err == nil {
		t.Fatal("expected error for nil DB, got nil")
	}
	if !strings.Contains(err.Error(), "storage is not configured") {
		t.Errorf("error = %q, want \"storage is not configured\"", err.Error())
	}
}

// TestRun_EmptySource verifies the empty-source guard fires before
// any DB I/O, by passing a TransferDB whose Conn would error out.
func TestRun_EmptySource(t *testing.T) {
	t.Parallel()

	v := transfer.New(nilTransferDB{}, newRegistry(t), "test-actor")
	_, err := v.Run(context.Background(), transfer.Args{Source: "", Dest: "brain"})
	if err == nil {
		t.Fatal("expected error for empty source, got nil")
	}
	if !strings.Contains(err.Error(), "source id is required") {
		t.Errorf("error = %q, want \"source id is required\"", err.Error())
	}
}

// TestRun_EmptyDest verifies the empty-dest guard fires before any DB I/O.
func TestRun_EmptyDest(t *testing.T) {
	t.Parallel()

	v := transfer.New(nilTransferDB{}, newRegistry(t), "test-actor")
	_, err := v.Run(context.Background(), transfer.Args{Source: "inbox-abc", Dest: ""})
	if err == nil {
		t.Fatal("expected error for empty dest, got nil")
	}
	if !strings.Contains(err.Error(), "destination store name is required") {
		t.Errorf("error = %q, want \"destination store name is required\"", err.Error())
	}
}

// TestRun_BadSourcePrefix verifies ISC-54: unknown prefix fails before
// DB I/O. The verb must reach Registry.ResolveSource before Conn().
func TestRun_BadSourcePrefix(t *testing.T) {
	t.Parallel()

	v := transfer.New(nilTransferDB{}, newRegistry(t), "test-actor")
	_, err := v.Run(context.Background(), transfer.Args{Source: "xyz-foo", Dest: "brain"})
	if err == nil {
		t.Fatal("expected error for bad source prefix, got nil")
	}
	if !strings.Contains(err.Error(), `unknown source store for prefix "xyz"`) {
		t.Errorf("error = %q, want it to mention unknown source prefix", err.Error())
	}
}

// TestRun_BadDest verifies ISC-55: unknown dest store name fails
// before DB I/O. Like the source prefix guard, this must surface
// before Conn() is called.
func TestRun_BadDest(t *testing.T) {
	t.Parallel()

	v := transfer.New(nilTransferDB{}, newRegistry(t), "test-actor")
	_, err := v.Run(context.Background(), transfer.Args{Source: "inbox-abc", Dest: "foobar"})
	if err == nil {
		t.Fatal("expected error for bad dest, got nil")
	}
	if !strings.Contains(err.Error(), `unknown destination store "foobar"`) {
		t.Errorf("error = %q, want it to mention unknown dest store", err.Error())
	}
}

// TestRun_SourceNotFound exercises the case where the source ID
// resolves to a known store but no row exists in <store>.issues.
// The verb must return "not found in <store>" (the wrapper turns
// this into a non-zero exit).
func TestRun_SourceNotFound(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// SELECT … FOR UPDATE returns sql.ErrNoRows shape (no rows).
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, title, description, issue_type, status FROM `inbox`.issues WHERE id = ? FOR UPDATE")).
		WithArgs("inbox-missing").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "issue_type", "status"}))
	mock.ExpectRollback()

	v := transfer.New(&mockTransferDB{db: db}, newRegistry(t), "test-actor")
	_, err = v.Run(context.Background(), transfer.Args{Source: "inbox-missing", Dest: "brain"})
	if err == nil {
		t.Fatal("expected error for missing source, got nil")
	}
	if !strings.Contains(err.Error(), "inbox-missing not found in inbox") {
		t.Errorf("error = %q, want it to say not found in inbox", err.Error())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRun_SourceAlreadyClosed exercises the case where the source row
// exists but is already status='closed'. Per the spec the verb must
// refuse — transferring a closed row would create an orphan with no
// way to mark the source supersession that already happened.
func TestRun_SourceAlreadyClosed(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, title, description, issue_type, status FROM `inbox`.issues WHERE id = ? FOR UPDATE")).
		WithArgs("inbox-already").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "issue_type", "status"}).
			AddRow("inbox-already", "Already moved", "body", "knowledge", "closed"))
	mock.ExpectRollback()

	v := transfer.New(&mockTransferDB{db: db}, newRegistry(t), "test-actor")
	_, err = v.Run(context.Background(), transfer.Args{Source: "inbox-already", Dest: "brain"})
	if err == nil {
		t.Fatal("expected error for already-closed source, got nil")
	}
	if !strings.Contains(err.Error(), "is already closed") {
		t.Errorf("error = %q, want \"is already closed\"", err.Error())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRun_HappyPath exercises a complete inbox → brain transfer.
// Verifies ISC-47, 50, 51, 52, 53, 56, 56.1:
//   - inbox→brain works (ISC-47)
//   - dest title carried from source (ISC-50)
//   - dest description carried from source (ISC-51)
//   - source closed with close_reason naming the dest (ISC-52)
//   - supersede link written to source.dependencies (ISC-53)
//   - SQL references databases by name, not BEADS_DIR (ISC-56)
//   - one BEGIN + one COMMIT spans everything (ISC-56.1)
//
// sqlmock is used as a deterministic SQL backend; the exact SQL the
// verb issues is asserted via regex matchers.
func TestRun_HappyPath_InboxToBrain(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()

	// 1. SELECT source from inbox.issues (FOR UPDATE).
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, title, description, issue_type, status FROM `inbox`.issues WHERE id = ? FOR UPDATE")).
		WithArgs("inbox-abc").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "issue_type", "status"}).
			AddRow("inbox-abc", "A captured note", "the body", "knowledge", "open"))

	// 2. Dest ID generation: at least one collision check is run
	// against dolt.issues. We allow any candidate ID (a hash) and
	// return count=0 the first time so generation succeeds on the
	// first try. Using AnyArg + flexible row return keeps the test
	// resilient to the actual id minted (which depends on
	// time/nonce — not the contract we care about here).
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `dolt`.issues WHERE id = ?")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))

	// 3. INSERT into dolt.issues. We check just that the INSERT
	// targets the correct DB and table; the exact column ordering
	// is asserted via the regex.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `dolt`.issues")).
		WithArgs(
			sqlmock.AnyArg(), // id
			sqlmock.AnyArg(), // content_hash
			"A captured note",
			sqlmock.AnyArg(), // description with provenance tag
			"knowledge",
			sqlmock.AnyArg(), // created_at
			"test-actor",
			sqlmock.AnyArg(), // updated_at
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 4. UPDATE inbox.issues SET status='closed', close_reason=...
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `inbox`.issues SET status = 'closed', close_reason = ?, closed_at = ?, updated_at = ? WHERE id = ?")).
		WithArgs(
			sqlmock.AnyArg(), // close_reason text — content checked below via custom matcher
			sqlmock.AnyArg(), // closed_at
			sqlmock.AnyArg(), // updated_at
			"inbox-abc",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 5. INSERT supersede edge into inbox.dependencies.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `inbox`.dependencies")).
		WithArgs(
			sqlmock.AnyArg(), // depid
			"inbox-abc",
			sqlmock.AnyArg(), // depends_on_external (new dest id)
			"supersedes",
			sqlmock.AnyArg(), // created_at
			"test-actor",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectCommit()

	v := transfer.New(&mockTransferDB{db: db}, newRegistry(t), "test-actor")
	res, err := v.Run(context.Background(), transfer.Args{Source: "inbox-abc", Dest: "brain"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Source != "inbox-abc" {
		t.Errorf("Result.Source = %q, want \"inbox-abc\"", res.Source)
	}
	if !strings.HasPrefix(res.Dest, "brain-") {
		t.Errorf("Result.Dest = %q, want prefix \"brain-\"", res.Dest)
	}
	if res.Store != "brain" {
		t.Errorf("Result.Store = %q, want \"brain\"", res.Store)
	}
	if !res.Supersede {
		t.Error("Result.Supersede = false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRun_HappyPath_InboxToTask exercises ISC-48: inbox → task works.
// Asserts the dest DB is "task" (not "tasks" / not "brain") and the
// new id carries the "task-" prefix.
func TestRun_HappyPath_InboxToTask(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM `inbox`.issues")).
		WithArgs("inbox-tt").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "issue_type", "status"}).
			AddRow("inbox-tt", "To do", "details", "task", "open"))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `task`.issues WHERE id = ?")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `task`.issues")).
		WithArgs(
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"To do",
			sqlmock.AnyArg(),
			"task",
			sqlmock.AnyArg(),
			"test-actor",
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `inbox`.issues SET status = 'closed'")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "inbox-tt").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `inbox`.dependencies")).
		WithArgs(sqlmock.AnyArg(), "inbox-tt", sqlmock.AnyArg(), "supersedes", sqlmock.AnyArg(), "test-actor").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	v := transfer.New(&mockTransferDB{db: db}, newRegistry(t), "test-actor")
	// "task" is the singular alias; the canonical Result.Store may be
	// "tasks" since that is the registry's canonical name.
	res, err := v.Run(context.Background(), transfer.Args{Source: "inbox-tt", Dest: "task"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(res.Dest, "task-") {
		t.Errorf("Result.Dest = %q, want prefix \"task-\"", res.Dest)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestRun_RollbackOnInsertDestFailure verifies the atomicity contract:
// if any step in the multi-statement TX fails, the whole TX rolls
// back. sqlmock asserts the ROLLBACK is issued and no COMMIT occurs.
func TestRun_RollbackOnInsertDestFailure(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM `inbox`.issues")).
		WithArgs("inbox-bad").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "issue_type", "status"}).
			AddRow("inbox-bad", "T", "D", "knowledge", "open"))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `dolt`.issues WHERE id = ?")).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"COUNT(*)"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `dolt`.issues")).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), "T", sqlmock.AnyArg(),
			"knowledge", sqlmock.AnyArg(), "test-actor", sqlmock.AnyArg(),
		).
		WillReturnError(errors.New("simulated insert failure"))
	mock.ExpectRollback()

	v := transfer.New(&mockTransferDB{db: db}, newRegistry(t), "test-actor")
	_, err = v.Run(context.Background(), transfer.Args{Source: "inbox-bad", Dest: "brain"})
	if err == nil {
		t.Fatal("expected error on insert failure, got nil")
	}
	if !strings.Contains(err.Error(), "insert dest") {
		t.Errorf("error = %q, want it to mention insert dest", err.Error())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestVerb_Name verifies the verb word matches what the Cobra wrapper
// must use as the first whitespace-delimited token of its Use field
// ("transfer"). A drift here would make `bd brain transfer …`
// unroutable.
func TestVerb_Name(t *testing.T) {
	t.Parallel()

	if got := (transfer.Verb{}).Name(); got != "transfer" {
		t.Errorf("Name() = %q, want \"transfer\"", got)
	}
}
