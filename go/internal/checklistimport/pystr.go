package checklistimport

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Python string / path / IO semantics the importer's observable behaviour depends on.

// pyIsSpace is Python's str.isspace for one rune (unicode.IsSpace plus U+001C..U+001F).
func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// pyStrip is str.strip() with no arguments.
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyRstrip is str.rstrip() with no arguments.
func pyRstrip(s string) string { return strings.TrimRightFunc(s, pyIsSpace) }

// runeLen is Python's len() of a str.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// universalNewlines mirrors text-mode reading with newline=None ("\r\n" and "\r" become "\n").
func universalNewlines(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// pySplitlines is str.splitlines() (no keepends).
func pySplitlines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		default:
			i += size
			continue
		}
		end := i + size
		if r == '\r' && end < len(s) && s[end] == '\n' {
			end++
		}
		out = append(out, s[start:i])
		start, i = end, end
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// pyPath renders a path string the way str(pathlib.PurePosixPath(s)) does: repeated slashes
// and "." components collapse, a trailing slash goes, "" becomes ".", and exactly two leading
// slashes are kept (POSIX implementation-defined root).
func pyPath(s string) string {
	root := ""
	switch {
	case strings.HasPrefix(s, "//") && !strings.HasPrefix(s, "///"):
		root = "//"
	case strings.HasPrefix(s, "/"):
		root = "/"
	}
	parts := []string{}
	for _, part := range strings.Split(s, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	out := root + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}

// pyPathName is PurePath.name ("" for "/" or ".").
func pyPathName(p string) string {
	if p == "." || strings.TrimRight(p, "/") == "" {
		return ""
	}
	return p[strings.LastIndex(p, "/")+1:]
}

// pySuffix is PurePath.suffix (Python 3.12 rules: a name ending in "." has no suffix).
func pySuffix(p string) string {
	name := pyPathName(p)
	i := strings.LastIndex(name, ".")
	if 0 < i && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// pyStem is PurePath.stem.
func pyStem(p string) string {
	name := pyPathName(p)
	i := strings.LastIndex(name, ".")
	if 0 < i && i < len(name)-1 {
		return name[:i]
	}
	return name
}

// pyLower is str.lower(): Go's simple lowercase mapping plus Python's two full-mapping
// differences, U+0130 -> "i̇" and the final-sigma rule. The final-sigma context test
// approximates Unicode's case-ignorable set (Mn/Me/Cf/Lm/Sk plus the common MidLetter
// punctuation), which is exact for every realistic file suffix.
func pyLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case r == 0x130:
			b.WriteString("i̇")
		case r == 0x3a3 && finalSigma(rs, i):
			b.WriteRune(0x3c2)
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func isCased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.In(r, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

func isCaseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027, 0xfe13,
		0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

func finalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && isCaseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !isCased(rs[j]) {
		return false
	}
	k := i + 1
	for k < len(rs) && isCaseIgnorable(rs[k]) {
		k++
	}
	return k == len(rs) || !isCased(rs[k])
}

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

// DecodeError is a file that is not valid UTF-8 (python: UnicodeDecodeError, a ValueError that
// load_checklist does NOT wrap in ChecklistImportError). Error() matches str(exc).
type DecodeError struct{ Message string }

func (e *DecodeError) Error() string { return e.Message }

// decodeUTF8 validates b the way bytes.decode("utf-8") does and reports the first error in
// CPython's words (position is a byte offset; the span is the maximal valid prefix).
func decodeUTF8(b []byte) (string, error) {
	if utf8.Valid(b) {
		return string(b), nil
	}
	cont := func(c byte) bool { return c >= 0x80 && c <= 0xbf }
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			i++
			continue
		}
		var need int
		lo, hi := byte(0x80), byte(0xbf)
		switch {
		case c >= 0xc2 && c <= 0xdf:
			need = 1
		case c == 0xe0:
			need, lo = 2, 0xa0
		case c == 0xed:
			need, hi = 2, 0x9f
		case c >= 0xe1 && c <= 0xef:
			need = 2
		case c == 0xf0:
			need, lo = 3, 0x90
		case c == 0xf4:
			need, hi = 3, 0x8f
		case c >= 0xf1 && c <= 0xf3:
			need = 3
		default:
			return "", utf8Err(b, i, i+1, "invalid start byte")
		}
		for k := 1; k <= need; k++ {
			if i+k >= len(b) {
				return "", utf8Err(b, i, len(b), "unexpected end of data")
			}
			d := b[i+k]
			ok := cont(d)
			if k == 1 {
				ok = d >= lo && d <= hi
			}
			if !ok {
				return "", utf8Err(b, i, i+k, "invalid continuation byte")
			}
		}
		i += need + 1
	}
	return string(b), nil
}

func utf8Err(b []byte, start, end int, reason string) error {
	if end-start == 1 {
		return &DecodeError{fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", b[start], start, reason)}
	}
	return &DecodeError{fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", start, end-1, reason)}
}

// errNullByte is Python's ValueError for a path containing NUL (not an OSError).
var errNullByte = errors.New("embedded null byte")

// readText is Path.read_text(encoding="utf-8"): it returns (text, osErr, otherErr) where osErr
// is rendered like str(OSError) ("[Errno 2] No such file or directory: 'p'") and otherErr is an
// exception Python would not class as OSError (decode errors, NUL in the path).
func readText(p string) (string, string, error) {
	if strings.ContainsRune(p, 0) {
		return "", "", errNullByte
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", pyOSError(p, err), nil
	}
	text, derr := decodeUTF8(raw)
	if derr != nil {
		return "", "", derr
	}
	return universalNewlines(text), "", nil
}

// pyOSError renders a Go filesystem error as str(OSError): "[Errno N] Strerror: 'filename'".
func pyOSError(p string, err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		msg := errno.Error()
		if msg != "" {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), msg, contracts.PyRepr(p))
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Sprintf("%s: %s", pe.Err, contracts.PyRepr(p))
	}
	return err.Error()
}
