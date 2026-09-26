package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// Derivation rule: DisplayName is a deterministic kebab-case projection of
// the title (lowercase, non-alphanumeric runs collapse to one hyphen,
// trimmed, capped at 64 chars on a word boundary).
func TestDisplayName_Deterministic(t *testing.T) {
	cases := map[string]string{
		"Use kebab-case bead names as returned IDs": "use-kebab-case-bead-names-as-returned-ids",
		"  Trim  spaces & punctuation!! ":           "trim-spaces-punctuation",
		"already-kebab":                             "already-kebab",
		"MixedCASE Title 123":                       "mixedcase-title-123",
		"a/b\\c:d.e_f":                              "a-b-c-d-e-f",
		"":                                          "",
		"---":                                       "",
		"!!!":                                       "",
	}
	for title, want := range cases {
		if got := DisplayName(title); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", title, got, want)
		}
		// Determinism: same input twice yields the same output.
		if again := DisplayName(title); again != DisplayName(title) {
			t.Errorf("DisplayName(%q) not deterministic: %q vs %q", title, again, DisplayName(title))
		}
	}
}

func TestDisplayName_TruncatesOnWordBoundary(t *testing.T) {
	long := strings.Repeat("word-", 20) + "tail"
	got := DisplayName(long)
	if len(got) > 64 {
		t.Errorf("DisplayName length %d exceeds 64: %q", len(got), got)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("DisplayName ends mid-word with hyphen: %q", got)
	}
	if !strings.HasPrefix(long, got) && !strings.HasPrefix(strings.ToLower(long), got) {
		t.Errorf("DisplayName %q is not a prefix of the kebabbed title", got)
	}
}

// Collision behaviour: two beads whose titles kebab to the same string
// share a display name. The name MUST NOT be treated as an identity —
// the canonical id stays the disambiguator.
func TestDisplayName_CollisionsShareName(t *testing.T) {
	a := DisplayName("Fix login bug")
	b := DisplayName("Fix  login   bug!!")
	if a == "" || a != b {
		t.Fatalf("expected colliding titles to share a name, got %q vs %q", a, b)
	}
	ia := &Issue{ID: "task-aaaa", Title: "Fix login bug"}
	ib := &Issue{ID: "task-bbbb", Title: "Fix  login   bug!!"}
	ia.EnsureName()
	ib.EnsureName()
	if ia.Name != ib.Name {
		t.Fatalf("expected shared name, got %q vs %q", ia.Name, ib.Name)
	}
	if ia.ID == ib.ID {
		t.Fatal("collision test needs distinct ids")
	}
}

// EnsureName never overwrites an explicitly set name and derives from the
// title otherwise.
func TestEnsureName_PreservesExplicit(t *testing.T) {
	i := &Issue{ID: "task-1", Title: "Some title", Name: "custom-name"}
	i.EnsureName()
	if i.Name != "custom-name" {
		t.Errorf("EnsureName overwrote explicit name: %q", i.Name)
	}
	j := &Issue{ID: "task-2", Title: "Some title"}
	j.EnsureName()
	if j.Name != "some-title" {
		t.Errorf("EnsureName did not derive: %q", j.Name)
	}
}

// Additive-shape contract: machine-readable output keeps the canonical id
// byte-for-byte and gains `name` alongside it. Existing id consumers see
// no change apart from the extra field.
func TestIssueJSON_AdditiveNameField(t *testing.T) {
	i := &Issue{ID: "task-5wxul", Title: "Use kebab-case bead names as returned IDs", Status: StatusOpen, Priority: 2}
	i.EnsureName()
	raw, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["id"] != "task-5wxul" {
		t.Errorf("canonical id changed: %v", m["id"])
	}
	if m["name"] != "use-kebab-case-bead-names-as-returned-ids" {
		t.Errorf("name missing or wrong: %v", m["name"])
	}
}

// Underivable titles omit the field entirely (omitempty), so old and new
// shapes match for nameless beads.
func TestIssueJSON_NameOmittedWhenUnderivable(t *testing.T) {
	i := &Issue{ID: "task-9", Title: "!!!"}
	i.EnsureName()
	raw, _ := json.Marshal(i)
	if strings.Contains(string(raw), `"name"`) {
		t.Errorf("nameless bead should omit name field: %s", raw)
	}
	if !strings.Contains(string(raw), `"id":"task-9"`) {
		t.Errorf("id must always be present: %s", raw)
	}
}
