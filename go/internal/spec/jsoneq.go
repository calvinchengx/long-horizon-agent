package spec

import (
	"encoding/json"
	"reflect"
	"testing"
)

// JSONEqual fails the test unless got (any Go value) marshals to JSON that is semantically equal
// to want: same keys, same values, numbers compared by value (Python writes 0.0 where Go writes 0,
// which is the same JSON number).
func JSONEqual(t testing.TB, label string, got any, want json.RawMessage) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	var g, w any
	if err := json.Unmarshal(gotJSON, &g); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s:\n got  %s\n want %s", label, gotJSON, want)
	}
}
