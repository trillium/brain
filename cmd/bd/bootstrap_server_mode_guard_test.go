//go:build cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
)

// Bootstrap's auto-init and clone paths must refuse to create an embedded
// database inside a dolt_mode=server store (robots-ucj59).
func TestBootstrapRefusesEmbeddedCreateInServerModeStore(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := configfile.DefaultConfig()
	cfg.DoltMode = configfile.DoltModeServer
	if err := cfg.Save(beadsDir); err != nil {
		t.Fatal(err)
	}

	assertNoEmbedded := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected refusal for server-mode store, got nil error")
		}
		if !strings.Contains(err.Error(), "dolt_mode=server") {
			t.Errorf("error should name the server-mode store, got: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(beadsDir, "embeddeddolt")); !os.IsNotExist(statErr) {
			t.Errorf("embeddeddolt directory must not be created, stat err = %v", statErr)
		}
	}

	t.Run("init action", func(t *testing.T) {
		plan := BootstrapPlan{BeadsDir: beadsDir, Database: cfg.GetDoltDatabase(), Action: "init"}
		assertNoEmbedded(t, executeInitAction(context.Background(), plan, cfg))
	})

	t.Run("clone via embedded", func(t *testing.T) {
		assertNoEmbedded(t, cloneViaEmbedded(context.Background(), beadsDir, "file:///nonexistent", "beads"))
	})
}

// Bootstrap decides server mode from the target workspace as well as the
// resolved config, so every server-mode signal must stop an embedded create.
func TestBootstrapPlanRefusesEmbeddedCreateWhenServerModeResolved(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, beadsDir string) *configfile.Config
	}{
		{
			name: "shared-server env var",
			setup: func(t *testing.T, beadsDir string) *configfile.Config {
				t.Setenv("BEADS_DOLT_SHARED_SERVER", "1")
				return configfile.DefaultConfig()
			},
		},
		{
			name: "dolt.shared-server in target config.yaml",
			setup: func(t *testing.T, beadsDir string) *configfile.Config {
				if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("dolt.shared-server: true\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return configfile.DefaultConfig()
			},
		},
		{
			name: "server-mode config inherited from parent workspace",
			setup: func(t *testing.T, beadsDir string) *configfile.Config {
				cfg := configfile.DefaultConfig()
				cfg.DoltMode = configfile.DoltModeServer
				return cfg
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BEADS_DOLT_SHARED_SERVER", "")
			beadsDir := filepath.Join(t.TempDir(), ".beads")
			if err := os.MkdirAll(beadsDir, 0o750); err != nil {
				t.Fatal(err)
			}
			cfg := tt.setup(t, beadsDir)

			plan := BootstrapPlan{BeadsDir: beadsDir, Database: cfg.GetDoltDatabase(), Action: "init"}
			err := executeBootstrapPlan(plan, cfg, true)
			if err == nil || !strings.Contains(err.Error(), "refusing to create embedded Dolt database") {
				t.Fatalf("expected embedded-create refusal, got: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(beadsDir, "embeddeddolt")); !os.IsNotExist(statErr) {
				t.Errorf("embeddeddolt directory must not be created, stat err = %v", statErr)
			}
		})
	}
}
