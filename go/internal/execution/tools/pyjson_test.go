package tools

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/json_errors.json was recorded from CPython 3.12's json.loads: [document, str(error) or
// null when it parses].
func TestPyJSONErrorsMatchCPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/json_errors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][2]*string
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		doc := *c[0]
		got := pyJSONError(doc)
		switch {
		case c[1] == nil && got != nil:
			t.Errorf("%q: unexpected %v", doc, got)
		case c[1] != nil && (got == nil || got.Error() != *c[1]):
			t.Errorf("%q: got %v, want %q", doc, got, *c[1])
		}
	}
	if msg := pyJSONMsg("{not json", nil); msg != "Expecting property name enclosed in double quotes" {
		t.Fatal(msg)
	}
}
