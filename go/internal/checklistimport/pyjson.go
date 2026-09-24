package checklistimport

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// A decoder with the observable behaviour of CPython 3.12's json.loads (the C scanner):
// the same accepted language (NaN, Infinity, -Infinity, arbitrary-precision ints), the same
// value model (ordered objects whose duplicate keys keep their first position and last value)
// and the same error messages and positions (in code points).
//
// Values: string, *big.Int, float64, bool, nil, []any, *pyDict.
//
// Known gaps: a lone surrogate escape ("\ud800") decodes to U+FFFD (Go strings cannot hold
// one), and very deep nesting does not raise Python's RecursionError.

// pyDict is a Python dict decoded from a JSON object (insertion-ordered).
type pyDict struct {
	keys []string
	vals map[string]any
}

func newPyDict() *pyDict { return &pyDict{vals: map[string]any{}} }

func (d *pyDict) set(k string, v any) {
	if _, ok := d.vals[k]; !ok {
		d.keys = append(d.keys, k)
	}
	d.vals[k] = v
}

func (d *pyDict) get(k string) (any, bool) {
	v, ok := d.vals[k]
	return v, ok
}

// jsonDecodeError is json.JSONDecodeError (or the plain ValueError of the int digit limit);
// Error() matches str(exc).
type jsonDecodeError struct{ rendered string }

func (e *jsonDecodeError) Error() string { return e.rendered }

// stopIteration is the scanner's "no value here" signal (becomes "Expecting value").
type stopIteration struct{ pos int }

func (e *stopIteration) Error() string { return "StopIteration" }

type pyJSONDecoder struct{ s []rune }

func (d *pyJSONDecoder) errAt(msg string, pos int) error {
	line := 1
	last := -1
	for i := 0; i < pos && i < len(d.s); i++ {
		if d.s[i] == '\n' {
			line++
			last = i
		}
	}
	col := pos - last
	return &jsonDecodeError{fmt.Sprintf("%s: line %d column %d (char %d)", msg, line, col, pos)}
}

// pyJSONLoads is json.loads(s).
func pyJSONLoads(text string) (any, error) {
	d := &pyJSONDecoder{s: []rune(text)}
	if len(d.s) > 0 && d.s[0] == 0xfeff {
		return nil, d.errAt("Unexpected UTF-8 BOM (decode using utf-8-sig)", 0)
	}
	idx := d.ws(0)
	v, end, err := d.scan(idx)
	if err != nil {
		if st, ok := err.(*stopIteration); ok {
			return nil, d.errAt("Expecting value", st.pos)
		}
		return nil, err
	}
	end = d.ws(end)
	if end != len(d.s) {
		return nil, d.errAt("Extra data", end)
	}
	return v, nil
}

func (d *pyJSONDecoder) ws(i int) int {
	for i < len(d.s) {
		switch d.s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func (d *pyJSONDecoder) hasAt(i int, lit string) bool {
	r := []rune(lit)
	if i+len(r) > len(d.s) {
		return false
	}
	for k, c := range r {
		if d.s[i+k] != c {
			return false
		}
	}
	return true
}

func (d *pyJSONDecoder) scan(i int) (any, int, error) {
	n := len(d.s)
	if i >= n {
		return nil, i, &stopIteration{i}
	}
	switch d.s[i] {
	case '"':
		return d.scanString(i + 1)
	case '{':
		return d.object(i + 1)
	case '[':
		return d.array(i + 1)
	case 'n':
		if d.hasAt(i, "null") {
			return nil, i + 4, nil
		}
	case 't':
		if d.hasAt(i, "true") {
			return true, i + 4, nil
		}
	case 'f':
		if d.hasAt(i, "false") {
			return false, i + 5, nil
		}
	case 'N':
		if d.hasAt(i, "NaN") {
			return math.NaN(), i + 3, nil
		}
	case 'I':
		if d.hasAt(i, "Infinity") {
			return math.Inf(1), i + 8, nil
		}
	case '-':
		if d.hasAt(i, "-Infinity") {
			return math.Inf(-1), i + 9, nil
		}
	}
	return d.number(i)
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func (d *pyJSONDecoder) number(start int) (any, int, error) {
	s, endIdx := d.s, len(d.s)-1
	idx := start
	if s[idx] == '-' {
		idx++
		if idx > endIdx {
			return nil, start, &stopIteration{start}
		}
	}
	switch {
	case s[idx] >= '1' && s[idx] <= '9':
		idx++
		for idx <= endIdx && isDigit(s[idx]) {
			idx++
		}
	case s[idx] == '0':
		idx++
	default:
		return nil, start, &stopIteration{start}
	}
	isFloat := false
	if idx < endIdx && s[idx] == '.' && isDigit(s[idx+1]) {
		isFloat = true
		idx += 2
		for idx <= endIdx && isDigit(s[idx]) {
			idx++
		}
	}
	if idx < endIdx && (s[idx] == 'e' || s[idx] == 'E') {
		eStart := idx
		idx++
		if idx < endIdx && (s[idx] == '-' || s[idx] == '+') {
			idx++
		}
		for idx <= endIdx && isDigit(s[idx]) {
			idx++
		}
		if isDigit(s[idx-1]) {
			isFloat = true
		} else {
			idx = eStart
		}
	}
	text := string(s[start:idx])
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !isRangeErr(err) {
			return nil, start, err
		}
		return f, idx, nil
	}
	digits := len(strings.TrimPrefix(text, "-"))
	if digits > 4300 {
		msg := fmt.Sprintf("Exceeds the limit (4300 digits) for integer string conversion: value has %d digits; "+
			"use sys.set_int_max_str_digits() to increase the limit", digits)
		return nil, start, &jsonDecodeError{msg}
	}
	v, _ := new(big.Int).SetString(text, 10)
	return v, idx, nil
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

func hexVal(r rune) (rune, bool) {
	switch {
	case r >= '0' && r <= '9':
		return r - '0', true
	case r >= 'a' && r <= 'f':
		return r - 'a' + 10, true
	case r >= 'A' && r <= 'F':
		return r - 'A' + 10, true
	}
	return 0, false
}

// scanString is scanstring_unicode (strict=True); end is the index after the opening quote.
func (d *pyJSONDecoder) scanString(end int) (any, int, error) {
	s, n := d.s, len(d.s)
	begin := end - 1
	var b strings.Builder
	for {
		next := end
		var c rune
		for ; next < n; next++ {
			c = s[next]
			if c == '"' || c == '\\' {
				break
			}
			if c <= 0x1f {
				return nil, 0, d.errAt("Invalid control character at", next)
			}
		}
		if next >= n {
			return nil, 0, d.errAt("Unterminated string starting at", begin)
		}
		b.WriteString(string(s[end:next]))
		next++
		if c == '"' {
			return b.String(), next, nil
		}
		if next == n {
			return nil, 0, d.errAt("Unterminated string starting at", begin)
		}
		c = s[next]
		if c != 'u' {
			end = next + 1
			switch c {
			case '"', '\\', '/':
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			default:
				return nil, 0, d.errAt("Invalid \\escape", end-2)
			}
			b.WriteRune(c)
			continue
		}
		next++
		end = next + 4
		if end >= n {
			return nil, 0, d.errAt("Invalid \\uXXXX escape", next-1)
		}
		c = 0
		for ; next < end; next++ {
			h, ok := hexVal(s[next])
			if !ok {
				return nil, 0, d.errAt("Invalid \\uXXXX escape", end-5)
			}
			c = c<<4 | h
		}
		if c >= 0xd800 && c <= 0xdbff && end+6 < n && s[next] == '\\' && s[next+1] == 'u' {
			next += 2
			end += 6
			var c2 rune
			for ; next < end; next++ {
				h, ok := hexVal(s[next])
				if !ok {
					return nil, 0, d.errAt("Invalid \\uXXXX escape", end-5)
				}
				c2 = c2<<4 | h
			}
			if c2 >= 0xdc00 && c2 <= 0xdfff {
				c = utf16.DecodeRune(c, c2)
			} else {
				end -= 6
			}
		}
		b.WriteRune(c) // a lone surrogate becomes U+FFFD (documented gap)
	}
}

func (d *pyJSONDecoder) object(idx int) (any, int, error) {
	s, n := d.s, len(d.s)
	out := newPyDict()
	idx = d.ws(idx)
	if idx >= n || s[idx] != '}' {
		for {
			if idx >= n || s[idx] != '"' {
				return nil, 0, d.errAt("Expecting property name enclosed in double quotes", idx)
			}
			key, next, err := d.scanString(idx + 1)
			if err != nil {
				return nil, 0, err
			}
			idx = d.ws(next)
			if idx >= n || s[idx] != ':' {
				return nil, 0, d.errAt("Expecting ':' delimiter", idx)
			}
			idx = d.ws(idx + 1)
			val, next, err := d.scan(idx)
			if err != nil {
				return nil, 0, err
			}
			out.set(key.(string), val)
			idx = d.ws(next)
			if idx < n && s[idx] == '}' {
				break
			}
			if idx >= n || s[idx] != ',' {
				return nil, 0, d.errAt("Expecting ',' delimiter", idx)
			}
			idx = d.ws(idx + 1)
		}
	}
	return out, idx + 1, nil
}

func (d *pyJSONDecoder) array(idx int) (any, int, error) {
	s, n := d.s, len(d.s)
	out := []any{}
	idx = d.ws(idx)
	if idx >= n || s[idx] != ']' {
		for {
			val, next, err := d.scan(idx)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, val)
			idx = d.ws(next)
			if idx < n && s[idx] == ']' {
				break
			}
			if idx >= n || s[idx] != ',' {
				return nil, 0, d.errAt("Expecting ',' delimiter", idx)
			}
			idx = d.ws(idx + 1)
		}
	}
	return out, idx + 1, nil
}

// pyValueRepr is repr() of a decoded value.
func pyValueRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return contracts.PyRepr(x)
	case *big.Int:
		return x.String()
	case float64:
		return pyFloatRepr(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyValueRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyDict:
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = contracts.PyRepr(k) + ": " + pyValueRepr(x.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// pyTypeName is type(v).__name__.
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case *big.Int:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	case *pyDict:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}
