package storage_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/telemetry"
)

// sqlTxStore is a concrete store that implements SQLTxRunner, standing in
// for the embedded and server-mode stores. Only identity and the two seam
// methods are used; interface-promoted calls would panic on the embedded nil.
type sqlTxStore struct {
	storage.DoltStorage
	reads, writes int
}

func (s *sqlTxStore) RunReadSQLTx(ctx context.Context, fn func(ctx context.Context, tx storage.SQLTx) error) error {
	s.reads++
	return fn(ctx, nil)
}

func (s *sqlTxStore) RunWriteSQLTx(ctx context.Context, fn func(ctx context.Context, tx storage.SQLTx) error) error {
	s.writes++
	return fn(ctx, nil)
}

// rawOnlyStore is a store that predates the seam: it exposes a *sql.DB
// through RawDBAccessor and nothing else.
type rawOnlyStore struct {
	storage.DoltStorage
	db *sql.DB
}

func (s *rawOnlyStore) DB() *sql.DB           { return s.db }
func (s *rawOnlyStore) UnderlyingDB() *sql.DB { return s.db }

// bareStore exposes neither seam.
type bareStore struct {
	storage.DoltStorage
}

// noConnDriver registers a driver whose connections always fail, so a
// *sql.DB can exist for the adapter test without a real database.
type noConnDriver struct{}

var errNoConn = errors.New("sqltx_fake: no connections")

func (noConnDriver) Open(string) (driver.Conn, error) { return nil, errNoConn }

func init() { sql.Register("sqltx_fake", noConnDriver{}) }

// TestSQLTxRunnerFor_ReachesThroughCLIDecoratorChain pins the fix for
// task-t57uw from the decorator side: the CLI stacks HookGuardStore,
// telemetry, HookFiringStore and BrainExfiltrationDecorator on the concrete
// store, and the brain/ISA verbs must still find the store's own
// SQLTxRunner underneath, so the hooks keep running on the typed surface
// while raw slug and ISA SQL goes straight to the store.
func TestSQLTxRunnerFor_ReachesThroughCLIDecoratorChain(t *testing.T) {
	raw := &sqlTxStore{}
	var outer storage.DoltStorage = storage.NewHookGuardStore(raw, nil, storage.HookGuardOptions{Command: "patch", Store: "proof"})
	outer = telemetry.WrapStorage(outer)
	outer = storage.NewHookFiringStore(outer, nil)
	outer = storage.NewBrainExfiltrationDecorator(outer, nil)

	runner, ok := storage.SQLTxRunnerFor(outer)
	if !ok {
		t.Fatal("SQLTxRunnerFor(decorated store) = false, want the concrete store's runner")
	}
	if got, same := runner.(*sqlTxStore); !same || got != raw {
		t.Fatalf("SQLTxRunnerFor(decorated store) = %T, want the concrete *sqlTxStore", runner)
	}

	ctx := context.Background()
	sentinel := errors.New("callback ran")
	if err := runner.RunWriteSQLTx(ctx, func(context.Context, storage.SQLTx) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("RunWriteSQLTx = %v, want the callback's error", err)
	}
	if err := runner.RunReadSQLTx(ctx, func(context.Context, storage.SQLTx) error { return nil }); err != nil {
		t.Fatalf("RunReadSQLTx = %v, want nil", err)
	}
	if raw.writes != 1 || raw.reads != 1 {
		t.Fatalf("concrete store saw writes=%d reads=%d, want 1 and 1", raw.writes, raw.reads)
	}
}

func TestSQLTxRunnerFor_PrefersNativeRunnerOverRawDB(t *testing.T) {
	// A store that implements both (the server-mode store does) must be used
	// through its own runner so its locking and retry policy apply.
	raw := &nativeAndRawStore{}
	runner, ok := storage.SQLTxRunnerFor(storage.NewHookFiringStore(raw, nil))
	if !ok {
		t.Fatal("SQLTxRunnerFor = false, want true")
	}
	if _, same := runner.(*nativeAndRawStore); !same {
		t.Fatalf("SQLTxRunnerFor = %T, want the store itself rather than the RawDBAccessor adapter", runner)
	}
}

type nativeAndRawStore struct {
	sqlTxStore
}

func (s *nativeAndRawStore) DB() *sql.DB           { return nil }
func (s *nativeAndRawStore) UnderlyingDB() *sql.DB { return nil }

func TestSQLTxRunnerFor_FallsBackToRawDBAccessor(t *testing.T) {
	// A registered backend that only exposes RawDBAccessor keeps working
	// through the adapter; with a nil *sql.DB there is nothing to adapt.
	if _, ok := storage.SQLTxRunnerFor(&rawOnlyStore{db: nil}); ok {
		t.Fatal("SQLTxRunnerFor(RawDBAccessor with nil DB) = true, want false")
	}
	db, err := sql.Open("sqltx_fake", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	runner, ok := storage.SQLTxRunnerFor(storage.NewHookFiringStore(&rawOnlyStore{db: db}, nil))
	if !ok {
		t.Fatal("SQLTxRunnerFor(RawDBAccessor-only store) = false, want the adapter")
	}
	err = runner.RunReadSQLTx(context.Background(), func(context.Context, storage.SQLTx) error {
		t.Fatal("adapter ran the callback without a transaction")
		return nil
	})
	if !errors.Is(err, errNoConn) {
		t.Fatalf("RunReadSQLTx through the adapter = %v, want the driver's BeginTx error", err)
	}
}

func TestSQLTxRunnerFor_NoSeam(t *testing.T) {
	if _, ok := storage.SQLTxRunnerFor(nil); ok {
		t.Fatal("SQLTxRunnerFor(nil) = true, want false")
	}
	if _, ok := storage.SQLTxRunnerFor(storage.NewHookFiringStore(&bareStore{}, nil)); ok {
		t.Fatal("SQLTxRunnerFor(store with neither seam) = true, want false")
	}
}
