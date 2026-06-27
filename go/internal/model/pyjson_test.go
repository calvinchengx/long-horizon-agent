package model

import (
	"encoding/json"
	"math"
	"testing"
)

// Expected strings are Python's own output (json.dumps / repr).
func TestPyJSONDumps(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{map[string]any{"path": "a/\u00fc\n\"x\"", "n": []any{1.0, 2.5, nil, true, 1e20, 1.5e-7}},
			`{"n": [1, 2.5, null, true, 1e+20, 1.5e-07], "path": "a/\u00fc\n\"x\""}`},
		{map[string]any{"a": "\u2028\U0001F600\x7f\x01\b\f\r\t\\/"}, `{"a": "\u2028\ud83d\ude00\u007f\u0001\b\f\r\t\\/"}`},
		{map[string]any{}, `{}`},
		{[]any{}, `[]`},
		{nil, `null`},
		{false, `false`},
		{json.Number("12.50"), `12.50`},
		{7, `7`},
		{int64(-9), `-9`},
		{math.NaN(), `NaN`},
		{math.Inf(1), `Infinity`},
		{math.Inf(-1), `-Infinity`},
		{func() float64 { a, b := 0.1, 0.2; return a + b }(), `0.30000000000000004`}, // not constant-folded
		{map[string]string{"k": "v"}, `{"k": "v"}`},
		{struct {
			A int `json:"a"`
		}{3}, `{"a": 3}`},
	}
	for _, c := range cases {
		got, err := pyJSONDumps(c.in)
		if err != nil || got != c.want {
			t.Errorf("pyJSONDumps(%#v) = %s, %v; want %s", c.in, got, err, c.want)
		}
	}
	if _, err := pyJSONDumps(map[string]any{"f": func() {}}); err == nil {
		t.Error("unmarshalable values error")
	}
	if _, err := pyJSONDumps([]any{make(chan int)}); err == nil {
		t.Error("unmarshalable values error")
	}
}

func TestPyFloatRepr(t *testing.T) {
	cases := map[float64]string{
		1.0: "1.0", 1e16: "1e+16", 1.5e-5: "1.5e-05", 123456789.125: "123456789.125",
		0.0001: "0.0001", 1e-5: "1e-05", -2.5e22: "-2.5e+22", 0: "0.0", -0.5: "-0.5",
		1234567890123456.0: "1234567890123456.0", 100: "100.0",
		math.Inf(1): "inf", math.Inf(-1): "-inf",
	}
	for f, want := range cases {
		if got := pyFloatRepr(f); got != want {
			t.Errorf("pyFloatRepr(%v) = %s, want %s", f, got, want)
		}
	}
	if pyFloatRepr(math.NaN()) != "nan" {
		t.Error("nan")
	}
}

func TestPythonValueHelpers(t *testing.T) {
	strs := map[string]any{"None": nil, "True": true, "False": false, "5": json.Number("5"), "5.5": json.Number("5.5"),
		"2.0": 2.0, "x": "x", "['a', 1.0]": []any{"a", 1.0}, "{'k': None}": map[string]any{"k": nil}}
	for want, in := range strs {
		if got := pyStr(in); got != want {
			t.Errorf("pyStr(%#v) = %q, want %q", in, got, want)
		}
	}
	ints := []struct {
		in   any
		want int
	}{{nil, 0}, {false, 0}, {true, 1}, {json.Number("7"), 7}, {json.Number("7.9"), 7}, {9.5, 9}, {" 4 ", 4}, {"", 0}, {[]any{}, 0}}
	for _, c := range ints {
		if got, err := pyIntOr0(c.in); err != nil || got != c.want {
			t.Errorf("pyIntOr0(%#v) = %d, %v", c.in, got, err)
		}
	}
	for in, want := range map[string]string{
		"x": "invalid literal for int() with base 10: 'x'",
	} {
		if _, err := pyIntOr0(in); err == nil || err.Error() != want {
			t.Errorf("pyIntOr0(%q) err = %v", in, err)
		}
	}
	if _, err := pyIntOr0([]any{1}); err == nil || err.Error() != "int() argument must be a string, a bytes-like object or a real number, not 'list'" {
		t.Errorf("err = %v", err)
	}
	if _, err := pyIntOr0(map[string]any{"a": 1}); err == nil {
		t.Error("dict")
	}
	if _, err := pyIntOr0(json.Number("1e999")); err == nil {
		t.Error("inf")
	}
	if pyTypeName(1.5) != "float" || pyTypeName(nil) != "NoneType" || pyTypeName("s") != "str" || pyTypeName(true) != "bool" || pyTypeName(map[string]any{}) != "dict" {
		t.Error("pyTypeName")
	}
	if !pyTruthy(json.Number("0.5")) || pyTruthy(json.Number("0")) || pyTruthy(0.0) || !pyTruthy(struct{}{}) {
		t.Error("pyTruthy")
	}
	if v := plainNumbers([]any{json.Number("1"), map[string]any{"a": json.Number("2.5")}}); v.([]any)[1].(map[string]any)["a"] != 2.5 {
		t.Errorf("plainNumbers = %v", v)
	}
}
