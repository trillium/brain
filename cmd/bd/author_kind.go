package main

import (
	"encoding/json"
	"os"
	"strings"
)

// Author kind — who, in kind, wrote a bead.
//
// created_by names the filer (see getFilingActor) but not what sort of filer
// it was, so the share of beads a person wrote could only be bounded from
// above. At create time a bead now also records, in its metadata object:
//
//	author_kind   "human", "agent" or "procedural"
//	author_agent  the agent's name, only when author_kind is "agent"
//
// No issue type, column or schema changes: the two keys live in the existing
// metadata blob, and a bead created before this stamp simply lacks them.
//
// The kind is decided in this order:
//  1. BEADS_AUTHOR_KIND, when set to one of the three kinds. This is how a
//     script, hook or scheduled job declares itself "procedural". A value
//     outside the three kinds is ignored rather than stamped.
//  2. "agent", when the process runs as a fleet agent (agentIdentityActor).
//  3. "human".
//
// A key the caller already put in --metadata is never overwritten.

const (
	authorKindEnvVar = "BEADS_AUTHOR_KIND"

	authorKindKey  = "author_kind"
	authorAgentKey = "author_agent"

	authorKindHuman      = "human"
	authorKindAgent      = "agent"
	authorKindProcedural = "procedural"
)

// resolveAuthorKind returns the author kind of this process and, for an agent,
// its name.
func resolveAuthorKind() (kind, agent string) {
	switch declared := strings.ToLower(strings.TrimSpace(os.Getenv(authorKindEnvVar))); declared {
	case authorKindHuman, authorKindAgent, authorKindProcedural:
		kind = declared
	}
	if kind == "" {
		if agentIdentityActor() != "" {
			kind = authorKindAgent
		} else {
			kind = authorKindHuman
		}
	}
	if kind == authorKindAgent {
		agent = agentIdentityActor()
	}
	return kind, agent
}

// withAuthorKind returns metadata with the author-kind stamp added. A nil or
// empty input becomes a fresh object; metadata that is not a JSON object is
// returned unchanged, since there is nowhere to put the keys.
func withAuthorKind(metadata json.RawMessage) json.RawMessage {
	fields := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(metadata))) > 0 {
		if err := json.Unmarshal(metadata, &fields); err != nil || fields == nil {
			return metadata
		}
	}
	kind, agent := resolveAuthorKind()
	if _, set := fields[authorKindKey]; !set {
		fields[authorKindKey], _ = json.Marshal(kind)
		if agent != "" {
			if _, set := fields[authorAgentKey]; !set {
				fields[authorAgentKey], _ = json.Marshal(agent)
			}
		}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return metadata
	}
	return out
}
