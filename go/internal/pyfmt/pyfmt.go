// Package pyfmt reproduces the Python string semantics LHA prompts and messages depend on:
// code-point len/slicing, str.strip, str() / repr() of JSON-like values, and order-keeping JSON
// decoding (so a dict repr matches Python byte for byte).
package pyfmt

import (
	"encoding/json"
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

// OrderedMap is a JSON object that remembers its key order (so Python's dict repr and json.dumps
// output can be reproduced exactly); see contracts.OrderedMap. Use DecodeOrdered to build one from
// JSON text.
type OrderedMap = contracts.OrderedMap

// NewOrderedMap builds an OrderedMap from alternating key, value arguments (a tool's JSON-Schema
// "properties" in declaration order, so its prompt rendering matches Python's dict repr).
func NewOrderedMap(kv ...any) *OrderedMap { return contracts.NewOrderedMap(kv...) }

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
func pyFloatRepr(f float64) string { return contracts.PyFloatRepr(f) }

// DecodeOrdered decodes JSON text keeping object key order (*OrderedMap) and exact numbers
// (json.Number). Duplicate keys keep their first position and the last value (Python dict).
func DecodeOrdered(data []byte) (any, error) { return contracts.DecodeOrdered(data) }

// PlainJSON converts DecodeOrdered output to what encoding/json produces for `any`
// (map[string]any, []any, float64), which is what tools receive as arguments.
func PlainJSON(v any) any { return contracts.PlainJSON(v) }
