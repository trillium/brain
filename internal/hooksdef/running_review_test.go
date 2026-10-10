//go:build !windows

package hooksdef

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A store path with glob metacharacters must not switch the ancestry layer off.
func TestAncestorRunningHookSeesMarkerInGlobCharacterPath(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), "proj[1]*?", ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(beadsDir, RunningPrefix+strconv.Itoa(os.Getppid())+RunningSuffix)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !AncestorRunningHook(beadsDir) {
		t.Fatal("a live ancestor's marker in a path containing [ * ? must still be detected")
	}
}

// A store directory that cannot be read cannot prove no hook is running.
func TestAncestorRunningHookUnreadableDirIsYes(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "notadir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !AncestorRunningHook(file) {
		t.Fatal("a read error other than not-exist must be treated as 'a hook is running'")
	}
	if AncestorRunningHook(filepath.Join(parent, "absent")) {
		t.Fatal("a store directory that does not exist has no markers")
	}
}

// The process walk must not resolve ps through PATH, which a hook controls.
func TestAncestryIgnoresPathShadowedPs(t *testing.T) {
	bin := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "fake-ps-ran")
	fake := "#!/bin/sh\ntouch " + sentinel + "\necho 1\n"
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, ok := ancestry(); !ok {
		t.Skip("ps unavailable in this environment")
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("ancestry ran the PATH-shadowed ps")
	}
	if !filepath.IsAbs(psPath) {
		t.Fatalf("psPath %q must be absolute", psPath)
	}
}
