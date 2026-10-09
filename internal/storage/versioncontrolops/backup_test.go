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
