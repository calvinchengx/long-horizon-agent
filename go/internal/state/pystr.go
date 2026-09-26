package state

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pyIsSpace mirrors Python's str.isspace for one rune (unicode.IsSpace plus the ASCII
// information separators U+001C..U+001F, which Python also treats as whitespace).
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyStrip mirrors Python's str.strip() with no arguments.
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// universalNewlines mirrors Python's text-mode decoding with newline=None: "\r\n" and a lone
// "\r" both become "\n" (subprocess text=True output and Path.read_text both do this).
func universalNewlines(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// isPyLineBoundary reports the line boundaries of Python's str.splitlines (other than "\r\n").
func isPyLineBoundary(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// pySplitlines mirrors Python's str.splitlines(keepends=keepends).
func pySplitlines(s string, keepends bool) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isPyLineBoundary(r) {
			i += size
			continue
		}
		end := i + size
		if r == '\r' && end < len(s) && s[end] == '\n' {
			end++
		}
		if keepends {
			out = append(out, s[start:end])
		} else {
			out = append(out, s[start:i])
		}
		start, i = end, end
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// runeLen is Python's len() for a str.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// pyFloatRepr renders f the way Python's repr(float) does.
func pyFloatRepr(f float64) string {
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, ok := strings.Cut(e, "e")
	if !ok { // NaN / Inf
		switch {
		case f != f:
			return "nan"
		case f > 0:
			return "inf"
		default:
			return "-inf"
		}
	}
	exp, _ := strconv.Atoi(expStr)
	if exp >= -4 && exp < 16 {
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	sign := "+"
	if exp < 0 {
		sign, exp = "-", -exp
	}
	es := strconv.Itoa(exp)
	if len(es) < 2 {
		es = "0" + es
	}
	return mant + "e" + sign + es
}

// PydanticJSON is v as pydantic's model_dump_json writes it (indent => indent=2).
func PydanticJSON(v any, indent bool) ([]byte, error) { return pydanticJSON(v, indent) }

// pydanticJSON serializes v like pydantic's model_dump_json: compact (or indent=2), UTF-8, with
// no HTML escaping and U+2028/U+2029 written raw. No trailing newline.
func pydanticJSON(v any, indent bool) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	raw = unescapeGoOnly(raw)
	if !indent {
		return raw, nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// unescapeGoOnly rewrites the escapes Go's encoder emits but pydantic does not (the escaped
// forms of '<', '>', '&', U+2028 and U+2029) back to the literal characters. It tracks
// string/escape state, so an escaped backslash followed by "u003c" text is left alone.
func unescapeGoOnly(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			out = append(out, c)
			continue
		}
		switch c {
		case '"':
			inString = false
			out = append(out, c)
		case '\\':
			if i+5 < len(b) && b[i+1] == 'u' {
				var lit string
				switch string(b[i+2 : i+6]) {
				case "003c":
					lit = "<"
				case "003e":
					lit = ">"
				case "0026":
					lit = "&"
				case "2028":
					lit = string(rune(0x2028))
				case "2029":
					lit = string(rune(0x2029))
				}
				if lit != "" {
					out = append(out, lit...)
					i += 5
					continue
				}
			}
			out = append(out, c)
			if i+1 < len(b) {
				out = append(out, b[i+1])
				i++
			}
		default:
			out = append(out, c)
		}
	}
	return out
}
