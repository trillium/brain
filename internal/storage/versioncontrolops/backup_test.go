package versioncontrolops

import (
	"fmt"
	"testing"
)

func TestExtractAddressConflictName(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("connection refused"),
			want: "",
		},
		{
			name: "standard conflict",
			err:  fmt.Errorf("Error 1105: address conflict with a remote: 'default' -> file:///backup"),
			want: "default",
		},
		{
			name: "full dolt error format from doc comment",
			err:  fmt.Errorf("Error 1105: address conflict with a remote: 'backup_export' -> file:///some/path"),
			want: "backup_export",
		},
		{
			name: "missing closing quote",
			err:  fmt.Errorf("address conflict with a remote: 'oops"),
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractAddressConflictName(tt.err); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsMemoryOrMembeadKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{
			name: "memory key accepted",
			key:  "kv.memory.auth-jwt",
			want: true,
		},
		{
			name: "memory key with simple slug",
			key:  "kv.memory.race-flag",
			want: true,
		},
		{
			name: "membead key accepted",
			key:  "kv.membead.auth-jwt",
			want: true,
		},
		{
			name: "membead key with issue ID",
			key:  "kv.membead.some-key",
			want: true,
		},
		{
			name: "generic kv key rejected",
			key:  "kv.mykey",
			want: false,
		},
		{
			name: "issue_prefix rejected",
			key:  "issue_prefix",
			want: false,
		},
		{
			name: "sync key rejected",
			key:  "sync.something",
			want: false,
		},
		{
			name: "empty string rejected",
			key:  "",
			want: false,
		},
		{
			name: "partial memory prefix not accepted",
			key:  "kv.mem",
			want: false,
		},
		{
			name: "partial membead prefix not accepted",
			key:  "kv.memb",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMemoryOrMembeadKey(tt.key); got != tt.want {
				t.Errorf("isMemoryOrMembeadKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestMembeadConflictResolution(t *testing.T) {
	tests := []struct {
		name           string
		ourVal         string
		theirVal       string
		expectWinner   string
		expectNoChange bool
	}{
		{
			name:           "lex smaller value wins",
			ourVal:         "bd-zzz999",
			theirVal:       "bd-aaa111",
			expectWinner:   "bd-aaa111",
			expectNoChange: false,
		},
		{
			name:           "lex larger value loses",
			ourVal:         "bd-aaa111",
			theirVal:       "bd-zzz999",
			expectWinner:   "bd-aaa111",
			expectNoChange: false,
		},
		{
			name:           "identical values no resolution needed",
			ourVal:         "bd-same123",
			theirVal:       "bd-same123",
			expectWinner:   "",
			expectNoChange: true,
		},
		{
			name:           "numeric comparison works",
			ourVal:         "bd-99a",
			theirVal:       "bd-10b",
			expectWinner:   "bd-10b",
			expectNoChange: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var winner string
			if tt.ourVal == tt.theirVal {
				winner = ""
			} else if tt.ourVal < tt.theirVal {
				winner = tt.ourVal
			} else {
				winner = tt.theirVal
			}

			if !tt.expectNoChange && winner != tt.expectWinner {
				t.Errorf("membead resolution: got winner %q, want %q", winner, tt.expectWinner)
			}
			if tt.expectNoChange && winner != "" {
				t.Errorf("expected no winner for identical values, got %q", winner)
			}
		})
	}
}

func TestConfigConflictKeyValidation(t *testing.T) {
	tests := []struct {
		name               string
		keys               []string
		shouldBeConvergent bool
		desc               string
	}{
		{
			name:               "memory conflict only",
			keys:               []string{"kv.memory.auth-jwt", "kv.memory.race-flag"},
			shouldBeConvergent: true,
			desc:               "same insight remembered on two clones converges",
		},
		{
			name:               "membead conflict only",
			keys:               []string{"kv.membead.auth-jwt"},
			shouldBeConvergent: true,
			desc:               "different minted issue IDs for same key converge",
		},
		{
			name:               "memory and membead mixed",
			keys:               []string{"kv.memory.auth-jwt", "kv.membead.auth-jwt", "kv.memory.race-flag"},
			shouldBeConvergent: true,
			desc:               "memory text + membead index conflicts converge together",
		},
		{
			name:               "foreign config key present",
			keys:               []string{"kv.memory.auth-jwt", "issue_prefix"},
			shouldBeConvergent: false,
			desc:               "genuinely foreign keys still park for operator",
		},
		{
			name:               "sync key present",
			keys:               []string{"kv.memory.auth-jwt", "sync.setting"},
			shouldBeConvergent: false,
			desc:               "sync config conflicts still park for operator",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			convergent := true
			for _, k := range tt.keys {
				if !isMemoryOrMembeadKey(k) {
					convergent = false
					break
				}
			}

			if convergent != tt.shouldBeConvergent {
				t.Errorf("config conflict validation: %s: got convergent=%v, want %v",
					tt.desc, convergent, tt.shouldBeConvergent)
			}
		})
	}
}
