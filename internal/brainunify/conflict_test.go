package brainunify

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func conflictNamespaces() map[string]Namespace {
	return map[string]Namespace{
		"agent":   {Prefix: "agent", Owner: "robots"},
		"robots":  {Prefix: "robots", Owner: "robots"},
		"brain":   {Prefix: "brain", Owner: "brain"},
		"lifespn": {Prefix: "lifespn", Owner: "brain"},
		"my-proj": {Prefix: "my-proj", Owner: "hyphen"},
		"task":    {Prefix: "task", Owner: "task"},
	}
}

func TestMintCopyIDIsDeterministicAndFollowsTheScheme(t *testing.T) {
	ns := conflictNamespaces()
	// Reproduce the scheme from the design by hand: prefix, hyphen, first 12 hex
	// of sha256(original id, NUL, authoring store).
	sum := sha256.Sum256([]byte("agent-0bq" + "\x00" + "brain"))
	want := "brain-" + hex.EncodeToString(sum[:])[:12]
	if got := mintCopyID(ns, "brain", "agent-0bq"); got != want {
		t.Errorf("mintCopyID = %q, want %q", got, want)
	}
	if mintCopyID(ns, "brain", "agent-0bq") != mintCopyID(ns, "brain", "agent-0bq") {
		t.Error("the same inputs must mint the same id")
	}
	if mintCopyID(ns, "brain", "agent-0bq") == mintCopyID(ns, "robots", "agent-0bq") {
		t.Error("two stores' copies of one id must not share an id")
	}
	// The NUL keeps (id, store) pairs from folding together.
	if mintCopyID(ns, "b", "a-1") == mintCopyID(ns, "1b", "a-") {
		t.Error(`("a-1","b") and ("a-","1b") must not mint the same id`)
	}
	// A minted id reads as the namespace it was minted in.
	for _, store := range []string{"brain", "robots", "task", "nobody"} {
		id := mintCopyID(ns, store, "agent-0bq")
		if PrefixOf(id) != copyPrefix(ns, store, "agent") {
			t.Errorf("PrefixOf(%q) = %q, want the copy prefix", id, PrefixOf(id))
		}
	}
}

func TestCopyPrefix(t *testing.T) {
	ns := conflictNamespaces()
	cases := []struct{ store, original, want string }{
		{"robots", "agent", "agent"},    // owns the original prefix
		{"robots", "task", "robots"},    // owns another prefix, named like the store
		{"brain", "agent", "brain"},     // owns brain and lifespn; brain is named like the store
		{"task", "agent", "task"},       // owns task
		{"nobody", "agent", "agent"},    // owns nothing: keeps the original's
		{"hyphen", "agent", "agent"},    // owns only a prefix with a hyphen, which cannot be one
		{"brain", "lifespn", "lifespn"}, // owns the original prefix
	}
	for _, tc := range cases {
		if got := copyPrefix(ns, tc.store, tc.original); got != tc.want {
			t.Errorf("copyPrefix(%s, %s) = %q, want %q", tc.store, tc.original, got, tc.want)
		}
	}
}

func TestDeriveChildKeyKeepsTheShapeOfTheKey(t *testing.T) {
	old := "0f9b2c1e-3a4d-4b6f-8c7d-9e0a1b2c3d4e"
	got := deriveChildKey("events", "brain", "agent-0bq", old)
	if !looksLikeUUID(got) || got == old {
		t.Errorf("a uuid must stay a uuid and change: %q", got)
	}
	if got[14] != '5' {
		t.Errorf("derived uuid is not version 5: %q", got)
	}
	if got != deriveChildKey("events", "brain", "agent-0bq", old) {
		t.Error("the derivation must be deterministic")
	}
	// Each of the table, store, bead and old key changes the result: the same
	// uuid in two copies must not stay shared.
	for _, other := range []string{
		deriveChildKey("comments", "brain", "agent-0bq", old),
		deriveChildKey("events", "robots", "agent-0bq", old),
		deriveChildKey("events", "brain", "agent-l7p", old),
		deriveChildKey("events", "brain", "agent-0bq", "1f9b2c1e-3a4d-4b6f-8c7d-9e0a1b2c3d4e"),
	} {
		if other == got {
			t.Errorf("an input was ignored: %q", other)
		}
	}
	if short := deriveChildKey("interactions", "brain", "agent-0bq", "int-ab12"); len(short) != len("int-ab12") || strings.ContainsAny(short, "-ghijklmnopqrstuvwxyz") {
		t.Errorf("a short key stays as long as it was, in hex: %q", short)
	}
}

func TestRekeyColumn(t *testing.T) {
	cases := []struct {
		name string
		tp   TablePlan
		col  string
		need bool
		err  bool
	}{
		{"keyed by the scope column", TablePlan{Table: "child_counters", Scope: ScopeIssueChild, ScopeColumn: "parent_id", PrimaryKey: []string{"parent_id"}}, "", false, false},
		{"composite key holding the scope column", TablePlan{Table: "labels", Scope: ScopeIssueChild, ScopeColumn: "issue_id", PrimaryKey: []string{"issue_id", "label"}}, "", false, false},
		{"a key of its own", TablePlan{Table: "events", Scope: ScopeIssueChild, ScopeColumn: "issue_id", PrimaryKey: []string{"id"}}, "id", true, false},
		{"no key at all", TablePlan{Table: "log", Scope: ScopeIssueChild, ScopeColumn: "issue_id"}, "", false, false},
		{"a composite key without the scope column", TablePlan{Table: "odd", Scope: ScopeIssueChild, ScopeColumn: "issue_id", PrimaryKey: []string{"a", "b"}}, "", false, true},
		{"database state", TablePlan{Table: "config", Scope: ScopeDatabaseState, PrimaryKey: []string{"key"}}, "", false, false},
	}
	for _, tc := range cases {
		col, need, err := rekeyColumn(tc.tp)
		if col != tc.col || need != tc.need || (err != nil) != tc.err {
			t.Errorf("%s: rekeyColumn = (%q, %v, %v), want (%q, %v, err=%v)", tc.name, col, need, err, tc.col, tc.need, tc.err)
		}
	}
	if err := validateRekeying([]TablePlan{{Table: "odd", Scope: ScopeIssueChild, ScopeColumn: "issue_id", PrimaryKey: []string{"a", "b"}}}); err == nil {
		t.Error("an unkeyable table must be refused before anything is written")
	}
}

func mapperPlan() Plan {
	return Plan{Collisions: []Collision{
		{ID: "x-1", Resolution: ResolutionMergedIdentical, Winner: "alpha", Losers: []string{"beta"}},
		{ID: "x-2", Resolution: ResolutionConflict, Divergent: true, Copies: []ConflictCopy{
			{Store: "alpha", ID: "alp-aaaaaaaaaaaa"}, {Store: "beta", ID: "bet-bbbbbbbbbbbb"},
		}},
	}}
}

func TestMapRowsSkipsIdenticalLosersAndMovesConflictCopies(t *testing.T) {
	m := newRowMapper(mapperPlan())
	events := TablePlan{Table: "events", Scope: ScopeIssueChild, ScopeColumn: "issue_id", PrimaryKey: []string{"id"}}
	cols := []string{"id", "issue_id", "event_type"}
	uuid := "0f9b2c1e-3a4d-4b6f-8c7d-9e0a1b2c3d4e"
	rows := [][]any{
		{uuid, "x-1", "created"},                                   // beta's copy of an identical duplicate: skipped
		{uuid, "x-2", "created"},                                   // a conflict's copy: moved and re-keyed
		{"11111111-3a4d-4b6f-8c7d-9e0a1b2c3d4e", "y-9", "created"}, // someone else's bead: untouched
	}

	kept, skipped, err := m.mapRows(events, "beta", cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 || len(kept) != 2 {
		t.Fatalf("beta: kept %d, skipped %d, want 2 and 1", len(kept), skipped)
	}
	if kept[0][1] != "bet-bbbbbbbbbbbb" || kept[0][0] == uuid || !looksLikeUUID(kept[0][0].(string)) {
		t.Errorf("the conflict copy's row must move to the minted id with a new key: %v", kept[0])
	}
	if !reflect.DeepEqual(kept[1], rows[2]) {
		t.Errorf("an unrelated row must pass through unchanged: %v", kept[1])
	}

	// The same event uuid in alpha's copy: moved to alpha's id, and the key it
	// gets is not the key beta's copy got, so the two copies can coexist.
	keptA, skippedA, err := m.mapRows(events, "alpha", cols, rows[1:2])
	if err != nil {
		t.Fatal(err)
	}
	if skippedA != 0 || keptA[0][1] != "alp-aaaaaaaaaaaa" {
		t.Errorf("alpha: %v skipped %d", keptA, skippedA)
	}
	if keptA[0][0] == kept[0][0] {
		t.Errorf("both copies got the key %v for a row they shared", keptA[0][0])
	}

	// A store that holds no copy of the conflicted id is not touched: its rows
	// are strays, and stay where they are.
	keptC, skippedC, err := m.mapRows(events, "gamma", cols, rows[:2])
	if err != nil || skippedC != 0 || !reflect.DeepEqual(keptC, rows[:2]) {
		t.Errorf("gamma holds no copy: kept %v skipped %d err %v", keptC, skippedC, err)
	}
}

func TestMapRowsKeyedByTheScopeColumnJustMoves(t *testing.T) {
	m := newRowMapper(mapperPlan())
	labels := TablePlan{Table: "labels", Scope: ScopeIssueChild, ScopeColumn: "issue_id", PrimaryKey: []string{"issue_id", "label"}}
	kept, _, err := m.mapRows(labels, "alpha", []string{"issue_id", "label"}, [][]any{{"x-2", "bug"}})
	if err != nil || !reflect.DeepEqual(kept, [][]any{{"alp-aaaaaaaaaaaa", "bug"}}) {
		t.Errorf("kept %v err %v", kept, err)
	}
	issues := TablePlan{Table: "issues", Scope: ScopeIssues, ScopeColumn: "id", PrimaryKey: []string{"id"}}
	kept, _, err = m.mapRows(issues, "beta", []string{"id", "title"}, [][]any{{"x-2", "t"}})
	if err != nil || !reflect.DeepEqual(kept, [][]any{{"bet-bbbbbbbbbbbb", "t"}}) {
		t.Errorf("kept %v err %v", kept, err)
	}
	if _, _, err := m.mapRows(labels, "alpha", []string{"label"}, [][]any{{"bug"}}); err == nil {
		t.Error("a batch read without the scope column cannot be mapped")
	}
}

func TestMapRowsRekeysAnInteractionsParent(t *testing.T) {
	m := newRowMapper(mapperPlan())
	inter := TablePlan{Table: "interactions", Scope: ScopeIssueChild, ScopeColumn: "issue_id", PrimaryKey: []string{"id"}}
	cols := []string{"id", "issue_id", "parent_id"}
	kept, _, err := m.mapRows(inter, "alpha", cols, [][]any{
		{"int-aaaa1111", "x-2", nil},
		{"int-bbbb2222", "x-2", "int-aaaa1111"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept[0][2] != nil || kept[1][2] != kept[0][0] {
		t.Errorf("a child interaction must point at its parent's new key: %v", kept)
	}
}

func TestConflictRowsAreWhatTheTitleAndDescriptionSay(t *testing.T) {
	c := Collision{
		ID: "agent-0bq", Divergent: true, Resolution: ResolutionConflict,
		DifferingColumns: []string{"status", "updated_at"}, DataColumnsDiffer: true,
		Copies: []ConflictCopy{
			{Store: "brain", ID: "brain-aaaaaaaaaaaa", Row: map[string]string{"status": "open", "title": "Guard", "created_at": "2026-06-14 17:01:24", "updated_at": "2026-06-14 17:01:24"}},
			{Store: "robots", ID: "agent-bbbbbbbbbbbb", Row: map[string]string{"status": "closed", "title": "Guard", "created_at": "2026-06-14 17:01:24", "updated_at": "2026-07-29 16:29:46"}},
		},
	}
	rows := conflictRows(c)
	issue := rows["issues"][0]
	if issue["id"] != "agent-0bq" || issue["status"] != "open" {
		t.Errorf("the conflict bead is the original id, open: %v", issue)
	}
	if issue["created_at"] != "2026-06-14 17:01:24" || issue["updated_at"] != "2026-07-29 16:29:46" {
		t.Errorf("a conflict bead is as old as its oldest copy and as new as its newest: %v", issue)
	}
	title, _ := issue["title"].(string)
	desc, _ := issue["description"].(string)
	if !strings.Contains(strings.ToLower(title), "conflict") || !strings.Contains(title, "agent-0bq") {
		t.Errorf("the title must say what this is: %q", title)
	}
	for _, want := range []string{"brain-aaaaaaaaaaaa", "agent-bbbbbbbbbbbb", "store brain", "store robots", "unresolved", "status, updated_at", "tracks"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description lacks %q:\n%s", want, desc)
		}
	}

	deps := rows["dependencies"]
	if len(deps) != 2 {
		t.Fatalf("one dependency per copy, got %d", len(deps))
	}
	for i, cp := range c.Copies {
		d := deps[i]
		if d["issue_id"] != "agent-0bq" || d["depends_on_issue_id"] != cp.ID || d["type"] != "tracks" {
			t.Errorf("dependency %d = %v", i, d)
		}
		if !looksLikeUUID(d["id"].(string)) {
			t.Errorf("a dependency id is a uuid: %v", d["id"])
		}
	}
	if deps[0]["id"] == deps[1]["id"] {
		t.Error("the dependencies need distinct ids")
	}
	if !reflect.DeepEqual(rows["labels"], []map[string]any{{"issue_id": "agent-0bq", "label": ConflictLabel}}) {
		t.Errorf("labels = %v", rows["labels"])
	}
	if !reflect.DeepEqual(conflictRows(c), rows) {
		t.Error("the rows of a conflict must be a function of the conflict: a rebuild writes the same rows")
	}
}

func TestTableConflictRowsUseTheTablesColumnOrder(t *testing.T) {
	c := Collision{ID: "x-1", Divergent: true, Copies: []ConflictCopy{{Store: "a", ID: "a-1"}, {Store: "b", ID: "b-1"}}}
	deps := TablePlan{Table: "dependencies", Target: "dependencies", Columns: []string{"id", "issue_id", "type", "created_at", "created_by", "metadata", "thread_id", "depends_on_issue_id"}}
	cols, rows := tableConflictRows(deps, []Collision{c})
	if !reflect.DeepEqual(cols, []string{"id", "issue_id", "type", "created_at", "created_by", "thread_id", "depends_on_issue_id"}) {
		t.Errorf("cols = %v (metadata is not provided, so the table defaults it)", cols)
	}
	if len(rows) != 2 || len(rows[0]) != len(cols) {
		t.Errorf("rows = %v", rows)
	}
	if cols, rows := tableConflictRows(TablePlan{Table: "events", Columns: []string{"id"}}, []Collision{c}); cols != nil || rows != nil {
		t.Error("the tool authors nothing in events")
	}
}

func TestValidateConflictTables(t *testing.T) {
	good := []TablePlan{
		{Table: "issues", Columns: conflictColumns["issues"]},
		{Table: "dependencies", Columns: conflictColumns["dependencies"]},
		{Table: "labels", Columns: conflictColumns["labels"]},
	}
	if err := validateConflictTables(good); err != nil {
		t.Errorf("a schema with the columns is fine: %v", err)
	}
	if err := validateConflictTables(good[:2]); err == nil || !strings.Contains(err.Error(), "labels") {
		t.Errorf("a schema without labels is refused, naming it: %v", err)
	}
	bad := append([]TablePlan(nil), good...)
	bad[1] = TablePlan{Table: "dependencies", Columns: []string{"id", "issue_id", "type", "created_at", "created_by"}}
	if err := validateConflictTables(bad); err == nil || !strings.Contains(err.Error(), "depends_on_issue_id") {
		t.Errorf("a missing column is named: %v", err)
	}
}

func TestPlanRefusals(t *testing.T) {
	a := Collision{ID: "x-1", Copies: []ConflictCopy{
		{Store: "alpha", ID: "alp-aaaaaaaaaaaa", Row: map[string]string{"slug": "same"}},
		{Store: "beta", ID: "bet-bbbbbbbbbbbb", Row: map[string]string{"slug": "same"}},
	}}
	if got := planRefusals([]Collision{a}); len(got) != 1 || !strings.Contains(got[0], "slug") {
		t.Errorf("two copies sharing a slug cannot both be inserted: %v", got)
	}
	b := Collision{ID: "x-2", Copies: []ConflictCopy{{Store: "alpha", ID: "alp-aaaaaaaaaaaa"}}}
	if got := planRefusals([]Collision{{ID: "x-1", Copies: []ConflictCopy{{Store: "alpha", ID: "alp-aaaaaaaaaaaa"}}}, b}); len(got) != 1 || !strings.Contains(got[0], "alp-aaaaaaaaaaaa") {
		t.Errorf("two derivations of one minted id are refused: %v", got)
	}
	a.Copies[1].Row["slug"] = "other"
	if got := planRefusals([]Collision{a}); len(got) != 0 {
		t.Errorf("distinct slugs are fine: %v", got)
	}
	if got := mintedClashes([]Collision{a}, map[string]bool{"bet-bbbbbbbbbbbb": true}); len(got) != 1 || !strings.Contains(got[0], "already") {
		t.Errorf("a minted id that an existing bead has is refused: %v", got)
	}
	p := Plan{Refusals: []string{"one", "two"}}
	if err := p.Refusal(); err == nil || err.Error() != "one; two" {
		t.Errorf("Refusal = %v", err)
	}
}

func TestExclusionsMatchOnBothSides(t *testing.T) {
	p := mapperPlan()
	if got := p.sourceExclusions("beta"); !reflect.DeepEqual(got, []string{"x-1", "x-2"}) {
		t.Errorf("beta skips its identical copy and leaves the conflicted id to the per-id check: %v", got)
	}
	if got := p.sourceExclusions("alpha"); !reflect.DeepEqual(got, []string{"x-2"}) {
		t.Errorf("the winner of an identical duplicate keeps its rows: %v", got)
	}
	if got := p.mergedExclusions(); !reflect.DeepEqual(got, []string{"alp-aaaaaaaaaaaa", "bet-bbbbbbbbbbbb", "x-2"}) {
		t.Errorf("the merged side leaves out the conflict bead and its copies: %v", got)
	}
}

func TestDigestCopyIsOrderIndependentAndSeesEveryCell(t *testing.T) {
	a := map[string][][]any{"issues": {{"i-1", "t", nil}, {"i-2", "u", "x"}}, "labels": {{"i-1", "bug"}}}
	b := map[string][][]any{"labels": {{"i-1", "bug"}}, "issues": {{"i-2", "u", "x"}, {"i-1", "t", nil}}}
	if digestCopy(a) != digestCopy(b) {
		t.Error("row and table order must not matter")
	}
	c := map[string][][]any{"issues": {{"i-1", "t", ""}, {"i-2", "u", "x"}}, "labels": {{"i-1", "bug"}}}
	if digestCopy(a) == digestCopy(c) {
		t.Error("NULL and an empty string must digest differently")
	}
	d := map[string][][]any{"issues": {{"i-1", "t", nil}, {"i-2", "u", "x"}}, "labels": {{"i-1", "bug "}}}
	if digestCopy(a) == digestCopy(d) {
		t.Error("a changed cell must change the digest")
	}
	if got := differingTables(a, d); !reflect.DeepEqual(got, []string{"labels"}) {
		t.Errorf("differingTables = %v", got)
	}
}
