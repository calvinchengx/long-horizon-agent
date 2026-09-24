package pyval

import (
	"encoding/json"
	"testing"
)

// The expected strings were recorded from CPython 3.12.
func TestDecodeUTF8MatchesCPython(t *testing.T) {
	for in, want := range map[string]string{
		"ab\xe2\x82":   "'utf-8' codec can't decode bytes in position 2-3: unexpected end of data",
		"a\xe2\x28":    "'utf-8' codec can't decode byte 0xe2 in position 1: invalid continuation byte",
		"\xff":         "'utf-8' codec can't decode byte 0xff in position 0: invalid start byte",
		"\xf0\x90\x28": "'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte",
		"\xed\xa0\x80": "'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte",
	} {
		if _, err := DecodeUTF8([]byte(in)); err == nil || err.Error() != want {
			t.Errorf("%q: %v", in, err)
		}
	}
	for in, want := range map[string]string{
		"ab\xe2\x82":        "ab�",
		"a\xe2\x28b":        "a�(b",
		"\xf0\x90\x28\xff":  "�(�",
		"ok é":              "ok é",
		"\xc0\xaf":          "��",
		"\xf4\x90\x80\x80x": "����x",
	} {
		if got := DecodeUTF8Replace([]byte(in)); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestReprAndTypes(t *testing.T) {
	v := map[string]any{"b": []any{"x'y", 1.0, 1.5, true, nil}, "a": json.Number("3"), "c": json.Number("2.0")}
	if got := Repr(v); got != `{'a': 3, 'b': ["x'y", 1, 1.5, True, None], 'c': 2.0}` {
		t.Fatal(got)
	}
	if got := JSONDumps(map[string]any{"tool": "t", "arguments": map[string]any{"é": []any{1.0, "\n"}}}, true); got != "{\"arguments\":{\"\\u00e9\":[1,\"\\n\"]},\"tool\":\"t\"}" {
		t.Fatal(got)
	}
	for v, want := range map[any]string{nil: "NoneType", true: "bool", "s": "str", 1.0: "int", 1.5: "float",
		json.Number("1e3"): "float", json.Number("7"): "int"} {
		if got := TypeName(v); got != want {
			t.Errorf("TypeName(%v) = %s", v, got)
		}
	}
	if !Equal(true, 1.0) || Equal("1", 1.0) || !Equal([]any{1.0, nil}, []any{json.Number("1"), nil}) {
		t.Fatal("Equal")
	}
	if got := SplitLines("a\r\nb\rc\x1cd e\n"); len(got) != 5 || got[4] != "e" {
		t.Fatal(got)
	}
	if Head("héllo", 2) != "hé" || Tail("héllo", 4) != "éllo" || Len("héllo") != 5 {
		t.Fatal("slicing")
	}
	if FloatRepr(1e16) != "1e+16" || FloatRepr(0.0001) != "0.0001" || FloatRepr(1.5e-05) != "1.5e-05" || FloatRepr(2) != "2.0" {
		t.Fatal(FloatRepr(1e16), FloatRepr(1.5e-05))
	}
}
