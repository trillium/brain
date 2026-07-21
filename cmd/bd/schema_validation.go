// Package main — schema_validation.go
//
// Per-store content-schema validation. A store can require that issues of a
// given type (or carrying a given label) declare a set of top-level keys in
// their description body. The requirement is configured per selector via the
// `validation.schema.<type|label>` config key, whose value is a comma-
// separated list of required keys. Enforcement is gated by the same tristate
// as `validation.on-create` (warn | error | none) at both create and patch
// time.
//
// The body probe is dependency-free: a key counts as present if the
// description contains a top-level YAML key (`key:`) or a JSON member
// (`"key":`). No YAML/JSON parse — a regex probe that tolerates either
// encoding, so no new modules are pulled in.
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/config"
)

// requiredSchemaKeys returns the union of required body keys configured for an
// issue's type and each of its labels. Selectors are matched case-insensitively
// against `validation.schema.<selector>`; an unset or empty value contributes
// nothing. Order is deterministic: type keys first, then per-label keys in the
// order labels are supplied, de-duplicated.
func requiredSchemaKeys(issueType string, labels []string) []string {
	seen := make(map[string]bool)
	var out []string

	add := func(selector string) {
		selector = strings.ToLower(strings.TrimSpace(selector))
		if selector == "" {
			return
		}
		raw := config.GetString("validation.schema." + selector)
		for _, k := range strings.Split(raw, ",") {
			k = strings.TrimSpace(k)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}

	add(issueType)
	for _, l := range labels {
		add(l)
	}
	return out
}

// bodyHasKey reports whether description declares key as a top-level YAML key
// (`key:`, any indentation) or as a JSON object member (`"key":`).
func bodyHasKey(description, key string) bool {
	q := regexp.QuoteMeta(key)
	if regexp.MustCompile(`(?m)^\s*` + q + `\s*:`).MatchString(description) {
		return true
	}
	return regexp.MustCompile(`"` + q + `"\s*:`).MatchString(description)
}

// validateBodySchema checks that description contains every key required by the
// schema configured for the issue's type/labels. Returns an error naming the
// missing keys (sorted), or nil when the schema is satisfied or no schema is
// configured for any of the issue's selectors.
func validateBodySchema(issueType string, labels []string, description string) error {
	keys := requiredSchemaKeys(issueType, labels)
	if len(keys) == 0 {
		return nil
	}
	var missing []string
	for _, k := range keys {
		if !bodyHasKey(description, k) {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("body schema: description is missing required key(s): %s", strings.Join(missing, ", "))
}
