package brainunify

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIsReadOnly(t *testing.T) {
	allowed := []string{
		"select * from issues",
		"  SHOW DATABASES",
		"describe `dolt`.`issues`",
		"with x as (select 1) select * from x",
	}
	for _, stmt := range allowed {
		if !isReadOnly(stmt) {
			t.Errorf("isReadOnly(%q) = false, want true", stmt)
		}
	}
	// Every one of these would modify a live store if it ever reached the
	// production connection, so the guard must refuse all of them.
	refused := []string{
		"",
		"insert into issues values (1)",
		"update issues set status='closed'",
		"delete from issues",
		"drop database brain",
		"create table t (id int)",
		"call dolt_add('.')",
		"set foreign_key_checks = 0",
		"/* comment */ drop table issues",
	}
	for _, stmt := range refused {
		if isReadOnly(stmt) {
			t.Errorf("isReadOnly(%q) = true, want false", stmt)
		}
	}
}

func TestReadOnlySourceRefusesWrites(t *testing.T) {
	src := &readOnlySource{}
	if _, err := src.query(t.Context(), "delete from issues"); err == nil {
		t.Fatal("expected the source connection to refuse a DELETE")
	} else if !strings.Contains(err.Error(), "refusing to issue a non-SELECT") {
		t.Errorf("unexpected error: %v", err)
	}
	if src.queryRow(t.Context(), "drop table issues") != nil {
		t.Error("queryRow must return nil rather than run a statement it refuses")
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"it's", `'it''s'`},
		{`back\slash`, `'back\\slash'`},
		{"line\nbreak", `'line\nbreak'`},
		{"", "''"},
		{"nul\x00byte", `'nul\0byte'`},
	}
	for _, tc := range cases {
		if got := quoteLiteral(tc.in); got != tc.want {
			t.Errorf("quoteLiteral(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestValueTupleHandlesNilAndTypes(t *testing.T) {
	got := valueTuple([]any{"a", nil, int64(3), []byte("z")})
	if got != "('a',NULL,3,'z')" {
		t.Errorf("valueTuple = %s", got)
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike("a_b%c"); got != `a\_b\%c` {
		t.Errorf("escapeLike = %q", got)
	}
}

func TestCombineIsDisjointAddition(t *testing.T) {
	a := Fingerprint{Rows: 2, Bytes: 100, Hash: 0b1010}
	b := Fingerprint{Rows: 3, Bytes: 200, Hash: 0b0110}
	got := Combine(a, b)
	if got.Rows != 5 || got.Bytes != 300 {
		t.Errorf("counts = %+v, want 5 rows / 300 bytes", got)
	}
	if got.Hash != a.Hash^b.Hash {
		t.Errorf("hash = %d, want the XOR %d", got.Hash, a.Hash^b.Hash)
	}
}

// writeStore lays down a fake store directory so registry resolution can be
// tested against real files rather than a mock filesystem.
func writeStore(t *testing.T, home, dir, database, projectID, issuePrefix, bdName string) {
	t.Helper()
	beads := filepath.Join(home, dir, ".beads")
	if err := os.MkdirAll(beads, 0o750); err != nil {
		t.Fatalf("creating %s: %v", beads, err)
	}
	md := `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":3307,"dolt_database":"` + database + `","project_id":"` + projectID + `"}`
	if err := os.WriteFile(filepath.Join(beads, "metadata.json"), []byte(md), 0o600); err != nil {
		t.Fatalf("writing metadata.json: %v", err)
	}
	cfg := "issue-prefix: \"" + issuePrefix + "\"\nBD_NAME: \"" + bdName + "\"\n"
	if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config.yaml: %v", err)
	}
}

func TestLoadRegistryResolvesDatabaseNotStoreName(t *testing.T) {
	home := t.TempDir()
	writeStore(t, home, "decisions", "decision", "p1", "decide", "decide")
	writeStore(t, home, "brain", "dolt", "p2", "brain", "brain")

	cfgDir := filepath.Join(home, ".config", "brain")
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// decisions is registered with a path that omits the .beads suffix, the
	// legacy form that is still in the file on disk.
	reg := "stores:\n    decisions:\n        path: " + filepath.Join(home, "decisions") + "\n    brain:\n        path: " + filepath.Join(home, "brain", ".beads") + "\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "stores.yaml"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadRegistry(home)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	byName := map[string]Store{}
	for _, s := range got.Stores {
		byName[s.Namespace] = s
	}
	dec, ok := byName["decisions"]
	if !ok {
		t.Fatalf("decisions store missing; got %v", got.Stores)
	}
	// The store is called "decisions" and its database is "decision"; conflating
	// the two is exactly the mistake that would migrate the wrong rows.
	if dec.Database != "decision" {
		t.Errorf("decisions database = %q, want decision", dec.Database)
	}
	if dec.ProjectID != "p1" {
		t.Errorf("decisions project id = %q, want p1", dec.ProjectID)
	}
	if len(dec.DeclaredPrefixes) != 1 || dec.DeclaredPrefixes[0] != "decide" {
		t.Errorf("decisions declared prefixes = %v, want [decide]", dec.DeclaredPrefixes)
	}
	if br := byName["brain"]; br.Database != "dolt" {
		t.Errorf("brain database = %q, want dolt", br.Database)
	}
	if !dec.ServerMode {
		t.Error("a server-mode store must be marked as such")
	}
}

func TestLoadRegistryReadsLegacyPath(t *testing.T) {
	home := t.TempDir()
	writeStore(t, home, "inbox", "inbox", "p3", "inbox", "inbox")
	cfgDir := filepath.Join(home, ".config", "pai")
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	reg := "stores:\n    inbox: " + filepath.Join(home, "inbox", ".beads") + "\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "stores.yaml"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRegistry(home)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(got.Stores) != 1 || got.Stores[0].Namespace != "inbox" {
		t.Fatalf("legacy registry not read: %+v", got.Stores)
	}
}

func TestLoadRegistryMissingIsAnError(t *testing.T) {
	if _, err := LoadRegistry(t.TempDir()); err == nil {
		t.Fatal("expected an error when no registry exists")
	}
}

func TestReadDeclaredPrefixesKeepsBoth(t *testing.T) {
	// The robots store declares issue-prefix "robots" while its BD_NAME — and
	// its older ids — are "agent". Both namespaces are legitimately its own.
	home := t.TempDir()
	writeStore(t, home, "robots", "agent", "p4", "robots", "agent")
	got := readDeclaredPrefixes(filepath.Join(home, "robots", ".beads"))
	if len(got) != 2 || got[0] != "robots" || got[1] != "agent" {
		t.Errorf("declared prefixes = %v, want [robots agent]", got)
	}
}

func TestFingerprintInvariantRejectsRowsWithNoContent(t *testing.T) {
	// A fingerprint that reports rows but zero bytes means the aggregate
	// columns were never read. That must be an error, not a passing
	// comparison: it is how a real defect hides inside a green result.
	fp := Fingerprint{Rows: 10}
	if err := fp.checkInvariant("applications", "config"); err == nil {
		t.Fatal("expected an error for a fingerprint with rows but no content bytes")
	}
	if err := (Fingerprint{}).checkInvariant("applications", "config"); err != nil {
		t.Fatalf("an empty table must be a valid fingerprint: %v", err)
	}
}

// TestSingleEncodingIsUsedOnBothSides documents the invariant that replaced
// three verifier defects: there is exactly one column encoding in the
// migration, the SQL in fingerprintFields, and the copy path computes no
// digest of its own. A client-side digest is what drifted from the server by a
// few bytes per row and produced failures indistinguishable from corruption.
func TestSingleEncodingIsUsedOnBothSides(t *testing.T) {
	src := reflect.TypeOf(&readOnlySource{})
	if _, ok := src.MethodByName("Digest"); ok {
		t.Error("readOnlySource must not expose a client-side digest; the server expression is the only encoding")
	}
	if _, ok := src.MethodByName("CopyRows"); !ok {
		t.Fatal("CopyRows must remain the row-copying path")
	}
}

// TestReKeyedFingerprintDigestsTheUnifiedColumnSet is the regression lock for
// verifier defect A: 277 of the 279 failures of the completed verification run
// compared a source-side fingerprint with no `store` column against a re-keyed
// unified table that has one, so no data could ever have produced a match.
//
// The invariant is not "the two sides produce the same number" — that would
// need a database and real data. It is the precondition the number depends on:
// the two sides digest the same column set, in the same order, with the same
// rendering, and differ only in where the store's value comes from.
func TestReKeyedFingerprintDigestsTheUnifiedColumnSet(t *testing.T) {
	// brain_unified_config as NamespacedDDL builds it: the store column first,
	// then the source table's own columns.
	unifiedTypes := map[string]string{"store": "varchar(128)", "key": "varchar", "value": "json"}
	unified := fingerprintFieldsForTypes(unifiedTypes, []string{"store", "key", "value"})

	// The source side is built by referenceFields, the function the build's
	// reference digest calls, so this fails if a re-keyed plan is ever digested
	// over the source's own columns alone.
	reKeyed := &TablePlan{Table: "config", Target: "brain_unified_config", Scope: ScopeDatabaseState, Columns: []string{"key", "value"}}
	source := referenceFields(reKeyed, unifiedTypes, "task")

	if len(source) != len(unified) {
		t.Fatalf("source side digests %d field(s) and the unified side %d; a comparison between different "+
			"column sets can never pass, for any store: source %v unified %v", len(source), len(unified), source, unified)
	}
	// Same rendering, same order. The only legitimate difference is the store
	// value's provenance: a literal on the source side, the column on the
	// unified side. If the store's bytes were rendered any other way the two
	// digests would differ even on identical rows.
	if got := strings.Replace(source[0], "lower(hex('task'))", "lower(hex(`store`))", 1); got != unified[0] {
		t.Errorf("source store field %q\nrenders the store differently from the unified field %q\n(after substituting the value's source: %q)",
			source[0], unified[0], got)
	}
	for i := range unified {
		source[i] = strings.Replace(source[i], "lower(hex('task'))", "lower(hex(`store`))", 1)
	}
	if !reflect.DeepEqual(source, unified) {
		t.Errorf("the two sides must digest the same column set in the same order:\nsource   %v\nunified  %v", source, unified)
	}

	// The same defect in the other direction: a table that folds into the
	// unified database unchanged must not be prefixed, or its two sides would
	// disagree instead. This is the shape issues, dependencies and the child
	// tables have.
	for _, scope := range []Scope{ScopeIssues, ScopeIssueChild} {
		table := &TablePlan{Table: "issues", Target: "issues", Scope: scope, Columns: []string{"id", "title"}}
		got := referenceFields(table, map[string]string{"id": "varchar", "title": "varchar"}, "task")
		if want := []string{digestFieldExpr("lower(hex(`id`))"), digestFieldExpr("lower(hex(`title`))")}; !reflect.DeepEqual(got, want) {
			t.Errorf("scope %v must keep the source column set: got %v want %v", scope, got, want)
		}
	}
}

// TestStoreColumnByteOverheadMatchesTheRecordedRepro locks the hand-computed
// reproduction that identified verifier defect A, so the 100-byte difference it
// rests on is arithmetic in the test suite rather than a number in a report.
//
// Recorded: source tasks.config 10 rows / 476 bytes / digest 307648475 against
// unified brain_unified_config for store `task` 10 rows / 576 bytes /
// 1552887092. The delta is the added store value rendered into each row: the
// \x02 present-value marker, the lower-cased hex of "task" (2 characters per
// byte, so 8), and the \x1f field separator.
func TestStoreColumnByteOverheadMatchesTheRecordedRepro(t *testing.T) {
	const (
		rows         = 10
		store        = "task"
		sourceBytes  = 476
		unifiedBytes = 576
	)
	perRow := len("\x02") + 2*len(store) + len("\x1f")
	if perRow != 10 {
		t.Fatalf("store field overhead = %d bytes per row, want 10", perRow)
	}
	if got := sourceBytes + rows*perRow; got != unifiedBytes {
		t.Errorf("%d source bytes + %d rows x %d bytes = %d, want the recorded unified %d",
			sourceBytes, rows, perRow, got, unifiedBytes)
	}
}

// TestCopyRowsReadsJSONThroughTheDigestExpression is the regression lock for
// defect B, the loss of 2289 bytes of metadata that a completed build reported
// as source_rows == imported_rows with no collision on record.
//
// A json column read as itself goes through the server's native json wire path,
// which on the federation's Dolt server returns a zero-length value for two of
// tasks.issues' rows whenever the result set holds more than one row; the copy
// then records that as {}. The same column read through cast(`c` as char) — the
// expression the fingerprints already digest it with — returns the whole
// document. Reading it that way is what keeps the digested bytes and the copied
// bytes the same bytes.
func TestCopyRowsReadsJSONThroughTheDigestExpression(t *testing.T) {
	got := copySelectList([]string{"id", "metadata", "title"}, []string{"metadata"})
	want := "`id`, cast(`metadata` as char), `title`"
	if got != want {
		t.Errorf("copySelectList = %q, want %q", got, want)
	}
	if strings.Contains(got, "`metadata`,") || strings.HasSuffix(got, "`metadata`") {
		t.Error("a json column must never be selected as itself: that is the path that loses the value")
	}
	// A table with no json columns is copied verbatim.
	if got := copySelectList([]string{"key", "value"}, nil); got != "`key`, `value`" {
		t.Errorf("copySelectList without json columns = %q, want %q", got, "`key`, `value`")
	}
	// The store column of a database-state copy is not json and stays itself.
	if got := copySelectList([]string{"store", "key", "value"}, []string{"metadata"}); got != "`store`, `key`, `value`" {
		t.Errorf("copySelectList = %q, want %q", got, "`store`, `key`, `value`")
	}
}

// TestIssueRowReadsJSONLikeTheCopy does the same for the collision comparison.
// Each duplicated id is read with one query per database, so this shape is not
// the one that loses the value today; it is read the same way anyway because
// the comparison decides whether a collision blocks the cutover, and a value
// that arrives empty on both sides reads as two copies agreeing.
func TestIssueRowReadsJSONLikeTheCopy(t *testing.T) {
	got := issueRowSelect("tasks", []string{"id", "metadata", "title"}, []string{"metadata"})
	want := "select `id`, cast(`metadata` as char), `title` from `tasks`.`issues` where `id` = ?"
	if got != want {
		t.Errorf("issueRowSelect = %q, want %q", got, want)
	}
}
