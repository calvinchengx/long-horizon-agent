package pyfmt

import (
	"encoding/json"
	"testing"
)

func TestSlicingCountsCodePoints(t *testing.T) {
	s := "aé🙂b"
	if RuneLen(s) != 4 || Head(s, 2) != "aé" || Tail(s, 2) != "🙂b" || Tail(s, 10) != s || Head(s, 0) != "" {
		t.Fatal("code-point slicing")
	}
	if PyStrip("\x1c  x \t\n") != "x" {
		t.Fatal("strip")
	}
}

func TestReprMatchesPython(t *testing.T) {
	v, err := DecodeOrdered([]byte(`{"b": [1, 2.5, 1e16, 0.0001, 1e-05, true, null, "it's"], "a": {"x": "\"q\""}, "b2": 10.0}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{'b': [1, 2.5, 1e+16, 0.0001, 1e-05, True, None, "it's"], 'a': {'x': '"q"'}, 'b2': 10.0}`
	if got := PyReprValue(v); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if PyStr("plain") != "plain" || PyStr([]string{"a"}) != "['a']" {
		t.Fatal("str")
	}
	// Plain Go maps have no order: keys are sorted.
	if got := PyReprValue(map[string]any{"z": 1, "a": false}); got != "{'a': False, 'z': 1}" {
		t.Fatal(got)
	}
}

func TestOrderedMapJSON(t *testing.T) {
	m := NewOrderedMap("path", map[string]any{"type": "string"}, "content", "x")
	data, err := json.Marshal(map[string]any{"properties": m})
	if err != nil || string(data) != `{"properties":{"path":{"type":"string"},"content":"x"}}` {
		t.Fatalf("%s %v", data, err)
	}
	if PyReprValue(m) != "{'path': {'type': 'string'}, 'content': 'x'}" {
		t.Fatal(PyReprValue(m))
	}
	if _, err := DecodeOrdered([]byte(`{"a": 1} trailing`)); err == nil {
		t.Fatal("trailing data accepted")
	}
}
