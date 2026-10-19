package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckEmbeddedCreateAllowed(t *testing.T) {
	tests := []struct {
		name     string
		metadata string // empty means no metadata.json is written
		wantErr  bool
	}{
		{name: "no metadata file", metadata: "", wantErr: false},
		{name: "no dolt_mode", metadata: `{"database":"beads"}`, wantErr: false},
		{name: "embedded mode", metadata: `{"database":"beads","dolt_mode":"embedded"}`, wantErr: false},
		{name: "server mode", metadata: `{"database":"beads","dolt_mode":"server"}`, wantErr: true},
		{name: "server mode uppercase", metadata: `{"database":"beads","dolt_mode":"SERVER"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beadsDir := filepath.Join(t.TempDir(), ".beads")
			if err := os.MkdirAll(beadsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if tt.metadata != "" {
				if err := os.WriteFile(ConfigPath(beadsDir), []byte(tt.metadata), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := CheckEmbeddedCreateAllowed(beadsDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckEmbeddedCreateAllowed() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "dolt_mode=server") {
				t.Errorf("error should name the server-mode store, got: %v", err)
			}
		})
	}
}
