package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// The release and history edge (docs/design/brain-prefix-release.md): every
// refusal, the event row shape, the claim-event append, and the
// no-silent-path guarantee. The claim-side conflict logic itself is tested
// in unified_namespaces_test.go.

// releaseSeqExpect writes the expectations a successful release must issue,
// in order, into the mock: row read, live bead count, event append, row
// delete, then the allowed_prefixes unclaim.
func releaseSeqExpect(t *testing.T, mock sqlmock.Sqlmock, prefix, owner, reason string, liveBeads int64) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs(prefix).
		WillReturnRows(rowsOf(t, "store,owner_reason", []string{owner, "operator-added"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM issues WHERE id = ? OR id LIKE CONCAT(?, '-%')")).
		WithArgs(prefix, prefix).
		WillReturnRows(rowsOf(t, "COUNT(*)", []string{intToString(t, liveBeads)}))
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS brain_store_prefix_events")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_store_prefix_events (id, event_type, prefix, actor, old_store, new_store, reason, bead_count, event_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)")).
		WithArgs(sqlmock.AnyArg(), "release", prefix, owner, owner, "", reason, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM brain_store_prefixes WHERE `prefix` = ?")).
		WithArgs(prefix).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// intToString renders a bead-count literal for the mock rows.
func intToString(t *testing.T, n int64) string {
	t.Helper()
	return strconv.FormatInt(n, 10)
}

func TestRecordStorePrefixClaimEventAppendedInSameTx(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("proto").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_store_prefixes (`prefix`,`store`,`owner_reason`,`declared_by`,`observed_by`,`bead_count`,`ambiguous`) VALUES (?, ?, ?, '', '', 0, 0)")).
		WithArgs("proto", "task", "operator-added").
		WillReturnResult(sqlmock.NewResult(1, 1))
	// Provisioning, live count, then the claim event — all inside the caller's
	// transaction, so the ownership row and the event land together or not at
	// all. The event carries the live count (2 wide-only beads already sit
	// under the fresh claim), not the always-zero row default.
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS brain_store_prefix_events")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM issues WHERE id = ? OR id LIKE CONCAT(?, '-%')")).
		WithArgs("proto", "proto").
		WillReturnRows(rowsOf(t, "COUNT(*)", []string{"2"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_store_prefix_events (id, event_type, prefix, actor, old_store, new_store, reason, bead_count, event_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)")).
		WithArgs(sqlmock.AnyArg(), "claim", "proto", "task", "", "task", "operator-added", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("task", "allowed_prefixes").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO brain_unified_config (`store`,`key`,value) VALUES (?, ?, ?)")).
		WithArgs("task", "allowed_prefixes", "proto").
		WillReturnResult(sqlmock.NewResult(1, 1))

	if _, err := RecordStorePrefix(ctx, tx, "task", "proto", "operator-added"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("claim transaction must append exactly one event row inside the tx: %v", err)
	}
}

// Nothing changed → nothing appended: the events row must not exist for a
// no-op claim or a refused rival claim, because appending to an append-only
// trail for a no-write act would fabricate history.
func TestRecordStorePrefixNoEventWithoutOwnershipChange(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	// Already-owned no-op: one read, no event.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("task").
		WillReturnRows(rowsOf(t, "store,owner_reason", []string{"task", "store-created"}))
	if _, err := RecordStorePrefix(ctx, tx, "task", "task", "operator-added"); err != nil {
		t.Fatalf("already-owned claim: %v", err)
	}
	// Rival claim refused: one read, no event, no rewrite.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("proto").
		WillReturnRows(rowsOf(t, "store,owner_reason", []string{"stories", "operator-added"}))
	if _, err := RecordStorePrefix(ctx, tx, "task", "proto", "operator-added"); err == nil {
		t.Fatal("rival claim must refuse")
	}
}

func TestReleaseStorePrefixHappyPath(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "tool"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	releaseSeqExpect(t, mock, "tool", "tool", "renamed the store", 0)
	// allowed_prefixes unclaim: the owned prefix leaves the scoped config row
	// in the same transaction.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("tool", "allowed_prefixes").
		WillReturnRows(rowsOf(t, "value", []string{"tool,aux"}))
	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO brain_unified_config (`store`,`key`,value) VALUES (?, ?, ?)")).
		WithArgs("tool", "allowed_prefixes", "aux").
		WillReturnResult(sqlmock.NewResult(1, 1))

	out, err := ReleaseStorePrefix(ctx, tx, "tool", ReleaseStorePrefixOpts{Actor: "tool", Reason: "renamed the store"})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !out.Released || out.Owner != "tool" || out.EventID == "" {
		t.Fatalf("out = %+v, want released with an event id", out)
	}
	if !out.AllowedPrefixesUpdated || out.LiveBeadCount != 0 {
		t.Fatalf("out = %+v, want allowed_prefixes updated and zero live beads", out)
	}
}

// The claim → release → re-claim transfer in one contiguous transaction
// story: the release frees the row (delete), and the re-claim by another
// store is the ordinary insert path — no special case.
func TestReleaseThenReclaimByAnotherStore(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "stories"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	releaseSeqExpect(t, mock, "proto", "stories", "re-homing", 0)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("stories", "allowed_prefixes").
		WillReturnError(sql.ErrNoRows)

	out, err := ReleaseStorePrefix(ctx, tx, "proto", ReleaseStorePrefixOpts{Actor: "stories", Reason: "re-homing"})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if !out.Released {
		t.Fatalf("out = %+v", out)
	}

	// Re-claim: the deleted row means the fresh claim inserts again, and its
	// claim event records the new owner.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("proto").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_store_prefixes (`prefix`,`store`,`owner_reason`,`declared_by`,`observed_by`,`bead_count`,`ambiguous`) VALUES (?, ?, ?, '', '', 0, 0)")).
		WithArgs("proto", "task", "operator-added").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS brain_store_prefix_events")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM issues WHERE id = ? OR id LIKE CONCAT(?, '-%')")).
		WithArgs("proto", "proto").
		WillReturnRows(rowsOf(t, "COUNT(*)", []string{"0"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO brain_store_prefix_events (id, event_type, prefix, actor, old_store, new_store, reason, bead_count, event_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)")).
		WithArgs(sqlmock.AnyArg(), "claim", "proto", "task", "", "task", "operator-added", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("task", "allowed_prefixes").
		WillReturnError(sql.ErrNoRows)

	if _, err := RecordStorePrefix(ctx, tx, "task", "proto", "operator-added"); err != nil {
		t.Fatalf("re-claim after release: %v", err)
	}
}

func TestReleaseStorePrefixNothingToRelease(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("ghost").
		WillReturnError(sql.ErrNoRows)

	_, err := ReleaseStorePrefix(ctx, tx, "ghost", ReleaseStorePrefixOpts{Actor: "task"})
	if err == nil {
		t.Fatal("release of a nonexistent claim must be an error, not a success-shaped no-op")
	}
	if !contains(err.Error(), "nothing to release") {
		t.Fatalf("err %q must say nothing to release", err.Error())
	}
}

func TestReleaseStorePrefixWrongActorRefuses(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "stories"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	// One read; no writes — the rival namespace cannot release what it does
	// not own, and there is no --store escape on the releasing side.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("proto").
		WillReturnRows(rowsOf(t, "store,owner_reason", []string{"task", "operator-added"}))

	_, err := ReleaseStorePrefix(ctx, tx, "proto", ReleaseStorePrefixOpts{Actor: "stories"})
	if err == nil {
		t.Fatal("release by a namespace that is not the owner must refuse")
	}
	for _, want := range []string{`owned by store "task"`, "stories"} {
		if !contains(err.Error(), want) {
			t.Fatalf("err %q must name the owner and the attempted actor", err.Error())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("refused release must read the row and write nothing: %v", err)
	}
}

func TestReleaseStorePrefixBuildDecidedRefuses(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "brain"})
	ctx := context.Background()
	for _, reason := range buildDecidedOwnerReasons {
		mock, db := newMock(t)
		tx := txOf(ctx, t, mock, db)
		mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
			WithArgs("brain").
			WillReturnRows(rowsOf(t, "store,owner_reason", []string{"brain", reason}))

		_, err := ReleaseStorePrefix(ctx, tx, "brain", ReleaseStorePrefixOpts{Actor: "brain"})
		if err == nil {
			t.Fatalf("reason %q: build-decided prefix must refuse", reason)
		}
		if !contains(err.Error(), "build-decided") {
			t.Fatalf("reason %q: err %q must name the refusal", reason, err.Error())
		}
	}
}

// Absolute refusal: live beads cannot be bought out with a flag — there is
// no --with-beads shape at all, and the tx writes nothing.
func TestReleaseStorePrefixLiveBeadsAbsoluteRefusal(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("robot").
		WillReturnRows(rowsOf(t, "store,owner_reason", []string{"task", "operator-added"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM issues WHERE id = ? OR id LIKE CONCAT(?, '-%')")).
		WithArgs("robot", "robot").
		WillReturnRows(rowsOf(t, "COUNT(*)", []string{"173"}))

	_, err := ReleaseStorePrefix(ctx, tx, "robot", ReleaseStorePrefixOpts{Actor: "task"})
	if err == nil {
		t.Fatal("a beads-carrying prefix must refuse release")
	}
	for _, want := range []string{"173", "ABSOLUTE refusal", "--with-beads"} {
		if !contains(err.Error(), want) {
			t.Fatalf("err %q must carry %q", err.Error(), want)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("absolute refusal must write nothing: %v", err)
	}
}

// Racing releases: the second actor's transaction reads the row inside its
// own tx, finds it gone, and refuses "nothing to release" — never a silent
// double-success.
func TestReleaseStorePrefixRaceFindsRowGone(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	releaseSeqExpect(t, mock, "proto", "task", "released", 0)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT value FROM brain_unified_config WHERE `store` = ? AND `key` = ?")).
		WithArgs("task", "allowed_prefixes").
		WillReturnError(sql.ErrNoRows)

	if _, err := ReleaseStorePrefix(ctx, tx, "proto", ReleaseStorePrefixOpts{Actor: "task"}); err != nil {
		t.Fatalf("first release: %v", err)
	}

	// The loser serialises second: the row is gone from its tx's view.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `store`, owner_reason FROM brain_store_prefixes WHERE prefix = ?")).
		WithArgs("proto").
		WillReturnError(sql.ErrNoRows)
	if _, err := ReleaseStorePrefix(ctx, tx, "proto", ReleaseStorePrefixOpts{Actor: "task"}); err == nil {
		t.Fatal("second release must refuse once the row is gone")
	}
}

// No silent path: empty opts.Actor refuses before anything else, and an
// attempted empty-actor "release" issues no queries at all.
func TestReleaseStorePrefixRefusesStorelessActor(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	if _, err := ReleaseStorePrefix(ctx, tx, "proto", ReleaseStorePrefixOpts{}); err == nil {
		t.Fatal("a release without an actor must refuse")
	}
}

func TestReleaseStorePrefixBadShapeRefusesBeforeAnyQuery(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	for _, bad := range []string{"a-b", "-lead", "has space", ""} {
		if _, err := ReleaseStorePrefix(ctx, tx, bad, ReleaseStorePrefixOpts{Actor: "task"}); err == nil {
			t.Fatalf("prefix %q: want refusal, got nil", bad)
		}
	}
}

func TestListStorePrefixEventsNewestFirst(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, event_type, prefix, actor, old_store, new_store, reason, bead_count, event_at FROM brain_store_prefix_events WHERE prefix = ? ORDER BY event_at DESC, id DESC")).
		WithArgs("proto").
		WillReturnRows(rowsOf(t,
			"id,event_type,prefix,actor,old_store,new_store,reason,bead_count,event_at",
			[]string{"ev-2", "claim", "proto", "task", "", "task", "operator-added", "0", "2026-10-07 12:00:00"},
			[]string{"ev-1", "release", "proto", "stories", "stories", "", "re-homing", "0", "2026-10-07 11:00:00"}))

	events, err := ListStorePrefixEvents(ctx, db, "proto")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].EventType != "claim" || events[1].EventType != "release" {
		t.Fatalf("order = %s then %s, want newest first", events[0].EventType, events[1].EventType)
	}
	if events[1].OldStore != "stories" || events[1].NewStore != "" || events[1].Reason != "re-homing" {
		t.Fatalf("release event shape = %+v", events[1])
	}
	if events[0].EventAt != "2026-10-07T12:00:00Z" {
		t.Fatalf("event_at = %q, want RFC3339 UTC", events[0].EventAt)
	}
}

func TestListStorePrefixEventsRefusesBadShape(t *testing.T) {
	ctx := context.Background()
	_, db := newMock(t)
	defer func() { _ = db.Close() }()

	if _, err := ListStorePrefixEvents(ctx, db, "a-b"); err == nil {
		t.Fatal("history of a shape-invalid prefix must refuse")
	}
}

// Honest boundary: on a legacy (per-store) database the ensure is a no-op —
// nothing is provisioned where no ownership record exists.
func TestEnsureStorePrefixEventsTableLegacyNoop(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: false})
	ctx := context.Background()
	_, db := newMock(t)
	defer func() { _ = db.Close() }()

	if err := EnsureStorePrefixEventsTable(ctx, db); err != nil {
		t.Fatalf("legacy ensure must be a no-op: %v", err)
	}
}

func TestAppendStorePrefixEventRefusesOversizeReason(t *testing.T) {
	pinScope(t, UnifiedScope{Unified: true, Store: "task"})
	ctx := context.Background()
	mock, db := newMock(t)
	tx := txOf(ctx, t, mock, db)
	defer func() { _ = tx.Rollback() }()

	if _, err := appendStorePrefixEvent(ctx, tx, PrefixEventRelease, "proto", "task", "task", "", strings.Repeat("x", 65), 0); err == nil {
		t.Fatal("an event reason above the row's 64-character limit must refuse")
	}
}

// A unified database that predates the event table answers history with the
// honest empty trail (the pre-adoption gap), not an error — and reading never
// creates the table.
func TestListStorePrefixEventsMissingTableIsEmptyTrail(t *testing.T) {
	ctx := context.Background()
	mock, db := newMock(t)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("FROM brain_store_prefix_events WHERE prefix = ?")).
		WithArgs("proto").
		WillReturnError(fmt.Errorf("Error 1146 (HY000): table not found: brain_store_prefix_events"))

	events, err := ListStorePrefixEvents(ctx, db, "proto")
	if err != nil {
		t.Fatalf("a missing event table is the pre-adoption gap, not an error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %d, want none", len(events))
	}
}
