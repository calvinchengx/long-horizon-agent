package contracts

import "testing"

func TestPyRepr(t *testing.T) {
	cases := map[string]string{
		"abc":          `'abc'`,
		"it's":         `"it's"`,
		`both ' and "`: `'both \' and "'`,
		"a\nb\tc":      `'a\nb\tc'`,
		"back\\slash":  `'back\\slash'`,
		"\x00\x7f":     `'\x00\x7f'`,
		"ü✓":           `'ü✓'`,
		"\u2028":       `'\u2028'`,
		"\u00a0":       `'\xa0'`,
		"":             `''`,
	}
	for in, want := range cases {
		if got := PyRepr(in); got != want {
			t.Errorf("PyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}
