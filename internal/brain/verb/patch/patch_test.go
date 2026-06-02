package patch

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateEnum table-drives the three enum fields and their failure modes.
// Each case asserts the validation outcome and, on failure, that the surfaced
// error mentions the offending value and lists the valid set so users get
// actionable feedback.
func TestValidateEnum(t *testing.T) {
	cases := []struct {
		name      string
		field     string
		value     string
		wantErr   bool
		wantInMsg []string // substrings the error message must contain
	}{
		{name: "phase_valid_BUILD", field: FieldISAPhase, value: "BUILD", wantErr: false},
		{name: "phase_valid_OBSERVE", field: FieldISAPhase, value: "OBSERVE", wantErr: false},
		{name: "phase_invalid_nonsense", field: FieldISAPhase, value: "NONSENSE", wantErr: true, wantInMsg: []string{"NONSENSE", "BUILD", "OBSERVE"}},
		{name: "phase_invalid_lowercase", field: FieldISAPhase, value: "build", wantErr: true, wantInMsg: []string{"build", "BUILD"}},
		{name: "phase_invalid_empty", field: FieldISAPhase, value: "", wantErr: true, wantInMsg: []string{FieldISAPhase}},

		{name: "effort_valid_E3", field: FieldISAEffort, value: "E3", wantErr: false},
		{name: "effort_valid_E5", field: FieldISAEffort, value: "E5", wantErr: false},
		{name: "effort_invalid_E0", field: FieldISAEffort, value: "E0", wantErr: true, wantInMsg: []string{"E0", "E1", "E5"}},
		{name: "effort_invalid_E6", field: FieldISAEffort, value: "E6", wantErr: true, wantInMsg: []string{"E6"}},

		{name: "mode_valid_NATIVE", field: FieldISAMode, value: "NATIVE", wantErr: false},
		{name: "mode_valid_ALGORITHM", field: FieldISAMode, value: "ALGORITHM", wantErr: false},
		{name: "mode_invalid_FOO", field: FieldISAMode, value: "FOO", wantErr: true, wantInMsg: []string{"FOO", "ALGORITHM", "NATIVE", "MINIMAL"}},

		{name: "non_enum_field_rejected", field: FieldISAProgressM, value: "0", wantErr: true, wantInMsg: []string{"not an enum field"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEnum(tc.field, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				for _, want := range tc.wantInMsg {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q missing substring %q", err.Error(), want)
					}
				}
				var verr *ValidationError
				if !errors.As(err, &verr) {
					t.Errorf("expected *ValidationError, got %T", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateProgressM covers parsing failure, negative input, and the
// m ≤ n invariant.
func TestValidateProgressM(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		currentN int
		want     int
		wantErr  bool
	}{
		{name: "valid_zero", value: "0", currentN: 10, want: 0, wantErr: false},
		{name: "valid_equal", value: "10", currentN: 10, want: 10, wantErr: false},
		{name: "valid_under", value: "7", currentN: 10, want: 7, wantErr: false},
		{name: "invalid_over", value: "999", currentN: 10, wantErr: true},
		{name: "invalid_negative", value: "-1", currentN: 10, wantErr: true},
		{name: "invalid_nonnumeric", value: "abc", currentN: 10, wantErr: true},
		{name: "invalid_empty", value: "", currentN: 10, wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateProgressM(tc.value, tc.currentN)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%d)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestValidateProgressN covers parsing failure, negative input, and the
// n ≥ currentM invariant (lowering N below current M is forbidden).
func TestValidateProgressN(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		currentM int
		want     int
		wantErr  bool
	}{
		{name: "valid_grow", value: "20", currentM: 5, want: 20, wantErr: false},
		{name: "valid_equal", value: "5", currentM: 5, want: 5, wantErr: false},
		{name: "invalid_shrink_below_m", value: "3", currentM: 5, wantErr: true},
		{name: "invalid_negative", value: "-1", currentM: 0, wantErr: true},
		{name: "invalid_nonnumeric", value: "x", currentM: 0, wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateProgressN(tc.value, tc.currentM)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%d)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestIsAllowedField verifies the allowlist contract.
func TestIsAllowedField(t *testing.T) {
	allowed := []string{
		FieldISAPhase, FieldISAProgressM, FieldISAProgressN,
		FieldISAEffort, FieldISAMode, FieldISAStartedAt, FieldISAUpdatedAt,
		FieldSlug,
	}
	for _, f := range allowed {
		if !IsAllowedField(f) {
			t.Errorf("expected %s to be allowed", f)
		}
	}
	for _, f := range []string{"status", "priority", "title", "description", "", "ISA_PHASE"} {
		if IsAllowedField(f) {
			t.Errorf("did not expect %q to be allowed", f)
		}
	}
}

// TestIsISAField verifies slug is NOT an ISA field (it is patchable on any
// kind and does not bump isa_updated_at).
func TestIsISAField(t *testing.T) {
	if !IsISAField(FieldISAPhase) {
		t.Errorf("expected %s to be an ISA field", FieldISAPhase)
	}
	if !IsISAField(FieldISAProgressM) {
		t.Errorf("expected %s to be an ISA field", FieldISAProgressM)
	}
	if IsISAField(FieldSlug) {
		t.Errorf("slug must NOT be classified as an ISA field")
	}
	if IsISAField("status") {
		t.Errorf("status must not be classified as an ISA field")
	}
}

// TestWrongKindError confirms the surfaced message contains the exact
// substring "only valid for kind=isa" — embedded tests grep for this.
func TestWrongKindError(t *testing.T) {
	err := WrongKindError(FieldISAPhase, "knowledge")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), "only valid for kind=isa") {
		t.Errorf("expected message to contain %q, got %q", "only valid for kind=isa", err.Error())
	}
	if !strings.Contains(err.Error(), FieldISAPhase) {
		t.Errorf("expected message to contain field name %q, got %q", FieldISAPhase, err.Error())
	}
}

// TestAllowedFieldsSorted asserts the surface order is deterministic and
// includes every field returned by IsAllowedField.
func TestAllowedFieldsSorted(t *testing.T) {
	got := AllowedFieldsSorted()
	if len(got) != 8 {
		t.Fatalf("expected 8 allowed fields, got %d: %v", len(got), got)
	}
	// Stability: calling twice must yield the same slice.
	got2 := AllowedFieldsSorted()
	for i := range got {
		if got[i] != got2[i] {
			t.Errorf("non-deterministic order at %d: %q vs %q", i, got[i], got2[i])
		}
		if !IsAllowedField(got[i]) {
			t.Errorf("AllowedFieldsSorted contains %q which is not allowed", got[i])
		}
	}
}
