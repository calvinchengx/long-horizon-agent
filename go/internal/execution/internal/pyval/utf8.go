package pyval

import (
	"fmt"
	"strings"
)

// UnicodeDecodeError mirrors Python's UnicodeDecodeError for the utf-8 codec; Error() is the
// exact str(exc).
type UnicodeDecodeError struct {
	Start, End int // the offending byte range [Start, End)
	Byte       byte
	Reason     string
}

func (e *UnicodeDecodeError) Error() string {
	if e.End-e.Start == 1 {
		return fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", e.Byte, e.Start, e.Reason)
	}
	return fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", e.Start, e.End-1, e.Reason)
}

// PyTypeName names the Python exception type.
func (e *UnicodeDecodeError) PyTypeName() string { return "UnicodeDecodeError" }

func isCont(b byte) bool { return b >= 0x80 && b <= 0xBF }

// nextError scans data from i and returns the length of the valid sequence at i (>0), or the
// error range end and reason (length 0).
func step(data []byte, i int) (size int, errEnd int, reason string) {
	n := len(data)
	ch := data[i]
	switch {
	case ch < 0x80:
		return 1, 0, ""
	case ch < 0xC2:
		return 0, i + 1, "invalid start byte"
	case ch < 0xE0:
		if n-i < 2 {
			return 0, n, "unexpected end of data"
		}
		if !isCont(data[i+1]) {
			return 0, i + 1, "invalid continuation byte"
		}
		return 2, 0, ""
	case ch < 0xF0:
		if n-i < 3 {
			if n-i < 2 {
				return 0, n, "unexpected end of data"
			}
			c2 := data[i+1]
			bad := ch == 0xED
			if c2 < 0xA0 {
				bad = ch == 0xE0
			}
			if !isCont(c2) || bad {
				return 0, i + 1, "invalid continuation byte"
			}
			return 0, n, "unexpected end of data"
		}
		c2, c3 := data[i+1], data[i+2]
		if !isCont(c2) || (ch == 0xE0 && c2 < 0xA0) || (ch == 0xED && c2 >= 0xA0) {
			return 0, i + 1, "invalid continuation byte"
		}
		if !isCont(c3) {
			return 0, i + 2, "invalid continuation byte"
		}
		return 3, 0, ""
	case ch < 0xF5:
		if n-i < 4 {
			if n-i < 2 {
				return 0, n, "unexpected end of data"
			}
			c2 := data[i+1]
			bad := ch == 0xF4
			if c2 < 0x90 {
				bad = ch == 0xF0
			}
			if !isCont(c2) || bad {
				return 0, i + 1, "invalid continuation byte"
			}
			if n-i < 3 {
				return 0, n, "unexpected end of data"
			}
			if !isCont(data[i+2]) {
				return 0, i + 2, "invalid continuation byte"
			}
			return 0, n, "unexpected end of data"
		}
		c2, c3, c4 := data[i+1], data[i+2], data[i+3]
		if !isCont(c2) || (ch == 0xF0 && c2 < 0x90) || (ch == 0xF4 && c2 >= 0x90) {
			return 0, i + 1, "invalid continuation byte"
		}
		if !isCont(c3) {
			return 0, i + 2, "invalid continuation byte"
		}
		if !isCont(c4) {
			return 0, i + 3, "invalid continuation byte"
		}
		return 4, 0, ""
	}
	return 0, i + 1, "invalid start byte"
}

// DecodeUTF8 is bytes.decode("utf-8") (strict): the first error is a *UnicodeDecodeError.
func DecodeUTF8(data []byte) (string, error) {
	for i := 0; i < len(data); {
		size, end, reason := step(data, i)
		if size == 0 {
			return "", &UnicodeDecodeError{Start: i, End: end, Byte: data[i], Reason: reason}
		}
		i += size
	}
	return string(data), nil
}

// DecodeUTF8Replace is bytes.decode("utf-8", errors="replace"): every error range becomes one
// U+FFFD, exactly as CPython does (Go's strings.ToValidUTF8 would merge adjacent ranges).
func DecodeUTF8Replace(data []byte) string {
	var b strings.Builder
	b.Grow(len(data))
	last := 0
	for i := 0; i < len(data); {
		size, end, _ := step(data, i)
		if size == 0 {
			b.Write(data[last:i])
			b.WriteString("�")
			i, last = end, end
			continue
		}
		i += size
	}
	b.Write(data[last:])
	return b.String()
}
