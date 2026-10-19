//go:build cgo

package embeddeddolt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A store whose metadata.json pins dolt_mode=server must never get a shadow
// .beads/embeddeddolt directory from any embedded open path (robots-ucj59).
func TestOpen_RefusesToCreateEmbeddedInServerModeStore(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	metadata := `{"database":"beads","dolt_mode":"server"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}

	opens := map[string]func() error{
		"Open": func() error {
			_, err := Open(context.Background(), beadsDir, "beads", "main")
			return err
		},
		"OpenForReadOnlyCommand": func() error {
			_, err := OpenForReadOnlyCommand(context.Background(), beadsDir, "beads", "main")
			return err
		},
	}
	for name, open := range opens {
		t.Run(name, func(t *testing.T) {
			err := open()
			if err == nil {
				t.Fatal("expected refusal for server-mode store, got nil error")
			}
			if !strings.Contains(err.Error(), "dolt_mode=server") {
				t.Errorf("error should name the server-mode store, got: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(beadsDir, "embeddeddolt")); !os.IsNotExist(statErr) {
				t.Errorf("embeddeddolt directory must not be created, stat err = %v", statErr)
			}
		})
	}
}
