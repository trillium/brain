package dolt

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A client wired straight to a Dolt server on another machine must fail in
// about the 500 ms dial probe when that machine is unreachable, not hang the
// hook that ran it (docs/brain/REMOTE.md, "Direct transport").
func TestNew_UnreachableRemoteHostFailsFast(t *testing.T) {
	t.Setenv("BEADS_TEST_MODE", "")
	t.Setenv("BEADS_DOLT_AUTO_START", "")

	dir := t.TempDir()
	// 192.0.2.0/24 (TEST-NET-1) is never routed: the dial times out or is refused.
	cfg := &Config{
		Path:       dir,
		BeadsDir:   dir,
		Database:   fmt.Sprintf("unreachable_%d", time.Now().UnixNano()),
		ServerHost: "192.0.2.1",
		ServerPort: 3307,
	}

	start := time.Now()
	store, err := New(context.Background(), cfg)
	elapsed := time.Since(start)
	if err == nil {
		_ = store.Close()
		t.Fatal("New succeeded against an unreachable host")
	}
	if !strings.Contains(err.Error(), "unreachable at 192.0.2.1:3307") {
		t.Errorf("error should name the unreachable endpoint, got: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("unreachable host took %v to fail; the dial probe allows 500ms", elapsed)
	}
}
