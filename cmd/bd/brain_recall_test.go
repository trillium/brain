package main

import (
	"reflect"
	"testing"
)

func TestRecallScopeEscapesNamespaceLikeMetacharacters(t *testing.T) {
	got, args := recallScope("id", []string{"a_b", "c%d", "e!f"})
	want := "(id LIKE ? ESCAPE '!' OR id LIKE ? ESCAPE '!' OR id LIKE ? ESCAPE '!')"
	if got != want {
		t.Fatalf("scope = %q, want %q", got, want)
	}
	if want := []any{"a!_b-%", "c!%d-%", "e!!f-%"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
}
