package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Fleet agent identity — "federated stores don't stamp the filing agent"
// (robots-0x8).
//
// Every store write already carries an actor: bd records it as created_by on the
// bead and as the actor on that bead's audit events, and getActorWithGit
// resolves the actor from an explicit override down to the human's git
// identity. What was missing was the middle of that chain — an agent process
// knows its own name, and nothing passed it on — so a bead an agent filed was
// stamped with whichever human git identity happened to be configured on the
// checkout, and "which agent filed this bead" was unanswerable.
//
// The fleet already keys agent identity on PARLAY_AGENT_ID: parlay's launch,
// identity, scratchpad and handoff tooling all resolve agents through it, and
// an agent's human-readable display name is either exported beside it
// (PARLAY_AGENT_NAME) or held in that agent's identity.md frontmatter. bd adds
// no second identity concept, no new field, and no schema: it reads the
// identity that already exists and hands it to the actor channel that already
// exists.

const (
	// agentIDEnvVar names this process's fleet agent.
	agentIDEnvVar = "PARLAY_AGENT_ID"
	// agentNameEnvVar carries that agent's display name; parlay exports both
	// together when it launches an agent.
	agentNameEnvVar = "PARLAY_AGENT_NAME"
)

// agentIDRe is parlay's own agent-id shape (^[A-Za-z0-9._-]+$). An id outside
// it is refused so a stray environment value can never name a path outside the
// agents directory.
var agentIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// agentIdentityActor returns the display name of the fleet agent running this
// process, or "" when the process is not running as a fleet agent.
//
// Resolution, most specific first:
//  1. $PARLAY_AGENT_NAME — the display name exported alongside the id.
//  2. the `name:` field of $HOME/.parlay/agents/<id>/identity.md — the agent's
//     own name, for a process that kept only the id.
//  3. $PARLAY_AGENT_ID itself — still an answer to "which agent filed this
//     bead" when no richer name is available.
//
// An explicit actor (--actor, the actor config key, BEADS_ACTOR, BD_ACTOR)
// always outranks this: see getActorWithGit.
func agentIdentityActor() string {
	if name := strings.TrimSpace(os.Getenv(agentNameEnvVar)); name != "" {
		return name
	}
	id := strings.TrimSpace(os.Getenv(agentIDEnvVar))
	// An id parlay would not have minted is not an identity: ignore it rather
	// than stamping it on a bead.
	if !agentIDRe.MatchString(id) {
		return ""
	}
	if name := agentIdentityFileName(id); name != "" {
		return name
	}
	return id
}

// agentIdentityFileName reads the `name:` field from an agent's identity.md
// frontmatter. An unreadable, malformed, or nameless file simply yields "",
// leaving the caller to fall back. The id guard keeps a caller that skipped
// agentIdentityActor's check from naming a path outside the agents directory.
func agentIdentityFileName(id string) string {
	if !agentIDRe.MatchString(id) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".parlay", "agents", id, "identity.md")) //nolint:gosec // id is validated above
	if err != nil {
		return ""
	}
	return frontmatterName(string(data))
}

// frontmatterName extracts the first top-level `name:` value from a document's
// leading `---` YAML frontmatter, or "" when there is none. Surrounding quotes
// are stripped, matching an identity.md that names a display name with spaces
// (`name: "Grandma Danny II"`).
func frontmatterName(doc string) string {
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, "\r")
		if line == "---" {
			return ""
		}
		// Only top-level keys count: an indented `name:` belongs to a nested
		// structure, not to the identity itself.
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "name" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}
