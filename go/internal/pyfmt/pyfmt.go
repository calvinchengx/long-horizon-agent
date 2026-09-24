// Package pyfmt reproduces the Python string semantics LHA prompts and messages depend on:
// code-point len/slicing, str.strip, str() / repr() of JSON-like values, and order-keeping JSON
// decoding (so a dict repr matches Python byte for byte).
package pyfmt

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Python string semantics the prompts depend on: len() and slicing count code points, and
// str.strip() removes Unicode whitespace plus the \x1c-\x1f separators.

func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// PyStrip is Python's str.strip().
func PyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// RuneLen is Python's len(str).
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// Head is Python's s[:n] (n >= 0).
func Head(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// Tail is Python's s[-n:] for n > 0 (the whole string when shorter).
func Tail(s string, n int) string {
	if n <= 0 {
		return s // s[-0:] is the whole string in Python
	}
	total := RuneLen(s)
	if total <= n {
		return s
	}
	skip := total - n
	i := 0
	for pos := range s {
		if i == skip {
			return s[pos:]
		}
		i++
	}
	return ""
}

// PyStr renders a JSON-decoded value the way Python's str() does inside an f-string: strings as
// themselves, everything else as repr().
func PyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return PyReprValue(v)
}

// PyReprValue renders a JSON-like value as Python's repr(): dicts ({'k': v}), lists, strings
// (quoted), True/False/None, ints and floats. Plain Go maps have no insertion order, so their
// keys are rendered sorted (Python renders declaration order); OrderedMap keeps its order.
func PyReprValue(v any) string {
	var b strings.Builder
	writeRepr(&b, v)
	return b.String()
}

// OrderedMap is a JSON object that remembers its key order (so Python's dict repr can be
// reproduced exactly). Use DecodeOrdered to build one from JSON text.
type OrderedMap struct {
	Keys   []string
	Values map[string]any
}

// NewOrderedMap builds an OrderedMap from alternating key, value arguments (a tool's JSON-Schema
// "properties" in declaration order, so its prompt rendering matches Python's dict repr).
func NewOrderedMap(kv ...any) *OrderedMap {
	m := &OrderedMap{Values: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		if _, seen := m.Values[k]; !seen {
			m.Keys = append(m.Keys, k)
		}
		m.Values[k] = kv[i+1]
	}
	return m
}

// MarshalJSON writes the object with its keys in order.
func (m *OrderedMap) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range m.Keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(m.Values[k])
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func writeRepr(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("None")
	case bool:
		if x {
			b.WriteString("True")
		} else {
			b.WriteString("False")
		}
	case string:
		b.WriteString(contracts.PyRepr(x))
	case json.Number:
		b.WriteString(pyNumber(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(pyFloatRepr(x))
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(contracts.PyRepr(s))
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRepr(b, e)
		}
		b.WriteByte(']')
	case *OrderedMap:
		b.WriteByte('{')
		for i, k := range x.Keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(contracts.PyRepr(k))
			b.WriteString(": ")
			writeRepr(b, x.Values[k])
		}
		b.WriteByte('}')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		writeRepr(b, &OrderedMap{Keys: keys, Values: x})
	case []map[string]any:
		list := make([]any, len(x))
		for i, m := range x {
			list[i] = m
		}
		writeRepr(b, list)
	default:
		// Round-trip anything else through JSON (structs, typed slices).
		data, err := json.Marshal(x)
		if err != nil {
			b.WriteString(contracts.PyRepr(err.Error()))
			return
		}
		decoded, err := DecodeOrdered(data)
		if err != nil {
			b.WriteString(contracts.PyRepr(string(data)))
			return
		}
		writeRepr(b, decoded)
	}
}

func pyNumber(n json.Number) string {
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		if i, ok := new(big.Int).SetString(s, 10); ok {
			return i.String()
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && f == 0 {
		return s
	}
	return pyFloatRepr(f)
}

// pyFloatRepr renders f the way Python's repr(float) does.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
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

// DecodeOrdered decodes JSON text keeping object key order (*OrderedMap) and exact numbers
// (json.Number). Duplicate keys keep their first position and the last value (Python dict).
func DecodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := &OrderedMap{Values: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if _, seen := m.Values[key]; !seen {
					m.Keys = append(m.Keys, key)
				}
				m.Values[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return m, nil
		case '[':
			list := []any{}
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, &json.SyntaxError{}
	default:
		return t, nil
	}
}

// PlainJSON converts DecodeOrdered output to what encoding/json produces for `any`
// (map[string]any, []any, float64), which is what tools receive as arguments.
func PlainJSON(v any) any {
	switch x := v.(type) {
	case *OrderedMap:
		out := make(map[string]any, len(x.Keys))
		for _, k := range x.Keys {
			out[k] = PlainJSON(x.Values[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = PlainJSON(e)
		}
		return out
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return v
	}
}
