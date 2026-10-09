package main

import "testing"

func TestUnifyCommandCanRunWithoutStore(t *testing.T) {
	// 'brain unify' reads and writes through the Dolt server, never through
	// the caller's local store. Requiring a local store would only stop the
	// one command whose job is to run when there is no local store.
	if !unifyCommandCanRunWithoutStore(unifyPlanCmd) {
		t.Error("unify plan must be allowed to run without a local store")
	}
	if !unifyCommandCanRunWithoutStore(unifyBuildCmd) {
		t.Error("unify build must be allowed to run without a local store")
	}
	if !unifyCommandCanRunWithoutStore(unifyReplayCmd) {
		t.Error("unify replay must be allowed to run without a local store")
	}
	if !unifyCommandCanRunWithoutStore(unifyVerifyCmd) {
		t.Error("unify verify must be allowed to run without a local store")
	}
}

func TestUnifyCommandCanRunWithoutStoreRejectsOthers(t *testing.T) {
	if unifyCommandCanRunWithoutStore(nil) {
		t.Error("nil command must not be classified as store-free")
	}
	if unifyCommandCanRunWithoutStore(brainUnifyCmd) {
		t.Error("the bare 'unify' group must not be classified as store-free")
	}
	// A different verb that happens to share a name must not inherit the
	// exemption: the check is scoped to the unify parent.
	if unifyCommandCanRunWithoutStore(brainStoresDoctorCmd) {
		t.Error("a command outside 'unify' must not be classified as store-free")
	}
}

func TestUnifyDataDirIsRequired(t *testing.T) {
	// The whole production-safety story rests on the unified database being
	// built somewhere that is not a production data directory, so an absent
	// --data-dir must be refused rather than defaulted.
	if unifyDataDir == "" {
		// The flag has no default, which is the point.
		return
	}
	t.Errorf("unifyDataDir defaults to %q; it must have no default", unifyDataDir)
}

func TestConfigCommandGateIncludesUnify(t *testing.T) {
	// The gate in PersistentPreRunE is a separate path from skipsStoreInit;
	// both have to know about unify or the command still aborts.
	if !configCommandCanRunWithoutStore(unifyPlanCmd, nil) {
		t.Error("the database-discovery gate must let 'unify plan' through")
	}
}
