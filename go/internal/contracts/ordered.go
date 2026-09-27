package contracts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// OrderedMap is a JSON object that remembers its key order, the way a Python dict does. Python
// writes dicts (json.dumps, pydantic's model_dump_json) in insertion order, so every Go value
// that ends up as JSON text a Python run also writes (event payloads, tool specs, the ownership
// map) is built as an OrderedMap to come out byte-identical. Values may be JSON scalars, lists,
// nested OrderedMaps, plain maps (written with sorted keys) or anything encoding/json handles.
//
// A nil *OrderedMap is an empty, read-only map: Get, Len and Range work on it.
type OrderedMap struct {
	Keys   []string
	Values map[string]any
}

// NewOrderedMap builds an OrderedMap from alternating key, value arguments. A repeated key keeps
// its first position and takes the last value (a Python dict literal).
func NewOrderedMap(kv ...any) *OrderedMap {
	m := &OrderedMap{Values: make(map[string]any, len(kv)/2)}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		m.Set(k, kv[i+1])
	}
	return m
}

// OrderedFromMap is an OrderedMap of a plain map with its keys sorted (what json.dumps(...,
// sort_keys=True) writes, and the only order a Go map can be given).
func OrderedFromMap(src map[string]any) *OrderedMap {
	m := &OrderedMap{Values: make(map[string]any, len(src))}
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m.Set(k, src[k])
	}
	return m
}

// Set stores value under key; a new key goes last (Python: d[key] = value).
func (m *OrderedMap) Set(key string, value any) {
	if m.Values == nil {
		m.Values = map[string]any{}
	}
	if _, seen := m.Values[key]; !seen {
		m.Keys = append(m.Keys, key)
	}
	m.Values[key] = value
}

// Delete removes key (Python: d.pop(key, None)).
func (m *OrderedMap) Delete(key string) {
	if m == nil {
		return
	}
	if _, ok := m.Values[key]; !ok {
		return
	}
	delete(m.Values, key)
	for i, k := range m.Keys {
		if k == key {
			m.Keys = append(m.Keys[:i:i], m.Keys[i+1:]...)
			break
		}
	}
}

// Get returns the value under key.
func (m *OrderedMap) Get(key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m.Values[key]
	return v, ok
}

// Value returns the value under key (nil when absent).
func (m *OrderedMap) Value(key string) any {
	v, _ := m.Get(key)
	return v
}

// String returns the value under key when it is a string.
func (m *OrderedMap) String(key string) (string, bool) {
	s, ok := m.Value(key).(string)
	return s, ok
}

// Len is the number of keys.
func (m *OrderedMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.Keys)
}

// Range calls fn for each key in order until fn returns false.
func (m *OrderedMap) Range(fn func(key string, value any) bool) {
	if m == nil {
		return
	}
	for _, k := range m.Keys {
		if !fn(k, m.Values[k]) {
			return
		}
	}
}

// Clone is a shallow copy (nil stays nil).
func (m *OrderedMap) Clone() *OrderedMap {
	if m == nil {
		return nil
	}
	out := &OrderedMap{Keys: append([]string{}, m.Keys...), Values: make(map[string]any, len(m.Values))}
	for k, v := range m.Values {
		out.Values[k] = v
	}
	return out
}

// MarshalJSON writes the object with its keys in order and its numbers as Python writes them
// (a float64 is written as pydantic writes a float: 1.0, 0.00001, 1e-6, 1e+16).
func (m *OrderedMap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	if err := writeOrderedJSON(&b, m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// UnmarshalJSON decodes a JSON object keeping its key order at every depth (nested objects
// become *OrderedMap) and its numbers exact (json.Number), so a decoded payload is written back
// byte for byte. JSON null leaves the map empty.
func (m *OrderedMap) UnmarshalJSON(data []byte) error {
	v, err := DecodeOrdered(data)
	if err != nil {
		return err
	}
	switch x := v.(type) {
	case nil:
		*m = OrderedMap{Values: map[string]any{}}
	case *OrderedMap:
		*m = *x
	default:
		return errors.New("OrderedMap: JSON value is not an object")
	}
	return nil
}

func writeOrderedJSON(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case *OrderedMap:
		if x == nil {
			b.WriteString("{}")
			return nil
		}
		b.WriteByte('{')
		for i, k := range x.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			key, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(key)
			b.WriteByte(':')
			if err := writeOrderedJSON(b, x.Values[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case map[string]any:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		return writeOrderedJSON(b, OrderedFromMap(x))
	case []any:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeOrderedJSON(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []*OrderedMap:
		list := make([]any, len(x))
		for i, e := range x {
			list[i] = e
		}
		return writeOrderedJSON(b, list)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("OrderedMap: unsupported float value " + strconv.FormatFloat(x, 'g', -1, 64))
		}
		b.WriteString(PydanticFloat(x))
	default:
		data, err := json.Marshal(x)
		if err != nil {
			return err
		}
		b.Write(data)
	}
	return nil
}

// PydanticFloat renders a finite f the way pydantic's model_dump_json does. It differs from
// repr() only for small magnitudes: plain decimals down to 1e-5 ("0.00001", "0.000025") and an
// unpadded exponent below that ("1.5e-7"); larger values match repr ("1.0", "1e+16").
func PydanticFloat(f float64) string {
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	switch {
	case exp >= -5 && exp < 16:
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	case exp < 0:
		return mant + "e-" + strconv.Itoa(-exp)
	}
	return PyFloatRepr(f)
}

// PyFloatRepr renders f the way Python's repr(float) does ("1.0", "0.0001", "1e-05", "1e+16").
func PyFloatRepr(f float64) string {
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeOrderedValue(dec *json.Decoder) (any, error) {
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
				val, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				m.Set(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return m, nil
		case '[':
			list := []any{}
			for dec.More() {
				val, err := decodeOrderedValue(dec)
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
// (map[string]any, []any, float64).
func PlainJSON(v any) any {
	switch x := v.(type) {
	case *OrderedMap:
		if x == nil {
			return map[string]any{}
		}
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

// AsFloat reads a JSON number however it was decoded or built (float64, int, int64, json.Number).
func AsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// Plain is the map as encoding/json decodes an object into `any` (map[string]any at every depth,
// float64 numbers): a convenience for reading values, never for writing them.
func (m *OrderedMap) Plain() map[string]any { return PlainJSON(m).(map[string]any) }
