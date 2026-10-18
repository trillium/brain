package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetActorWithGit tests the actor resolution fallback chain.
// Priority: --actor flag > BEADS_ACTOR env > BD_ACTOR env (deprecated) > fleet
// agent identity > git config user.name > $USER > "unknown"
func TestGetActorWithGit(t *testing.T) {
	// These cases exercise the fallbacks below the agent identity: a suite run
	// from inside an agent process must not see that agent's identity.
	t.Setenv(agentNameEnvVar, "")
	t.Setenv(agentIDEnvVar, "")

	// Save original environment and actor variable
	origActor := actor
	origBdActor, bdActorSet := os.LookupEnv("BD_ACTOR")
	origBeadsActor, beadsActorSet := os.LookupEnv("BEADS_ACTOR")
	origUser, userSet := os.LookupEnv("USER")

	// Cleanup after test
	defer func() {
		actor = origActor
		if bdActorSet {
			os.Setenv("BD_ACTOR", origBdActor)
		} else {
			os.Unsetenv("BD_ACTOR")
		}
		if beadsActorSet {
			os.Setenv("BEADS_ACTOR", origBeadsActor)
		} else {
			os.Unsetenv("BEADS_ACTOR")
		}
		if userSet {
			os.Setenv("USER", origUser)
		} else {
			os.Unsetenv("USER")
		}
	}()

	// Helper to get current git user.name (may be empty if not configured)
	getGitUserName := func() string {
		out, err := exec.Command("git", "config", "user.name").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	gitUserName := getGitUserName()

	tests := []struct {
		name        string
		actorFlag   string
		bdActor     string
		beadsActor  string
		user        string
		expected    string
		skipIfNoGit bool // Skip if git user.name is not configured
	}{
		{
			name:       "actor flag takes priority",
			actorFlag:  "flag-actor",
			bdActor:    "bd-actor",
			beadsActor: "beads-actor",
			user:       "system-user",
			expected:   "flag-actor",
		},
		{
			name:       "BEADS_ACTOR takes priority when no flag",
			actorFlag:  "",
			bdActor:    "bd-actor",
			beadsActor: "beads-actor",
			user:       "system-user",
			expected:   "beads-actor",
		},
		{
			name:       "BD_ACTOR used as fallback when no BEADS_ACTOR",
			actorFlag:  "",
			bdActor:    "bd-actor",
			beadsActor: "",
			user:       "system-user",
			expected:   "bd-actor",
		},
		{
			name:        "git config user.name used when no env vars",
			actorFlag:   "",
			bdActor:     "",
			beadsActor:  "",
			user:        "system-user",
			expected:    gitUserName, // Will be git user.name if configured
			skipIfNoGit: true,
		},
		{
			name:       "USER fallback when no git config",
			actorFlag:  "",
			bdActor:    "",
			beadsActor: "",
			user:       "fallback-user",
			expected:   "fallback-user",
			// Note: This test may fail if git user.name is configured
			// We handle this by checking the actual git config in the test
		},
		{
			name:       "unknown as final fallback",
			actorFlag:  "",
			bdActor:    "",
			beadsActor: "",
			user:       "",
			expected:   "unknown",
			// Note: This test may get git user.name instead if configured
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Skip tests that require git user.name to not be configured
			if tt.skipIfNoGit && gitUserName == "" {
				t.Skip("Skipping: git config user.name is not configured")
			}

			// For tests expecting USER or unknown, skip if git user.name is configured
			// because git takes priority over USER
			if (tt.expected == tt.user || tt.expected == "unknown") && gitUserName != "" && tt.bdActor == "" && tt.beadsActor == "" && tt.actorFlag == "" {
				t.Skipf("Skipping: git config user.name (%s) takes priority over expected %s", gitUserName, tt.expected)
			}

			// Set up test environment
			actor = tt.actorFlag

			if tt.bdActor != "" {
				os.Setenv("BD_ACTOR", tt.bdActor)
			} else {
				os.Unsetenv("BD_ACTOR")
			}

			if tt.beadsActor != "" {
				os.Setenv("BEADS_ACTOR", tt.beadsActor)
			} else {
				os.Unsetenv("BEADS_ACTOR")
			}

			if tt.user != "" {
				os.Setenv("USER", tt.user)
			} else {
				os.Unsetenv("USER")
			}

			// Call the function
			result := getActorWithGit()

			// Check result
			if result != tt.expected {
				t.Errorf("getActorWithGit() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// TestGetActorWithGit_PriorityOrder tests that the priority order is respected
func TestGetActorWithGit_PriorityOrder(t *testing.T) {
	t.Setenv(agentNameEnvVar, "")
	t.Setenv(agentIDEnvVar, "")

	// Save original state
	origActor := actor
	origBdActor, bdActorSet := os.LookupEnv("BD_ACTOR")
	origBeadsActor, beadsActorSet := os.LookupEnv("BEADS_ACTOR")

	defer func() {
		actor = origActor
		if bdActorSet {
			os.Setenv("BD_ACTOR", origBdActor)
		} else {
			os.Unsetenv("BD_ACTOR")
		}
		if beadsActorSet {
			os.Setenv("BEADS_ACTOR", origBeadsActor)
		} else {
			os.Unsetenv("BEADS_ACTOR")
		}
	}()

	// Test: flag > BEADS_ACTOR > BD_ACTOR
	actor = "from-flag"
	os.Setenv("BD_ACTOR", "from-bd-actor")
	os.Setenv("BEADS_ACTOR", "from-beads-actor")

	result := getActorWithGit()
	if result != "from-flag" {
		t.Errorf("Expected flag to take priority, got %q", result)
	}

	// Test: BEADS_ACTOR > BD_ACTOR (no flag)
	actor = ""
	result = getActorWithGit()
	if result != "from-beads-actor" {
		t.Errorf("Expected BEADS_ACTOR to take priority over BD_ACTOR, got %q", result)
	}

	// Test: BD_ACTOR as fallback when BEADS_ACTOR is empty
	os.Unsetenv("BEADS_ACTOR")
	result = getActorWithGit()
	if result != "from-bd-actor" {
		t.Errorf("Expected BD_ACTOR to be used as fallback, got %q", result)
	}
}

// TestGetActorWithGit_FleetAgentIdentity pins where the fleet agent identity sits
// in the chain (robots-0x8): it loses to every explicit actor and wins over the
// human git identity, so an agent-filed bead is stamped with the agent's name.
func TestGetActorWithGit_FleetAgentIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USER", "fallback-user")
	t.Setenv(agentNameEnvVar, "Volt")
	t.Setenv(agentIDEnvVar, "volt")

	origActor := actor
	t.Cleanup(func() { actor = origActor })

	// git user.name resolves to something human here; the agent identity must
	// still win, whatever git answers.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	if err := os.WriteFile(filepath.Join(home, "gitconfig"), []byte("[user]\n\tname = Human Owner\n"), 0o644); err != nil {
		t.Fatalf("write gitconfig: %v", err)
	}

	t.Run("agent identity outranks git user.name", func(t *testing.T) {
		actor = ""
		t.Setenv("BEADS_ACTOR", "")
		t.Setenv("BD_ACTOR", "")
		if got := getActorWithGit(); got != "Volt" {
			t.Errorf("getActorWithGit() = %q, want %q", got, "Volt")
		}
	})

	t.Run("name resolved from identity.md when only the id is exported", func(t *testing.T) {
		writeIdentityFile(t, home, "volt", "---\nid: volt\nname: \"Volt\"\n---\n")
		t.Setenv(agentNameEnvVar, "")
		actor = ""
		t.Setenv("BEADS_ACTOR", "")
		t.Setenv("BD_ACTOR", "")
		if got := getActorWithGit(); got != "Volt" {
			t.Errorf("getActorWithGit() = %q, want %q", got, "Volt")
		}
	})

	t.Run("BEADS_ACTOR outranks the agent identity", func(t *testing.T) {
		actor = ""
		t.Setenv("BEADS_ACTOR", "explicit-actor")
		t.Setenv("BD_ACTOR", "bd-actor")
		if got := getActorWithGit(); got != "explicit-actor" {
			t.Errorf("getActorWithGit() = %q, want %q", got, "explicit-actor")
		}
	})

	t.Run("BD_ACTOR outranks the agent identity", func(t *testing.T) {
		actor = ""
		t.Setenv("BEADS_ACTOR", "")
		t.Setenv("BD_ACTOR", "bd-actor")
		if got := getActorWithGit(); got != "bd-actor" {
			t.Errorf("getActorWithGit() = %q, want %q", got, "bd-actor")
		}
	})

	t.Run("--actor outranks the agent identity", func(t *testing.T) {
		actor = "flag-actor"
		t.Setenv("BEADS_ACTOR", "")
		t.Setenv("BD_ACTOR", "")
		if got := getActorWithGit(); got != "flag-actor" {
			t.Errorf("getActorWithGit() = %q, want %q", got, "flag-actor")
		}
	})

	t.Run("no agent identity falls through to git user.name", func(t *testing.T) {
		actor = ""
		t.Setenv(agentNameEnvVar, "")
		t.Setenv(agentIDEnvVar, "")
		t.Setenv("BEADS_ACTOR", "")
		t.Setenv("BD_ACTOR", "")
		// Whatever git answers here is human attribution; what matters is that
		// the agent identity above is not the one consulted when it is absent.
		want := "fallback-user"
		if out, err := exec.Command("git", "config", "user.name").Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				want = name
			}
		}
		if got := getActorWithGit(); got != want {
			t.Errorf("getActorWithGit() = %q, want %q", got, want)
		}
	})
}
