package validation

import "testing"

func TestChildMayFollowParent(t *testing.T) {
	cases := []struct {
		parent, child int
		ok            bool
	}{
		{0, 0, false},
		{0, 1, true},
		{0, 4, true},
		{4, 4, true},
		{2, 1, false},
		{4, 3, false},
		{3, 4, true},
		{1, 1, false},
	}
	for _, tc := range cases {
		got := ChildMayFollowParent(tc.parent, tc.child)
		if got != tc.ok {
			t.Errorf("parent %d child %d: got %v, want %v", tc.parent, tc.child, got, tc.ok)
		}
	}
}

func TestHierarchySpecificityError(t *testing.T) {
	err := HierarchySpecificityError("hs-child", 0, "hs-parent", 0)
	want := "child hs-child priority 0 is not more specific than parent hs-parent priority 0"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestHierarchySpecificityEnabled(t *testing.T) {
	if !HierarchySpecificityEnabled("specificity") {
		t.Fatal("specificity should enable")
	}
	if !HierarchySpecificityEnabled("  specificity  ") {
		t.Fatal("trimmed specificity should enable")
	}
	for _, v := range []string{"", "Specificity", "SPECIFICITY", "off", "critical"} {
		if HierarchySpecificityEnabled(v) {
			t.Fatalf("%q should not enable", v)
		}
	}
}
