package state

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestPySplitlines(t *testing.T) {
	in := "a\nb\r\nc\rd\ve\x1cf\u2028g\x1fh"
	if got := pySplitlines(in, false); !slices.Equal(got, []string{"a", "b", "c", "d", "e", "f", "g\x1fh"}) {
		t.Fatalf("%q", got)
	}
	if got := pySplitlines("x\r\ny\n", true); !slices.Equal(got, []string{"x\r\n", "y\n"}) {
		t.Fatalf("%q", got)
	}
	if got := pySplitlines("", false); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestPyStripAndNewlines(t *testing.T) {
	if got := pyStrip("\x1c\u00a0 x \x1f\n"); got != "x" {
		t.Fatalf("%q", got)
	}
	if got := universalNewlines("a\r\nb\rc\n"); got != "a\nb\nc\n" {
		t.Fatalf("%q", got)
	}
}

func TestPyFloatRepr(t *testing.T) {
	cases := map[float64]string{
		120: "120.0", 0.5: "0.5", 1e-9: "1e-09", 1e16: "1e+16", 1234567: "1234567.0",
		1.5e-5: "1.5e-05", 0.0001: "0.0001", 123456789012345680: "1.2345678901234568e+17",
		math.Inf(1): "inf", math.Inf(-1): "-inf", math.NaN(): "nan",
	}
	for f, want := range cases {
		if got := pyFloatRepr(f); got != want {
			t.Errorf("pyFloatRepr(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestPydanticJSON(t *testing.T) {
	v := map[string]any{"s": "<a&b> \u2028\u2029 \\u003c ü", "l": []any{}, "o": map[string]any{}}
	got, err := pydanticJSON(v, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"l":[],"o":{},"s":"<a&b> ` + "\u2028\u2029" + ` \\u003c ü"}`
	if string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
	var back map[string]any
	if err := json.Unmarshal(got, &back); err != nil || back["s"] != v["s"] {
		t.Fatalf("round trip: %v %v", back, err)
	}
	ind, err := pydanticJSON(v, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ind), "\n  \"l\": [],\n  \"o\": {},") {
		t.Fatalf("%s", ind)
	}
}
