package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/config"
)

func TestWorkspaceDiagHint_HintsInitConfiguration(t *testing.T) {
	original, wasSet := os.LookupEnv("BD_HINTS_INIT")
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv("BD_HINTS_INIT", original)
		} else {
			_ = os.Unsetenv("BD_HINTS_INIT")
		}
		if err := config.Initialize(); err != nil {
			t.Errorf("restore config: %v", err)
		}
	})

	t.Run("default retains standalone init guidance", func(t *testing.T) {
		_ = os.Unsetenv("BD_HINTS_INIT")
		if err := config.Initialize(); err != nil {
			t.Fatal(err)
		}
		if !config.GetBool(ConfigKeyHintsInit) {
			t.Fatalf("%s should default to true", ConfigKeyHintsInit)
		}
		if hint := diagHint(); !strings.Contains(hint, "bd init") {
			t.Fatalf("default diagnostic hint lost init guidance: %q", hint)
		}
	})

	t.Run("wrapper environment suppresses only init guidance", func(t *testing.T) {
		if err := os.Setenv("BD_HINTS_INIT", "false"); err != nil {
			t.Fatal(err)
		}
		if err := config.Initialize(); err != nil {
			t.Fatal(err)
		}
		if config.GetBool(ConfigKeyHintsInit) {
			t.Fatalf("%s should honor BD_HINTS_INIT=false", ConfigKeyHintsInit)
		}
		if hint := diagHint(); strings.Contains(hint, "bd init") || !strings.Contains(hint, "bd where") {
			t.Fatalf("suppressed hint = %q; want workspace guidance without bd init", hint)
		}
		if hint := whereDiagHint(); strings.Contains(hint, "bd init") || !strings.Contains(hint, "BEADS_DIR/worktree setup") {
			t.Fatalf("suppressed where hint = %q; want workspace guidance without bd init", hint)
		}
	})
}

func TestJsonStderrError_StructuredOutput(t *testing.T) {
	tests := []struct {
		name    string
		message string
		hint    string
	}{
		{"message_only", "database not found", ""},
		{"message_with_hint", "database not found", "Run 'bd init' to create one"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := map[string]interface{}{
				"schema_version": JSONSchemaVersion,
				"error":          tt.message,
			}
			if tt.hint != "" {
				obj["hint"] = tt.hint
			}

			data, err := json.Marshal(obj)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var parsed map[string]interface{}
			if err := json.Unmarshal(data, &parsed); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if parsed["schema_version"] != float64(JSONSchemaVersion) {
				t.Errorf("schema_version = %v, want %d", parsed["schema_version"], JSONSchemaVersion)
			}
			if parsed["error"] != tt.message {
				t.Errorf("error = %v, want %s", parsed["error"], tt.message)
			}
			if tt.hint != "" {
				if parsed["hint"] != tt.hint {
					t.Errorf("hint = %v, want %s", parsed["hint"], tt.hint)
				}
			} else {
				if _, ok := parsed["hint"]; ok {
					t.Errorf("hint should not be present when empty")
				}
			}
		})
	}
}
