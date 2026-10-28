package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A machine can sit between clients and the brain host without being either:
// it only relays the Dolt port to the host through a small forwarder script.
// Such a machine has no host.yaml, which 'brain host status' used to read as
// "host (implied)". Detection stays local and cheap: files and one loopback
// dial, no network calls.

const relayForwarderPath = ".local/share/dolt-forward/fwd.py"

type relayRole string

const (
	relayNone    relayRole = ""        // no forwarder here; fall back to the usual reading
	relayActive  relayRole = "relay"   // forwarder present, no local dolt-server job, port answering
	relayUnknown relayRole = "unknown" // forwarder present but the rest does not add up
)

type relayInfo struct {
	Role   relayRole
	Target string // forward target as written in the forwarder, "" when not recognisable
	Reason string // why the role is unknown
}

// relayTargetRe finds the first host[:port] string literal assigned to a
// target-like name in the forwarder, e.g. TARGET = "mini0:3307".
var relayTargetRe = regexp.MustCompile(`(?mi)^\s*[A-Za-z_]*(?:target|upstream|dest|remote|host)[A-Za-z_]*\s*=\s*["']([A-Za-z0-9._-]+(?::\d+)?)["']`)

// detectRelay reports whether this machine only forwards the Dolt port.
func detectRelay(home string, listening func(port int) bool) relayInfo {
	fwd := filepath.Join(home, relayForwarderPath)
	data, err := os.ReadFile(fwd) //nolint:gosec // fixed path under the user's home
	if err != nil {
		return relayInfo{}
	}
	info := relayInfo{}
	if m := relayTargetRe.FindStringSubmatch(string(data)); m != nil {
		info.Target = m[1]
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.brain.dolt-server.plist")
	if _, err := os.Stat(plist); err == nil {
		info.Role, info.Reason = relayUnknown, "a dolt-server job is installed next to the forwarder"
		return info
	}
	if !listening(3307) {
		info.Role, info.Reason = relayUnknown, "forwarder present but nothing answers on port 3307"
		return info
	}
	info.Role = relayActive
	return info
}

// describe is the role line for 'brain host status'.
func (r relayInfo) describe() string {
	target := r.Target
	if target == "" {
		target = "(target not recognised)"
	}
	if r.Role == relayUnknown {
		return "unknown — " + r.Reason + " (forwards to " + target + ")"
	}
	return "relay — forwards port 3307 to " + target + "; not a Dolt host"
}

func (r relayInfo) String() string { return strings.TrimSpace(string(r.Role)) }
