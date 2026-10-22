package main

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"

	braindocs "github.com/steveyegge/beads/docs/brain"
)

func runRunbook(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	brainRunbookCmd.SetOut(&out)
	t.Cleanup(func() { brainRunbookCmd.SetOut(nil) })
	err := brainRunbookCmd.RunE(brainRunbookCmd, args)
	return out.String(), err
}

func TestBrainRunbook_EveryEmbeddedTopicIsListedAndPrints(t *testing.T) {
	files, err := fs.Glob(braindocs.Runbooks, "RUNBOOK_*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no embedded runbooks: %v", err)
	}
	topics, err := runbookTopics(braindocs.Runbooks)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != len(files) {
		t.Fatalf("%d topics for %d embedded files", len(topics), len(files))
	}
	list, err := runRunbook(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, tp := range topics {
		if !strings.Contains(list, tp.Name) {
			t.Errorf("topic %q missing from list:\n%s", tp.Name, list)
		}
		body, err := runRunbook(t, tp.Name)
		if err != nil {
			t.Errorf("topic %q: %v", tp.Name, err)
		}
		if !strings.HasPrefix(body, "# ") || len(body) < 500 {
			t.Errorf("topic %q printed an implausible body (%d bytes)", tp.Name, len(body))
		}
	}
	if _, err := runRunbook(t, "stores-and-deploy"); err != nil {
		t.Errorf("stores-and-deploy topic: %v", err)
	}
}

func TestBrainRunbook_UnknownTopic(t *testing.T) {
	if _, err := runRunbook(t, "no-such-topic"); err == nil || !strings.Contains(err.Error(), "brain runbook") {
		t.Fatalf("want an error pointing at 'brain runbook', got %v", err)
	}
}
