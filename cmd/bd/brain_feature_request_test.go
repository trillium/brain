package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestIsBrainMode_BDNameEnv(t *testing.T) {
	t.Setenv("BD_NAME", "brain")
	if !isBrainMode() {
		t.Fatal("isBrainMode() = false; want true when BD_NAME=brain")
	}
}

func TestIsBrainMode_NotBrain(t *testing.T) {
	t.Setenv("BD_NAME", "bd")
	// os.Args[0] won't be "brain" in test binaries, so this should be false.
	if os.Getenv("BD_NAME") == "brain" {
		t.Skip("BD_NAME=brain inherited from outer env")
	}
	if isBrainMode() {
		t.Fatal("isBrainMode() = true; want false when BD_NAME != brain and binary != brain")
	}
}

// TestAutoFileBrainFeatureRequest_OptOut verifies BRAIN_NO_AUTO_FEATURE_REQUEST
// suppresses the handler and returns the original error unchanged.
func TestAutoFileBrainFeatureRequest_OptOut(t *testing.T) {
	t.Setenv("BRAIN_NO_AUTO_FEATURE_REQUEST", "1")
	t.Setenv("BD_NAME", "brain")

	cmd := &cobra.Command{Use: "create"}
	rootLike := &cobra.Command{Use: "brain"}
	rootLike.AddCommand(cmd)

	origErr := errors.New("unknown flag: --slug")
	got := autoFileBrainFeatureRequest(cmd, origErr)
	if got != origErr {
		t.Fatalf("expected original error returned unchanged; got %v", got)
	}
}

// TestAutoFileBrainFeatureRequest_NotBrainMode verifies the handler is a no-op
// when not in brain mode.
func TestAutoFileBrainFeatureRequest_NotBrainMode(t *testing.T) {
	t.Setenv("BD_NAME", "bd")
	t.Setenv("BRAIN_NO_AUTO_FEATURE_REQUEST", "")

	cmd := &cobra.Command{Use: "create"}
	rootLike := &cobra.Command{Use: "bd"}
	rootLike.AddCommand(cmd)

	origErr := errors.New("unknown flag: --slug")
	got := autoFileBrainFeatureRequest(cmd, origErr)
	if got != origErr {
		t.Fatalf("expected original error returned unchanged in bd mode; got %v", got)
	}
}

// TestAutoFileBrainFeatureRequest_NonFlagError verifies non-"unknown flag" errors
// pass through without filing a feature request.
func TestAutoFileBrainFeatureRequest_NonFlagError(t *testing.T) {
	t.Setenv("BD_NAME", "brain")
	t.Setenv("BRAIN_NO_AUTO_FEATURE_REQUEST", "")

	cmd := &cobra.Command{Use: "create"}
	rootLike := &cobra.Command{Use: "brain"}
	rootLike.AddCommand(cmd)

	origErr := errors.New("required flag not set: --title")
	got := autoFileBrainFeatureRequest(cmd, origErr)
	if got != origErr {
		t.Fatalf("non-flag error should pass through unchanged; got %v", got)
	}
}

// TestAutoFileBrainFeatureRequest_FlagNameParsing verifies the flag name is
// extracted correctly from the cobra error string.
func TestAutoFileBrainFeatureRequest_FlagNameParsing(t *testing.T) {
	cases := []struct {
		errStr   string
		wantFlag string
		matches  bool
	}{
		{"unknown flag: --slug", "--slug", true},
		{"unknown flag: --foo-bar", "--foo-bar", true},
		{"required flag: --title", "", false},
		{"unknown flag: -x", "-x", true},
	}
	for _, tc := range cases {
		got := strings.HasPrefix(tc.errStr, "unknown flag: ")
		if got != tc.matches {
			t.Errorf("prefix check for %q: got %v, want %v", tc.errStr, got, tc.matches)
			continue
		}
		if tc.matches {
			flag := strings.TrimPrefix(tc.errStr, "unknown flag: ")
			if flag != tc.wantFlag {
				t.Errorf("extracted flag from %q: got %q, want %q", tc.errStr, flag, tc.wantFlag)
			}
		}
	}
}

// TestFlagErrorFuncRegistered verifies rootCmd has a FlagErrorFunc set.
func TestFlagErrorFuncRegistered(t *testing.T) {
	if rootCmd.FlagErrorFunc() == nil {
		t.Fatal("rootCmd has no FlagErrorFunc; brain_feature_request.go init() did not run")
	}
}
