// Package pystr reproduces the CPython str operations the safety boundary depends on.
//
// The command classifier, the egress policy and the redactor compare strings the way the Python
// reference does (str.lower, str.upper, str.casefold, str.strip, str.isdigit, the re module's \s
// and \w). Go's unicode package uses simple case mappings and different character classes, so a
// value such as "İ" (lower-cases to "i" + U+0307 in Python, to "i" in Go) or "²" (a digit to Python, not to
// Go) would otherwise be classified differently. The tables are generated from CPython's
// unicodedata (see gen.go / dump_unicode.py), so behaviour matches the reference exactly for that
// Unicode version.
package pystr

import (
	"strings"
	"unicode"
)

type rng struct{ lo, hi rune }

func inTable(t []rng, r rune) bool {
	lo, hi := 0, len(t)
	for lo < hi {
		mid := (lo + hi) / 2
		switch {
		case r < t[mid].lo:
			hi = mid
		case r > t[mid].hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// IsSpace is str.isspace for one character (also the re module's \s).
func IsSpace(r rune) bool { return inTable(spaceTable, r) }

// IsAlnum is str.isalnum for one character.
func IsAlnum(r rune) bool { return inTable(alnumTable, r) }

// IsDigit is str.isdigit for one character (includes superscripts, circled digits, ...).
func IsDigit(r rune) bool { return inTable(digitTable, r) }

// IsDecimal is str.isdecimal for one character (also the re module's \d).
func IsDecimal(r rune) bool { return inTable(decimalTable, r) }

// IsWord is the re module's \w for str patterns (alphanumeric or underscore).
func IsWord(r rune) bool { return r == '_' || IsAlnum(r) }

// HasNFKCURLDelimiter reports a character whose NFKC form contains one of "/?#@:" (the check
// urllib.parse applies to non-ASCII netlocs).
func HasNFKCURLDelimiter(r rune) bool { return inTable(nfkcURLDelimiterTable, r) }

// IsDigitString is str.isdigit: non-empty and every character a digit.
func IsDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !IsDigit(r) {
			return false
		}
	}
	return true
}

// IsAlnumString is str.isalnum: non-empty and every character alphanumeric.
func IsAlnumString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !IsAlnum(r) {
			return false
		}
	}
	return true
}

// IsASCII is str.isascii (true for the empty string).
func IsASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Lower is str.lower: full case mapping, including the Final_Sigma context rule.
func Lower(s string) string {
	if IsASCII(s) {
		return strings.ToLower(s)
	}
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if r == 0x03A3 {
			b.WriteRune(capitalSigma(rs, i))
			continue
		}
		if m, ok := lowerExceptions[r]; ok {
			b.WriteString(m)
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// capitalSigma mirrors CPython's handle_capital_sigma:
// \p{cased}\p{case-ignorable}* U+03A3 !(\p{case-ignorable}* \p{cased}) lower-cases to ς.
func capitalSigma(rs []rune, i int) rune {
	j := i - 1
	for j >= 0 && inTable(caseIgnorableTable, rs[j]) {
		j--
	}
	final := j >= 0 && inTable(casedNonIgnorableTable, rs[j])
	if final && i < len(rs)-1 {
		j = i + 1
		for j < len(rs) && inTable(caseIgnorableTable, rs[j]) {
			j++
		}
		final = j == len(rs) || !inTable(casedNonIgnorableTable, rs[j])
	}
	if final {
		return 0x03C2
	}
	return 0x03C3
}

// Upper is str.upper (full case mapping, e.g. "ß" -> "SS", "ﬆ" -> "ST").
func Upper(s string) string {
	if IsASCII(s) {
		return strings.ToUpper(s)
	}
	var b strings.Builder
	for _, r := range s {
		if m, ok := upperExceptions[r]; ok {
			b.WriteString(m)
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// Casefold is str.casefold.
func Casefold(s string) string {
	if IsASCII(s) {
		return strings.ToLower(s)
	}
	var b strings.Builder
	for _, r := range s {
		if m, ok := casefoldExceptions[r]; ok {
			b.WriteString(m)
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Strip is str.strip() (Python whitespace on both ends).
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// LStrip is str.lstrip().
func LStrip(s string) string { return strings.TrimLeftFunc(s, IsSpace) }

// RStrip is str.rstrip().
func RStrip(s string) string { return strings.TrimRightFunc(s, IsSpace) }

// FoldsToASCIILetter reports whether r matches the ASCII letter lower (a-z) in a re pattern
// compiled with IGNORECASE: the letter in either case, plus the extra case-insensitive matches
// CPython's sre recognises (ı and İ for i, ſ for s, the Kelvin sign for k).
func FoldsToASCIILetter(r rune, lower byte) bool {
	if r < 0x80 {
		return r|0x20 == rune(lower) && lower >= 'a' && lower <= 'z'
	}
	switch r {
	case 0x0130, 0x0131:
		return lower == 'i'
	case 0x017F:
		return lower == 's'
	case 0x212A:
		return lower == 'k'
	}
	return false
}
