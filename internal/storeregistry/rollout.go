package storeregistry

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Rollout outcomes. A machine is only ever "verified" after the machine itself
// reported the registry epoch and the binary it runs.
const (
	RolloutVerified    = "verified"
	RolloutApplied     = "applied"
	RolloutFailed      = "failed"
	RolloutRefused     = "refused"
	RolloutUnreachable = "unreachable"
	RolloutRolledBack  = "rolled-back"
)

// Rollout is what the last 'brain rollout' run established about one machine:
// the binary it runs, the state of its wrapper pin, the registry epoch it was
// proved at and when. It is a record of an operator action, written by the
// machine that runs the rollout, which is why it does not live in
// brain_registry_machines: that table is each machine's own report, replaced
// whole (REPLACE INTO) by every sync, so an extra column would be wiped by the
// next sync or by any older binary.
type Rollout struct {
	Machine        string `json:"machine"`
	Role           string `json:"role"` // direct | client | unknown
	BdVersion      string `json:"bd_version"`
	PinState       string `json:"pin_state"` // ok | absent | stale | unreadable
	PinWant        string `json:"pin_want"`
	BinaryPath     string `json:"binary_path"`
	PreviousBinary string `json:"previous_binary"` // kept on the machine for rollback; "" if none was replaced
	Epoch          int64  `json:"epoch"`
	Outcome        string `json:"outcome"`
	Step           string `json:"step"` // the step that produced the outcome
	Detail         string `json:"detail"`
	Actor          string `json:"actor"`
	VerifiedAt     string `json:"verified_at"` // "" until a run verified the machine
	UpdatedAt      string `json:"updated_at"`
}

// RolloutDDL is created on the first rollout write, like the registry tables.
const RolloutDDL = `CREATE TABLE IF NOT EXISTS brain_registry_rollouts (
  machine varchar(128) NOT NULL,
  role varchar(8) NOT NULL,
  bd_version varchar(64) NOT NULL,
  pin_state varchar(16) NOT NULL,
  pin_want varchar(64) NOT NULL,
  binary_path varchar(512) NOT NULL,
  previous_binary varchar(512) NOT NULL,
  epoch bigint NOT NULL,
  outcome varchar(16) NOT NULL,
  step varchar(16) NOT NULL,
  detail text NOT NULL,
  actor varchar(128) NOT NULL,
  verified_at varchar(32) NOT NULL,
  updated_at datetime NOT NULL,
  PRIMARY KEY (machine)
)`

// ReportRollout upserts one machine's rollout record. It never touches the
// registry epoch or any other registry table. A record without a VerifiedAt
// keeps the machine's earlier VerifiedAt, and one without a PreviousBinary keeps
// the earlier one: a failed run does not erase when the machine was last proved
// or which kept binary a rollback would restore.
func ReportRollout(ctx context.Context, db DBTX, r Rollout, at time.Time) error {
	if _, err := db.ExecContext(ctx, RolloutDDL); err != nil {
		return fmt.Errorf("provisioning the rollout record: %w", err)
	}
	if r.VerifiedAt == "" || r.PreviousBinary == "" {
		var verified, previous sql.NullString
		err := db.QueryRowContext(ctx, "SELECT verified_at, previous_binary FROM brain_registry_rollouts WHERE machine = ?", r.Machine).Scan(&verified, &previous)
		if err == nil {
			if r.VerifiedAt == "" {
				r.VerifiedAt = verified.String
			}
			if r.PreviousBinary == "" {
				r.PreviousBinary = previous.String
			}
		}
	}
	if _, err := db.ExecContext(ctx,
		"REPLACE INTO brain_registry_rollouts (machine, role, bd_version, pin_state, pin_want, binary_path, previous_binary, epoch, outcome, step, detail, actor, verified_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.Machine, r.Role, r.BdVersion, r.PinState, r.PinWant, r.BinaryPath, r.PreviousBinary, r.Epoch,
		r.Outcome, r.Step, r.Detail, r.Actor, r.VerifiedAt, at.UTC().Format(timeFmt)); err != nil {
		return fmt.Errorf("recording rollout of %q: %w", r.Machine, err)
	}
	return nil
}

// ReadRollouts returns every machine's record, sorted by machine. A database
// that never saw a rollout reads as empty.
func ReadRollouts(ctx context.Context, q DBTX) ([]Rollout, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT machine, role, bd_version, pin_state, pin_want, binary_path, previous_binary, epoch, outcome, step, detail, actor, verified_at, CAST(updated_at AS CHAR) FROM brain_registry_rollouts ORDER BY machine")
	if err != nil {
		if isNoTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading rollout records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Rollout
	for rows.Next() {
		var r Rollout
		if err := rows.Scan(&r.Machine, &r.Role, &r.BdVersion, &r.PinState, &r.PinWant, &r.BinaryPath, &r.PreviousBinary,
			&r.Epoch, &r.Outcome, &r.Step, &r.Detail, &r.Actor, &r.VerifiedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
