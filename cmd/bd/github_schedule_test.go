package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildGitHubScheduleJobPrefersTheStoreWrapper(t *testing.T) {
	home := t.TempDir()
	wrapper := filepath.Join(home, ".local", "bin", "brain")
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	job, err := buildGitHubScheduleJob("brain", home, 15*time.Minute, "/usr/bin:/opt/homebrew/bin", func() (string, error) {
		return "", errors.New("must not be asked")
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(job.Command, " ") != wrapper+" github sync" {
		t.Errorf("command = %v; the store's own wrapper carries its environment", job.Command)
	}
	if job.Label != "dev.brain.github-sync.brain" {
		t.Errorf("label = %q", job.Label)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents", job.Label+".plist"); job.PlistPath != want {
		t.Errorf("plist path = %q, want %q", job.PlistPath, want)
	}
}

func TestBuildGitHubScheduleJobFallsBackToTheExecutable(t *testing.T) {
	job, err := buildGitHubScheduleJob("nowrapper", t.TempDir(), time.Hour, "/usr/bin", func() (string, error) { return "/opt/bd", nil })
	if err != nil {
		t.Fatal(err)
	}
	if job.Command[0] != "/opt/bd" {
		t.Errorf("command = %v", job.Command)
	}
}

func TestBuildGitHubScheduleJobRefusesUnsafeStoreNames(t *testing.T) {
	for _, name := range []string{"", "a/b", "a b", "../x", "x\"y", "x<y"} {
		if _, err := buildGitHubScheduleJob(name, t.TempDir(), time.Hour, "", func() (string, error) { return "/bd", nil }); err == nil {
			t.Errorf("store name %q must be refused", name)
		}
	}
}

func TestGitHubSchedulePlist(t *testing.T) {
	job := &githubScheduleJob{
		Label:    "dev.brain.github-sync.brain",
		LogPath:  "/tmp/log & more.log",
		Command:  []string{"/home/me/.local/bin/brain", "github", "sync"},
		Interval: 90 * time.Second,
		Path:     "/usr/bin:/opt/homebrew/bin",
	}
	plist := job.Plist()
	for _, want := range []string{
		"<string>dev.brain.github-sync.brain</string>",
		"<string>/home/me/.local/bin/brain</string>",
		"<string>github</string>",
		"<string>sync</string>",
		"<integer>90</integer>",
		"<key>RunAtLoad</key>\n\t<false/>",
		"/tmp/log &amp; more.log",
		"<string>/usr/bin:/opt/homebrew/bin</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	// The job runs plain 'github sync': it must not smuggle in a token or a
	// forced direction.
	for _, banned := range []string{"GITHUB_TOKEN", "--prefer", "--direction", "--repo"} {
		if strings.Contains(plist, banned) {
			t.Errorf("plist must not contain %q", banned)
		}
	}
}
