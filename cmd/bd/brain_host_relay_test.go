package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeRelayFile(t *testing.T, home, rel, body string) {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectRelay(t *testing.T) {
	up := func(int) bool { return true }
	down := func(int) bool { return false }
	const fwd = "TARGET = \"mini0:3307\"\n"

	t.Run("client or host machine has no forwarder", func(t *testing.T) {
		if got := detectRelay(t.TempDir(), up); got.Role != relayNone {
			t.Errorf("role = %q, want none", got.Role)
		}
	})
	t.Run("forwarder, no dolt-server job, port answering is a relay", func(t *testing.T) {
		home := t.TempDir()
		writeRelayFile(t, home, relayForwarderPath, fwd)
		writeRelayFile(t, home, "Library/LaunchAgents/com.brain.dolt-server.plist.disabled", "x")
		got := detectRelay(home, up)
		if got.Role != relayActive || got.Target != "mini0:3307" {
			t.Errorf("got %+v, want relay to mini0:3307", got)
		}
	})
	t.Run("unrecognised forwarder still a relay with no target", func(t *testing.T) {
		home := t.TempDir()
		writeRelayFile(t, home, relayForwarderPath, "print('hi')\n")
		got := detectRelay(home, up)
		if got.Role != relayActive || got.Target != "" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("a live dolt-server job next to the forwarder is unknown", func(t *testing.T) {
		home := t.TempDir()
		writeRelayFile(t, home, relayForwarderPath, fwd)
		writeRelayFile(t, home, "Library/LaunchAgents/com.brain.dolt-server.plist", "x")
		if got := detectRelay(home, up); got.Role != relayUnknown {
			t.Errorf("role = %q, want unknown", got.Role)
		}
	})
	t.Run("forwarder but nothing listening is unknown", func(t *testing.T) {
		home := t.TempDir()
		writeRelayFile(t, home, relayForwarderPath, fwd)
		if got := detectRelay(home, down); got.Role != relayUnknown {
			t.Errorf("role = %q, want unknown", got.Role)
		}
	})
}

func TestRelayDescribe(t *testing.T) {
	if got := (relayInfo{Role: relayActive, Target: "mini0:3307"}).describe(); got != "relay — forwards port 3307 to mini0:3307; not a Dolt host" {
		t.Errorf("describe = %q", got)
	}
	if got := (relayInfo{Role: relayUnknown, Reason: "why"}).describe(); got != "unknown — why (forwards to (target not recognised))" {
		t.Errorf("describe = %q", got)
	}
}
