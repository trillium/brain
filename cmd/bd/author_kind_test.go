package main

import (
	"encoding/json"
	"testing"
)

func decodeStamp(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("metadata %q is not a JSON object: %v", raw, err)
	}
	return m
}

// clearAuthorEnv removes every input that could name an author, so a test
// states exactly the ones it cares about.
func clearAuthorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(authorKindEnvVar, "")
	t.Setenv(agentNameEnvVar, "")
	t.Setenv(agentIDEnvVar, "")
}

func TestResolveAuthorKind(t *testing.T) {
	tests := []struct {
		name      string
		declared  string
		agentName string
		wantKind  string
		wantAgent string
	}{
		{name: "no signal is human", wantKind: "human"},
		{name: "fleet agent is agent", agentName: "Volt", wantKind: "agent", wantAgent: "Volt"},
		{name: "declared procedural beats agent identity", declared: "procedural", agentName: "Volt", wantKind: "procedural"},
		{name: "declared human beats agent identity", declared: "human", agentName: "Volt", wantKind: "human"},
		{name: "declared kind is case and space tolerant", declared: " Procedural ", wantKind: "procedural"},
		{name: "declared agent without identity has no name", declared: "agent", wantKind: "agent"},
		{name: "unknown declared kind is ignored", declared: "robot", wantKind: "human"},
		{name: "unknown declared kind falls back to agent", declared: "robot", agentName: "Volt", wantKind: "agent", wantAgent: "Volt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearAuthorEnv(t)
			t.Setenv(authorKindEnvVar, tt.declared)
			t.Setenv(agentNameEnvVar, tt.agentName)
			kind, agent := resolveAuthorKind()
			if kind != tt.wantKind || agent != tt.wantAgent {
				t.Errorf("resolveAuthorKind() = %q, %q; want %q, %q", kind, agent, tt.wantKind, tt.wantAgent)
			}
		})
	}
}

func TestWithAuthorKind(t *testing.T) {
	t.Run("fresh object for empty metadata", func(t *testing.T) {
		clearAuthorEnv(t)
		m := decodeStamp(t, withAuthorKind(nil))
		if m["author_kind"] != "human" {
			t.Errorf("author_kind = %v, want human", m["author_kind"])
		}
		if _, ok := m["author_agent"]; ok {
			t.Errorf("author_agent present for a human: %v", m)
		}
	})
	t.Run("agent stamps its name", func(t *testing.T) {
		clearAuthorEnv(t)
		t.Setenv(agentNameEnvVar, "Volt")
		m := decodeStamp(t, withAuthorKind(nil))
		if m["author_kind"] != "agent" || m["author_agent"] != "Volt" {
			t.Errorf("stamp = %v, want agent/Volt", m)
		}
	})
	t.Run("procedural carries no agent name", func(t *testing.T) {
		clearAuthorEnv(t)
		t.Setenv(authorKindEnvVar, "procedural")
		t.Setenv(agentNameEnvVar, "Volt")
		m := decodeStamp(t, withAuthorKind(nil))
		if m["author_kind"] != "procedural" {
			t.Errorf("author_kind = %v, want procedural", m["author_kind"])
		}
		if _, ok := m["author_agent"]; ok {
			t.Errorf("author_agent present for procedural: %v", m)
		}
	})
	t.Run("existing keys are preserved", func(t *testing.T) {
		clearAuthorEnv(t)
		m := decodeStamp(t, withAuthorKind(json.RawMessage(`{"brain_slug":"x","n":3}`)))
		if m["brain_slug"] != "x" || m["n"] != float64(3) || m["author_kind"] != "human" {
			t.Errorf("merged metadata = %v", m)
		}
	})
	t.Run("caller-set author_kind is never overwritten", func(t *testing.T) {
		clearAuthorEnv(t)
		t.Setenv(agentNameEnvVar, "Volt")
		m := decodeStamp(t, withAuthorKind(json.RawMessage(`{"author_kind":"procedural"}`)))
		if m["author_kind"] != "procedural" {
			t.Errorf("author_kind = %v, want caller's procedural", m["author_kind"])
		}
		if _, ok := m["author_agent"]; ok {
			t.Errorf("author_agent added next to a caller-set kind: %v", m)
		}
	})
	t.Run("non-object metadata is returned unchanged", func(t *testing.T) {
		clearAuthorEnv(t)
		for _, in := range []string{`[1,2]`, `"s"`, `null`} {
			if got := string(withAuthorKind(json.RawMessage(in))); got != in {
				t.Errorf("withAuthorKind(%s) = %s, want unchanged", in, got)
			}
		}
	})
}
