// Package main — brain_host_follow.go
//
// Self-correcting clients. When the host role moves, the old host keeps a
// durable forwarding record naming the new host. A client that still points at
// the old host (it was offline during the move, or was never registered with
// the host) is told so the next time it runs any brain command; it then checks
// that the machine it was sent to really is a brain host and really is the one
// the old host named, rewrites its own remote config, and runs the command
// again once.
//
// What the client trusts, and what it checks:
//
//   - It trusts the host it was configured with, reached over ssh (so ssh's own
//     host-key check is the anchor). A third machine cannot redirect it: only an
//     answer from that host, over that connection, is followed.
//   - It does not trust the redirect blindly. It connects to the named machine
//     and requires it to say it is a brain host, and to show a lineage — the
//     chain of promotions that made it the host — that contains the promotion
//     the old host named (its id, generation and the machine that handed over).
//     A machine the old host merely named, or one that claims the role without
//     having received it, fails that check.
//   - A chain of moves (A → B → C) is followed hop by hop, each hop verified
//     against the final host's lineage; loops and chains longer than five hops
//     are refused.
//   - If anything cannot be verified, nothing is rewritten, the command is not
//     run anywhere, and the client says exactly what it could not verify.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// forwardRecordRel is the old host's durable forwarding record.
	forwardRecordRel = ".config/brain/forward.yaml"

	// hostMovedUnverifiedExit is the status a client exits with when the host
	// said it moved but the move could not be verified (EX_PROTOCOL).
	hostMovedUnverifiedExit = 76

	maxFollowHops = 5

	// envFollowAcceptNew lets the verification connection accept the host key of
	// a machine this client has never connected to (ssh StrictHostKeyChecking=
	// accept-new). Off by default: an unknown host key stops the follow.
	envFollowAcceptNew = "BRAIN_FOLLOW_ACCEPT_NEW"
)

// lineageEntry is one promotion in the chain that produced a host: who handed
// the role to whom, under which promotion id, at which generation.
type lineageEntry struct {
	PromoteID  string `yaml:"promote_id" json:"promote_id"`
	Generation int    `yaml:"generation" json:"generation"`
	From       string `yaml:"from" json:"from"` // machine id of the host that handed the role over
	To         string `yaml:"to" json:"to"`     // machine id of the host that received it
	At         string `yaml:"at,omitempty" json:"at,omitempty"`
}

// forwardRecord is what a host that gave up the role keeps, so that clients
// still pointing at it can find the new host.
type forwardRecord struct {
	To          string `yaml:"to" json:"to"` // ssh target of the new host, as this machine reaches it
	PromoteID   string `yaml:"promote_id" json:"promote_id"`
	Generation  int    `yaml:"generation" json:"generation"` // the new host's generation
	FromMachine string `yaml:"from_machine" json:"from_machine"`
	MovedAt     string `yaml:"moved_at" json:"moved_at"`
}

func forwardRecordFile(home string) string { return filepath.Join(home, forwardRecordRel) }

func loadForwardRecord(home string) (*forwardRecord, error) {
	data, err := os.ReadFile(forwardRecordFile(home)) //nolint:gosec // our own record
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r forwardRecord
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", forwardRecordFile(home), err)
	}
	if r.To == "" {
		return nil, nil
	}
	return &r, nil
}

func marshalForwardRecord(r *forwardRecord) ([]byte, error) {
	data, err := yaml.Marshal(r)
	if err != nil {
		return nil, err
	}
	header := "# brain forwarding record — this machine gave the brain host role to 'to'.\n" +
		"# Clients that still point here are told so and follow it (after verifying it). Kept until the move is rolled back.\n"
	return append([]byte(header), data...), nil
}

// activeForward is the forwarding record this machine should answer with, or
// nil. A record is stale — and ignored — if this machine has since become the
// host again at a newer generation.
func activeForward(home string) *forwardRecord {
	rec, err := loadForwardRecord(home)
	if err != nil || rec == nil {
		return nil
	}
	if cfg, cerr := loadHostConfig(home); cerr == nil && cfg != nil && (cfg.Role == roleHost || cfg.Role == roleFrozen) && cfg.Generation >= rec.Generation {
		return nil
	}
	return rec
}

// movedNotice is the redirect a moved host answers a client with. On the wire
// it is one stderr line; the first word after the marker is the target (which
// is all PR #29-era clients read), the rest is key=value facts for verification
// and a sentence for people.
type movedNotice struct {
	Target     string
	PromoteID  string
	Generation int
	From       string
	At         string
}

func (n movedNotice) line() string {
	return fmt.Sprintf("%s%s promote=%s generation=%d from=%s at=%s — this machine is no longer the brain host. Point this client at it: brain remote set %s",
		hostMovedMarker, n.Target, n.PromoteID, n.Generation, n.From, n.At, n.Target)
}

func noticeFromRecord(r *forwardRecord) movedNotice {
	return movedNotice{Target: r.To, PromoteID: r.PromoteID, Generation: r.Generation, From: r.FromMachine, At: shortDate(r.MovedAt)}
}

func parseMovedNotice(line []byte) *movedNotice {
	s := strings.TrimRight(string(line), "\r\n")
	if !strings.HasPrefix(s, hostMovedMarker) {
		return nil
	}
	rest := strings.TrimPrefix(s, hostMovedMarker)
	if i := strings.Index(rest, " — "); i >= 0 {
		rest = rest[:i]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return nil
	}
	n := &movedNotice{Target: fields[0]}
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "promote":
			n.PromoteID = v
		case "generation":
			n.Generation, _ = strconv.Atoi(v)
		case "from":
			n.From = v
		case "at":
			n.At = v
		}
	}
	return n
}

// complete reports whether the notice carries what verification needs. A notice
// from an older host (target only) is not enough to verify and is not followed.
func (n *movedNotice) complete() bool {
	return n.Target != "" && validPromoteID(n.PromoteID) && n.Generation > 0 && n.From != ""
}

// followMove walks the forwarding records from first to the machine that really
// holds the role, verifying every hop. It returns the final ssh target and the
// hops it crossed, or an error saying what could not be verified.
func followMove(ctx context.Context, cfg *remoteConfig, first *movedNotice) (string, []movedNotice, error) {
	if !first.complete() {
		return "", nil, fmt.Errorf("the host %s answered that it moved to %q, but its answer lacks the promotion id, generation and sender that would let this client verify the move (it runs an older brain)", cfg.Host, first.Target)
	}
	opts := append([]string{}, cfg.SSHOptions...)
	if v := os.Getenv(envFollowAcceptNew); v != "" && v != "0" {
		opts = append(opts, "StrictHostKeyChecking=accept-new")
	}
	visitedTargets := map[string]bool{cfg.Host: true}
	visitedMachines := map[string]bool{first.From: true}
	hops := []movedNotice{*first}
	cur := *first
	for {
		if len(hops) > maxFollowHops {
			return "", hops, fmt.Errorf("the chain of moves is longer than %d hops; refusing to follow it", maxFollowHops)
		}
		if !remoteHostRE.MatchString(cur.Target) || strings.HasPrefix(cur.Target, "-") {
			return "", hops, fmt.Errorf("%q is not a valid ssh target", cur.Target)
		}
		if visitedTargets[cur.Target] {
			return "", hops, fmt.Errorf("the moves form a loop (%s was already visited)", cur.Target)
		}
		visitedTargets[cur.Target] = true

		hctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		hello, st, err := followProbe(hctx, cur.Target, opts)
		cancel()
		if err != nil {
			return "", hops, fmt.Errorf("%s could not be verified: %w\n  (if this is a host key problem, run 'ssh %s true' once to accept the key, or set %s=1 to accept it automatically)", cur.Target, hostFirstLineErr(err), cur.Target, envFollowAcceptNew)
		}
		if visitedMachines[hello.MachineID] {
			return "", hops, fmt.Errorf("the moves lead back to a machine already visited (%s): a loop", cur.Target)
		}
		visitedMachines[hello.MachineID] = true
		if st.Host == nil {
			return "", hops, fmt.Errorf("%s is not a brain host (it has no host record)", cur.Target)
		}
		if err := lineageContains(st.Host.Lineage, hops); err != nil {
			return "", hops, fmt.Errorf("%s does not confirm the move: %w", cur.Target, err)
		}
		switch st.Host.Role {
		case roleHost, roleFrozen:
			if st.Host.Generation < cur.Generation {
				return "", hops, fmt.Errorf("%s is at generation %d, older than the move named (%d)", cur.Target, st.Host.Generation, cur.Generation)
			}
			return cur.Target, hops, nil
		case roleMoved:
			if st.Forward == nil {
				return "", hops, fmt.Errorf("%s says it moved on but holds no forwarding record", cur.Target)
			}
			next := noticeFromRecord(st.Forward)
			if !next.complete() {
				return "", hops, fmt.Errorf("the forwarding record on %s is incomplete", cur.Target)
			}
			hops = append(hops, next)
			cur = next
		default:
			return "", hops, fmt.Errorf("%s is not a brain host (role %q)", cur.Target, st.Host.Role)
		}
	}
}

func hostFirstLineErr(err error) error {
	return errors.New(hostFirstLine(err.Error()))
}

// followProbe asks the machine at target who it is and what role it holds. It
// is a variable so tests can stand in for the network.
var followProbe = sshFollowProbe

func sshFollowProbe(ctx context.Context, target string, sshOptions []string) (*peerHelloResp, *peerStatusResp, error) {
	conn, err := newPeerConn(target, sshOptions)
	if err != nil {
		return nil, nil, err
	}
	hello, err := conn.hello(ctx, "", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("it is not reachable, or is not a brain host that supports 'brain host': %w", err)
	}
	var st peerStatusResp
	if err := conn.call(ctx, "status", peerStatusReq{}, &st); err != nil {
		return nil, nil, fmt.Errorf("asking for its status: %w", err)
	}
	return hello, &st, nil
}

// lineageContains requires every hop to appear in lineage, in order, with the
// same promotion id, generation and sending machine.
func lineageContains(lineage []lineageEntry, hops []movedNotice) error {
	idx := 0
	for _, h := range hops {
		found := false
		for ; idx < len(lineage); idx++ {
			e := lineage[idx]
			if e.PromoteID == h.PromoteID && e.Generation == h.Generation && e.From == h.From {
				found = true
				idx++
				break
			}
		}
		if !found {
			return fmt.Errorf("its lineage does not contain promotion %s (generation %d) from the machine that named it", h.PromoteID, h.Generation)
		}
	}
	return nil
}
