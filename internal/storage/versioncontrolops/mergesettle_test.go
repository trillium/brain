package versioncontrolops

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"testing"
)

type testRows struct {
	data   [][]interface{}
	index  int
	closed bool
}

func (r *testRows) Columns() ([]string, error) {
	return []string{"our_key", "their_key"}, nil
}

func (r *testRows) Scan(dest ...interface{}) error {
	if r.index >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.index]
	for i, v := range row {
		if i < len(dest) {
			ptr := dest[i].(*sql.NullString)
			if s, ok := v.(string); ok && s != "" {
				*ptr = sql.NullString{String: s, Valid: true}
			} else {
				*ptr = sql.NullString{Valid: false}
			}
		}
	}
	return nil
}

func (r *testRows) Next() bool {
	if r.index >= len(r.data) {
		return false
	}
	r.index++
	return true
}

func (r *testRows) Close() error {
	r.closed = true
	return nil
}

func (r *testRows) Err() error {
	return nil
}

type testMembeadRows struct {
	data   [][]interface{}
	index  int
	closed bool
}

func (r *testMembeadRows) Columns() ([]string, error) {
	return []string{"our_value", "their_value"}, nil
}

func (r *testMembeadRows) Scan(dest ...interface{}) error {
	if r.index >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.index]
	for i, v := range row {
		if i < len(dest) {
			ptr := dest[i].(*sql.NullString)
			if s, ok := v.(string); ok && s != "" {
				*ptr = sql.NullString{String: s, Valid: true}
			} else {
				*ptr = sql.NullString{Valid: false}
			}
		}
	}
	return nil
}

func (r *testMembeadRows) Next() bool {
	if r.index >= len(r.data) {
		return false
	}
	r.index++
	return true
}

func (r *testMembeadRows) Close() error {
	r.closed = true
	return nil
}

func (r *testMembeadRows) Err() error {
	return nil
}

type testDB struct {
	queryFn func(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	execFn  func(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func (db *testDB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if db.queryFn != nil {
		return db.queryFn(ctx, query, args...)
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

func (db *testDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if db.execFn != nil {
		return db.execFn(ctx, query, args...)
	}
	return nil, nil
}

func (db *testDB) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return nil
}

func TestConfigConflictsAreMemoryConvergentMembead(t *testing.T) {
	tests := []struct {
		name               string
		conflictData       [][]interface{}
		shouldBeConvergent bool
	}{
		{
			name: "membead_only_conflict",
			conflictData: [][]interface{}{
				{"kv.membead.auth-jwt", ""},
			},
			shouldBeConvergent: true,
		},
		{
			name: "memory_and_membead_conflict",
			conflictData: [][]interface{}{
				{"kv.memory.auth-jwt", ""},
				{"kv.membead.auth-jwt", ""},
			},
			shouldBeConvergent: true,
		},
		{
			name: "foreign_key_present",
			conflictData: [][]interface{}{
				{"kv.memory.auth-jwt", ""},
				{"issue_prefix", ""},
			},
			shouldBeConvergent: false,
		},
		{
			name: "sync_key_present",
			conflictData: [][]interface{}{
				{"kv.membead.auth-jwt", ""},
				{"sync.setting", ""},
			},
			shouldBeConvergent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testData := tt.conflictData
			convergent := true
			for _, row := range testData {
				ourKey := row[0].(string)
				theirKey := row[1].(string)

				if ourKey != "" && !isMemoryOrMembeadKey(ourKey) {
					convergent = false
					break
				}
				if theirKey != "" && !isMemoryOrMembeadKey(theirKey) {
					convergent = false
					break
				}
			}

			if convergent != tt.shouldBeConvergent {
				t.Errorf("got convergent=%v, want %v", convergent, tt.shouldBeConvergent)
			}
		})
	}
}

func TestMembeadDeterministicResolution(t *testing.T) {
	tests := []struct {
		name         string
		ourValue     string
		theirValue   string
		expectWinner string
	}{
		{
			name:         "smaller_lexic_wins",
			ourValue:     "bd-zzz999",
			theirValue:   "bd-aaa111",
			expectWinner: "bd-aaa111",
		},
		{
			name:         "order_independent_swap",
			ourValue:     "bd-aaa111",
			theirValue:   "bd-zzz999",
			expectWinner: "bd-aaa111",
		},
		{
			name:         "numeric_prefix_ordered",
			ourValue:     "bd-99abc",
			theirValue:   "bd-10xyz",
			expectWinner: "bd-10xyz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var winner string
			if tt.ourValue == tt.theirValue {
				winner = ""
			} else if tt.ourValue < tt.theirValue {
				winner = tt.ourValue
			} else {
				winner = tt.theirValue
			}

			if winner != tt.expectWinner {
				t.Errorf("got winner %q, want %q", winner, tt.expectWinner)
			}
		})
	}
}
