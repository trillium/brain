package versioncontrolops

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestConfigConflictsAreMemoryConvergent(t *testing.T) {
	tests := []struct {
		name          string
		conflictKeys  [][]string
		wantConvergent bool
		desc          string
	}{
		{
			name: "membead_only_converges",
			conflictKeys: [][]string{
				{"kv.membead.auth-jwt", ""},
			},
			wantConvergent: true,
			desc:           "membead-only conflicts should converge",
		},
		{
			name: "memory_only_converges",
			conflictKeys: [][]string{
				{"kv.memory.auth-jwt", ""},
			},
			wantConvergent: true,
			desc:           "memory-only conflicts should converge",
		},
		{
			name: "mixed_memory_membead_converges",
			conflictKeys: [][]string{
				{"kv.memory.auth-jwt", ""},
				{"kv.membead.auth-jwt", ""},
			},
			wantConvergent: true,
			desc:           "mixed memory + membead conflicts should converge",
		},
		{
			name: "foreign_key_prevents_convergence",
			conflictKeys: [][]string{
				{"kv.membead.auth-jwt", ""},
				{"issue_prefix", ""},
			},
			wantConvergent: false,
			desc:           "foreign config key prevents convergence",
		},
		{
			name: "empty_converges",
			conflictKeys: [][]string{},
			wantConvergent: true,
			desc:           "empty conflict set should converge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })

			rows := sqlmock.NewRows([]string{"our_key", "their_key"})
			for _, keys := range tt.conflictKeys {
				ourKey := keys[0]
				theirKey := keys[1]
				var ourVal interface{} = ourKey
				var theirVal interface{} = theirKey
				if ourKey == "" {
					ourVal = nil
				}
				if theirKey == "" {
					theirVal = nil
				}
				rows = rows.AddRow(ourVal, theirVal)
			}

			mock.ExpectQuery("SELECT our_key, their_key FROM dolt_conflicts_config").
				WillReturnRows(rows)

			got, err := configConflictsAreMemoryConvergent(context.Background(), db)
			if err != nil {
				t.Fatalf("configConflictsAreMemoryConvergent returned error: %v", err)
			}

			if got != tt.wantConvergent {
				t.Errorf("%s: got convergent=%v, want %v", tt.desc, got, tt.wantConvergent)
			}
		})
	}
}

func TestResolveMembeadConflictsDeterministically(t *testing.T) {
	tests := []struct {
		name         string
		ourID        string
		theirID      string
		expectWinner string
		desc         string
	}{
		{
			name:         "smaller_lexic_wins",
			ourID:        "bd-zzz999",
			theirID:      "bd-aaa111",
			expectWinner: "bd-aaa111",
			desc:         "smaller lexicographic ID should win",
		},
		{
			name:         "order_independent",
			ourID:        "bd-aaa111",
			theirID:      "bd-zzz999",
			expectWinner: "bd-aaa111",
			desc:         "winner should be order-independent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })

			rows := sqlmock.NewRows([]string{"our_value", "their_value"}).
				AddRow(tt.ourID, tt.theirID)

			mock.ExpectQuery("SELECT our_value, their_value FROM dolt_conflicts_config").
				WillReturnRows(rows)

			mock.ExpectExec("UPDATE config SET value = \\? WHERE value = \\? OR value = \\?").
				WithArgs(tt.expectWinner, tt.ourID, tt.theirID).
				WillReturnResult(sqlmock.NewResult(0, 1))

			err = resolveMembeadConflictsDeterministically(context.Background(), db)
			if err != nil {
				t.Fatalf("resolveMembeadConflictsDeterministically returned error: %v", err)
			}

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("%s: %v", tt.desc, err)
			}
		})
	}
}
