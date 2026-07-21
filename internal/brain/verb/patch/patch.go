// Package patch implements pure-Go validation for the `bd patch` verb.
//
// `bd patch` writes a single field on an issue non-interactively. It is the
// primary mutation path for ISA-substrate fields (isa_phase, isa_progress_m,
// isa_progress_n, isa_effort, isa_mode, isa_started_at, isa_updated_at, slug).
//
// This package contains the kind-agnostic validation primitives: enum sets,
// the allowlist of ISA fields, and helpers to validate a field/value pair
// before any storage call is made. The cobra command in cmd/bd/patch.go
// composes these primitives with database lookups (current progress_n, row
// kind) to produce the full F1b behavior.
package patch

import (
	"fmt"
	"strconv"
	"strings"
)

// Field name constants for the ISA substrate columns.
const (
	FieldISAPhase     = "isa_phase"
	FieldISAProgressM = "isa_progress_m"
	FieldISAProgressN = "isa_progress_n"
	FieldISAEffort    = "isa_effort"
	FieldISAMode      = "isa_mode"
	FieldISAStartedAt = "isa_started_at"
	FieldISAUpdatedAt = "isa_updated_at"
	FieldSlug         = "slug"
)

// ValidPhases enumerates the legal values for isa_phase. The Algorithm v6.3.0
// defines seven phases; this set mirrors that contract exactly.
var ValidPhases = map[string]struct{}{
	"OBSERVE": {},
	"THINK":   {},
	"PLAN":    {},
	"BUILD":   {},
	"EXECUTE": {},
	"VERIFY":  {},
	"LEARN":   {},
}

// ValidEfforts enumerates the legal values for isa_effort.
var ValidEfforts = map[string]struct{}{
	"E1": {},
	"E2": {},
	"E3": {},
	"E4": {},
	"E5": {},
}

// ValidModes enumerates the legal values for isa_mode.
var ValidModes = map[string]struct{}{
	"NATIVE":    {},
	"ALGORITHM": {},
	"MINIMAL":   {},
}

// isaFields is the set of ISA-only fields. Patches to these on a non-ISA row
// must be rejected with exit 2.
var isaFields = map[string]struct{}{
	FieldISAPhase:     {},
	FieldISAProgressM: {},
	FieldISAProgressN: {},
	FieldISAEffort:    {},
	FieldISAMode:      {},
	FieldISAStartedAt: {},
	FieldISAUpdatedAt: {},
}

// allowedFields is the full allowlist of fields the patch verb accepts.
// It includes the ISA fields plus `slug`, which is patchable on any kind but
// does NOT participate in isa_updated_at touching.
var allowedFields = map[string]struct{}{
	FieldISAPhase:     {},
	FieldISAProgressM: {},
	FieldISAProgressN: {},
	FieldISAEffort:    {},
	FieldISAMode:      {},
	FieldISAStartedAt: {},
	FieldISAUpdatedAt: {},
	FieldSlug:         {},
}

// IsAllowedField reports whether name is a field bd patch knows how to write
// directly (via the ISA path or as `slug`). Non-allowed fields are not
// rejected here — the patch command routes them to storage.UpdateIssue, which
// has its own allowlist (IsAllowedUpdateField). This function is only used to
// decide which write path to take.
func IsAllowedField(name string) bool {
	_, ok := allowedFields[name]
	return ok
}

// IsISAField reports whether name is one of the ISA substrate fields. Slug is
// deliberately NOT an ISA field: it is patchable on any kind and never bumps
// isa_updated_at.
func IsISAField(name string) bool {
	_, ok := isaFields[name]
	return ok
}

// AllowedFieldsSorted returns the allowlist in a deterministic order, suitable
// for surfacing in error messages.
func AllowedFieldsSorted() []string {
	// Manual ordering keeps related fields adjacent and stable across Go map
	// iteration randomization.
	return []string{
		FieldISAPhase,
		FieldISAProgressM,
		FieldISAProgressN,
		FieldISAEffort,
		FieldISAMode,
		FieldISAStartedAt,
		FieldISAUpdatedAt,
		FieldSlug,
	}
}

// ValidationError describes a user-facing validation failure. Callers should
// surface its Message via stderr and exit with code 2.
type ValidationError struct {
	Field   string
	Value   string
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// newEnumError builds a ValidationError listing the valid enum members in
// deterministic alphabetical order so the surfaced message is stable across
// runs (Go's map iteration order is randomized).
func newEnumError(field, value string, allowed map[string]struct{}) *ValidationError {
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	// Insertion-style sort to avoid pulling sort just for ~7 entries; stable
	// alphabetic order is what matters for the error message.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return &ValidationError{
		Field:   field,
		Value:   value,
		Message: fmt.Sprintf("invalid value %q for field %s: valid values are %s", value, field, strings.Join(keys, ", ")),
	}
}

// ValidateEnum validates that value belongs to the named enum field. Returns
// nil on success or a ValidationError otherwise. Caller must guarantee that
// `field` is one of FieldISAPhase, FieldISAEffort, FieldISAMode.
func ValidateEnum(field, value string) error {
	switch field {
	case FieldISAPhase:
		if _, ok := ValidPhases[value]; !ok {
			return newEnumError(field, value, ValidPhases)
		}
	case FieldISAEffort:
		if _, ok := ValidEfforts[value]; !ok {
			return newEnumError(field, value, ValidEfforts)
		}
	case FieldISAMode:
		if _, ok := ValidModes[value]; !ok {
			return newEnumError(field, value, ValidModes)
		}
	default:
		// Defensive: callers should not invoke ValidateEnum on non-enum fields.
		return &ValidationError{
			Field:   field,
			Value:   value,
			Message: fmt.Sprintf("field %s is not an enum field", field),
		}
	}
	return nil
}

// ValidateProgressM checks that the proposed isa_progress_m value is a
// non-negative integer that does not exceed currentN (the current
// isa_progress_n on the row). Returns the parsed integer on success.
func ValidateProgressM(value string, currentN int) (int, error) {
	m, err := strconv.Atoi(value)
	if err != nil {
		return 0, &ValidationError{
			Field:   FieldISAProgressM,
			Value:   value,
			Message: fmt.Sprintf("invalid value %q for field %s: must be a non-negative integer", value, FieldISAProgressM),
		}
	}
	if m < 0 {
		return 0, &ValidationError{
			Field:   FieldISAProgressM,
			Value:   value,
			Message: fmt.Sprintf("invalid value %q for field %s: must be >= 0", value, FieldISAProgressM),
		}
	}
	if m > currentN {
		return 0, &ValidationError{
			Field:   FieldISAProgressM,
			Value:   value,
			Message: fmt.Sprintf("invalid value %d for field %s: must be <= isa_progress_n (%d)", m, FieldISAProgressM, currentN),
		}
	}
	return m, nil
}

// ValidateProgressN checks that the proposed isa_progress_n value is a
// non-negative integer. The progress-m ≤ progress-n invariant is enforced at
// the row level: callers must verify that the existing isa_progress_m on the
// row does not exceed the proposed N (otherwise we would silently allow the
// invariant to be broken from the other side).
func ValidateProgressN(value string, currentM int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, &ValidationError{
			Field:   FieldISAProgressN,
			Value:   value,
			Message: fmt.Sprintf("invalid value %q for field %s: must be a non-negative integer", value, FieldISAProgressN),
		}
	}
	if n < 0 {
		return 0, &ValidationError{
			Field:   FieldISAProgressN,
			Value:   value,
			Message: fmt.Sprintf("invalid value %q for field %s: must be >= 0", value, FieldISAProgressN),
		}
	}
	if n < currentM {
		return 0, &ValidationError{
			Field:   FieldISAProgressN,
			Value:   value,
			Message: fmt.Sprintf("invalid value %d for field %s: must be >= isa_progress_m (%d)", n, FieldISAProgressN, currentM),
		}
	}
	return n, nil
}

// WrongKindError is returned by callers when an ISA-only field is patched on
// an issue whose issue_type != 'isa'. Surfaced as a ValidationError-shaped
// message so the cmd/bd layer can treat it identically (exit 2).
func WrongKindError(field, kind string) *ValidationError {
	return &ValidationError{
		Field:   field,
		Value:   "",
		Message: fmt.Sprintf("field %s only valid for kind=isa (got kind=%s)", field, kind),
	}
}
