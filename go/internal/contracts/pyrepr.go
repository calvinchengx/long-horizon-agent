package contracts

import (
	"strings"
	"unicode"
)

// PyRepr renders s the way Python's repr() renders a str. Error and gate messages embed values
// with repr ("{path!r}"), and those messages are part of the observable behaviour both
// implementations share (spec/), so Go reproduces the quoting rules exactly: single quotes unless
// the string contains a single quote and no double quote, and backslash escapes for control and
// non-printable characters.
func PyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x`)
			b.WriteString(hex2(int(r)))
		case r < 0x80:
			b.WriteRune(r)
		case !isPyPrintable(r):
			switch {
			case r <= 0xff:
				b.WriteString(`\x` + hex2(int(r)))
			case r <= 0xffff:
				b.WriteString(`\u` + hexN(int(r), 4))
			default:
				b.WriteString(`\U` + hexN(int(r), 8))
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// isPyPrintable approximates Python's str.isprintable for non-ASCII runes: separators (Z*) other
// than the plain space and "other" categories (C*) are escaped.
func isPyPrintable(r rune) bool {
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp, unicode.Zs) {
		return false
	}
	return unicode.IsPrint(r) || unicode.IsGraphic(r)
}

func hex2(v int) string { return hexN(v, 2) }

func hexN(v, n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = digits[v&0xf]
		v >>= 4
	}
	return string(out)
}
