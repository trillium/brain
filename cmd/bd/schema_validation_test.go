package main

import "testing"

func TestBodyHasKey_YamlAndJsonForms(t *testing.T) {
	cases := []struct {
		name string
		body string
		key  string
		want bool
	}{
		{"yaml top-level", "ingredients:\n  - salt\nsteps: mix\n", "ingredients", true},
		{"yaml indented", "  steps: mix well\n", "steps", true},
		{"json member", `{"ingredients": ["salt"], "steps": "mix"}`, "steps", true},
		{"json spaced", `{ "steps" : "mix" }`, "steps", true},
		{"missing key", "title: soup\n", "ingredients", false},
		{"substring not a key", "num_steps: 3\n", "steps", false},
		{"key in prose only", "The steps are simple.\n", "steps", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bodyHasKey(tc.body, tc.key); got != tc.want {
				t.Errorf("bodyHasKey(%q, %q) = %v, want %v", tc.body, tc.key, got, tc.want)
			}
		})
	}
}

func TestValidateBodySchema_NoSchemaIsNoop(t *testing.T) {
	// With no validation.schema.* configured, any body passes.
	if err := validateBodySchema("recipe", []string{"food"}, "anything"); err != nil {
		t.Errorf("expected nil with no schema configured, got: %v", err)
	}
}
