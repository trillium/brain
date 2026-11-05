package tracker

import "testing"

// A typed nil map is a non-nil interface that marshals to the JSON literal
// null. Stored as an issue's metadata it made every later metadata merge write
// into a nil map and panic, which crashed pulls from trackers that report no
// metadata.
func TestMarshalTrackerMetadataIgnoresNullAndNil(t *testing.T) {
	var nilMap map[string]interface{}
	for name, in := range map[string]interface{}{
		"untyped nil": nil,
		"typed nil":   nilMap,
	} {
		if raw, ok := marshalTrackerMetadata(in); ok {
			t.Errorf("%s: stored %s as metadata", name, raw)
		}
	}
	raw, ok := marshalTrackerMetadata(map[string]interface{}{"k": "v"})
	if !ok || string(raw) != `{"k":"v"}` {
		t.Fatalf("real metadata: %s, %v", raw, ok)
	}
}
