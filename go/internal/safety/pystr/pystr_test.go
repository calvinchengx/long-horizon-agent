package pystr

import "testing"

// Expected values are CPython 3.12's str methods.

func TestLower(t *testing.T) {
	for in, want := range map[string]string{
		"ABC":                 "abc",
		"\u0391\u03a3":        "\u03b1\u03c2",        // final sigma
		"\u03a3":              "\u03c3",              // no preceding cased letter
		"\u0391\u03a3 \u0391": "\u03b1\u03c2 \u03b1", // followed by a space
		"\u0391\u03a3\u0391":  "\u03b1\u03c3\u03b1",
		"\u0391.\u03a3":       "\u03b1.\u03c2", // "." is case-ignorable
		"\u0391\u03a3.":       "\u03b1\u03c2.",
		"\u0386\u03a3":        "\u03ac\u03c2",
		"\u0130":              "i\u0307",
		"\u01c5":              "\u01c6",
		"\u212a":              "k",
		"G\u0130T":            "gi\u0307t",
	} {
		if got := Lower(in); got != want {
			t.Errorf("Lower(%+q) = %+q, want %+q", in, got, want)
		}
	}
}

func TestUpperCasefold(t *testing.T) {
	for in, want := range map[string]string{
		"post": "POST", "ß": "SS", "po\ufb06": "POST", "\u0131": "I", "\u017f": "S", "\u01c5": "\u01c4",
	} {
		if got := Upper(in); got != want {
			t.Errorf("Upper(%+q) = %+q, want %+q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		".GIT": ".git", "ß": "ss", "\u0130": "i\u0307", "\u03c2": "\u03c3", "\u0391\u03a3": "\u03b1\u03c3",
	} {
		if got := Casefold(in); got != want {
			t.Errorf("Casefold(%+q) = %+q, want %+q", in, got, want)
		}
	}
}

func TestPredicates(t *testing.T) {
	if !IsDigit('\u00b2') || IsDecimal('\u00b2') || !IsDecimal('\u0663') || IsDigit('a') {
		t.Error("digit / decimal")
	}
	if !IsDigitString("12\u00b2") || IsDigitString("") || IsDigitString("1a") {
		t.Error("IsDigitString")
	}
	if !IsAlnum('\u216b') || IsAlnum('_') || !IsWord('_') || IsWord('-') {
		t.Error("alnum / word")
	}
	if !IsAlnumString("a1") || IsAlnumString("") || IsAlnumString("a-1") {
		t.Error("IsAlnumString")
	}
	if !IsSpace('\x1c') || !IsSpace('\u00a0') || !IsSpace('\u2028') || IsSpace('\u200b') {
		t.Error("space")
	}
	if got := Strip(" \x1c\u00a0x\u2028 "); got != "x" {
		t.Errorf("Strip = %+q", got)
	}
	if LStrip("  x ") != "x " || RStrip(" x  ") != " x" {
		t.Error("LStrip / RStrip")
	}
	if !IsASCII("") || !IsASCII("abc") || IsASCII("é") {
		t.Error("IsASCII")
	}
	if !HasNFKCURLDelimiter('\u2100') || !HasNFKCURLDelimiter('\uff0f') || HasNFKCURLDelimiter('a') {
		t.Error("HasNFKCURLDelimiter")
	}
}

func TestFoldsToASCIILetter(t *testing.T) {
	for _, c := range []struct {
		r     rune
		lower byte
		want  bool
	}{
		{'a', 'a', true}, {'A', 'a', true}, {'b', 'a', false}, {'@', '`', false},
		{'\u0130', 'i', true}, {'\u0131', 'i', true}, {'\u017f', 's', true}, {'\u212a', 'k', true},
		{'\u212a', 'i', false}, {'\u00e9', 'e', false},
	} {
		if got := FoldsToASCIILetter(c.r, c.lower); got != c.want {
			t.Errorf("FoldsToASCIILetter(%q, %q) = %v", c.r, c.lower, got)
		}
	}
}
