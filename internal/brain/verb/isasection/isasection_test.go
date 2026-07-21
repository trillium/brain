package isasection

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSectionName_AcceptsAllTwelveCanonicalNames(t *testing.T) {
	canonical := []string{
		"problem", "vision", "out_of_scope", "principles", "constraints",
		"goal", "criteria", "test_strategy", "features", "decisions",
		"changelog", "verification",
	}
	if len(canonical) != 12 {
		t.Fatalf("expected 12 canonical names in test fixture, got %d", len(canonical))
	}
	for _, name := range canonical {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSectionName(name); err != nil {
				t.Errorf("expected %q to be valid, got error: %v", name, err)
			}
			if !IsValidSection(name) {
				t.Errorf("IsValidSection(%q) = false, want true", name)
			}
		})
	}
}

func TestValidateSectionName_RejectsUnknown(t *testing.T) {
	err := ValidateSectionName("not_a_section")
	if err == nil {
		t.Fatal("expected error for unknown section, got nil")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if !strings.Contains(err.Error(), "not_a_section") {
		t.Errorf("expected error message to mention rejected name, got: %s", err.Error())
	}
	// Sanity: error should enumerate all twelve valid names so users can fix.
	for _, name := range SortedSectionNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("expected error message to list valid name %q, got: %s", name, err.Error())
		}
	}
}

func TestValidateSectionName_RejectsEmpty(t *testing.T) {
	err := ValidateSectionName("")
	if err == nil {
		t.Fatal("expected error for empty section name, got nil")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "required") {
		t.Errorf("expected empty-name error to mention 'required', got: %s", err.Error())
	}
}

func TestValidateSectionName_IsCaseSensitive(t *testing.T) {
	// ISA sections are a fixed lower_snake_case alphabet. Uppercase or mixed
	// case must be rejected — silently lowercasing would mask user typos.
	cases := []string{"Problem", "PROBLEM", "Out_Of_Scope", "Test_Strategy"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSectionName(name); err == nil {
				t.Errorf("expected %q (wrong case) to be rejected, got nil error", name)
			}
		})
	}
}

func TestSortedSectionNames_ReturnsTwelveStable(t *testing.T) {
	first := SortedSectionNames()
	if len(first) != 12 {
		t.Fatalf("expected 12 names, got %d", len(first))
	}
	// Stable across calls — sort.Strings is deterministic but assert anyway.
	second := SortedSectionNames()
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("SortedSectionNames not stable at index %d: %q vs %q", i, first[i], second[i])
		}
	}
	// Alphabetical sanity: changelog < verification.
	if first[0] != "changelog" {
		t.Errorf("expected first sorted name to be 'changelog', got %q", first[0])
	}
	if first[len(first)-1] != "vision" {
		t.Errorf("expected last sorted name to be 'vision', got %q", first[len(first)-1])
	}
}

func TestSectionConstantsMatchMapKeys(t *testing.T) {
	// Guard against drift between the constants and the ValidSections map.
	for _, c := range []string{
		SectionProblem, SectionVision, SectionOutOfScope, SectionPrinciples,
		SectionConstraints, SectionGoal, SectionCriteria, SectionTestStrategy,
		SectionFeatures, SectionDecisions, SectionChangelog, SectionVerification,
	} {
		if !IsValidSection(c) {
			t.Errorf("constant %q is not in ValidSections map", c)
		}
	}
}
