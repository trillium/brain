package testutil

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// IsolateGitConfig points git at an empty per-test global config file and
// disables the system config, so a test never observes — or is influenced
// by — the developer's real ~/.gitconfig.
//
// Without this, any check that shells out to `git config --get <key>` reads
// whatever the machine running the test happens to have set. That makes the
// "key is not configured" and "not a git repository" cases impossible to
// exercise: git walks up to the global config and answers from there even
// when the temp repo (or plain directory) under test has nothing set.
//
// Uses t.Setenv, so the calling test must not be parallel.
func IsolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
}

// ForceRepoLocalHooksPath configures a git test repository to use .git/hooks
// regardless of any global core.hooksPath configuration.
func ForceRepoLocalHooksPath(repoDir string) error {
	cmd := exec.Command("git", "config", "core.hooksPath", ".git/hooks")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("set core.hooksPath in %s: %w (output: %s)", repoDir, err, trimmed)
		}
		return fmt.Errorf("set core.hooksPath in %s: %w", repoDir, err)
	}
	return nil
}
