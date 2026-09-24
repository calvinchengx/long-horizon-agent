// Package pyval reproduces the few Python value semantics the execution layer's observable
// strings depend on: repr()/str() of decoded JSON values, type(x).__name__, ==, json.dumps with
// ensure_ascii, len()/slicing by code point, str.splitlines and UTF-8 decoding (strict and
// errors="replace").
//
// Numbers: tool arguments reach Go as float64 (encoding/json), json.Number (spec corpora) or Go
// ints (tests and Go-built values). Python distinguishes int from float; a float64 cannot, so an
// INTEGRAL float64 is treated as a Python int (what the model's JSON "5" decodes to in Python).
// json.Number keeps the distinction exactly ("5" is an int, "5.0" a float).
package pyval

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// num classifies a numeric value: ok=false for non-numbers.
type num struct {
	isInt bool
	i     *big.Int // set when isInt
	f     float64  // always set (the float value)
}

func asNum(v any) (num, bool) {
	switch x := v.(type) {
	case int:
		return num{true, big.NewInt(int64(x)), float64(x)}, true
	case int8:
		return num{true, big.NewInt(int64(x)), float64(x)}, true
	case int16:
		return num{true, big.NewInt(int64(x)), float64(x)}, true
	case int32:
		return num{true, big.NewInt(int64(x)), float64(x)}, true
	case int64:
		return num{true, big.NewInt(x), float64(x)}, true
	case uint:
		return num{true, new(big.Int).SetUint64(uint64(x)), float64(x)}, true
	case uint32:
		return num{true, new(big.Int).SetUint64(uint64(x)), float64(x)}, true
	case uint64:
		return num{true, new(big.Int).SetUint64(x), float64(x)}, true
	case float32:
		return floatNum(float64(x)), true
	case float64:
		return floatNum(x), true
	case json.Number:
		s := string(x)
		if !strings.ContainsAny(s, ".eE") {
			if i, ok := new(big.Int).SetString(s, 10); ok {
				f, _ := new(big.Float).SetInt(i).Float64()
				return num{true, i, f}, true
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return num{f: f}, true
		}
		return num{f: f}, true
	}
	return num{}, false
}

func floatNum(f float64) num {
	if !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f) && math.Abs(f) < 1<<63 {
		i, _ := new(big.Float).SetFloat64(f).Int(nil)
		return num{true, i, f}
	}
	return num{f: f}
}

// IsNumber reports a Python int or float (bool excluded).
func IsNumber(v any) bool { _, ok := asNum(v); return ok }

// IsInt reports a Python int (bool excluded; an integral float64 counts, see the package doc).
func IsInt(v any) bool { n, ok := asNum(v); return ok && n.isInt }

// Float is the value of a number as float64 (0 for non-numbers).
func Float(v any) float64 { n, _ := asNum(v); return n.f }

// Int is the value of an int (IsInt) as int, saturating at the int range.
func Int(v any) int {
	n, ok := asNum(v)
	if !ok {
		return 0
	}
	if n.isInt {
		if n.i.IsInt64() {
			x := n.i.Int64()
			if x > math.MaxInt {
				return math.MaxInt
			}
			if x < math.MinInt {
				return math.MinInt
			}
			return int(x)
		}
		if n.i.Sign() > 0 {
			return math.MaxInt
		}
		return math.MinInt
	}
	return int(n.f)
}

// Compare compares two numbers (-1, 0, 1).
func Compare(a, b any) int {
	x, _ := asNum(a)
	y, _ := asNum(b)
	if x.isInt && y.isInt {
		return x.i.Cmp(y.i)
	}
	switch {
	case x.f < y.f:
		return -1
	case x.f > y.f:
		return 1
	}
	return 0
}

// TypeName is type(v).__name__ for a decoded JSON value.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case []any, []string:
		return "list"
	case map[string]any, map[string]string:
		return "dict"
	}
	if n, ok := asNum(v); ok {
		if n.isInt {
			return "int"
		}
		return "float"
	}
	return "object"
}

// FloatRepr is repr() of a Python float.
func FloatRepr(f float64) string {
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

func numStr(n num) string {
	if n.isInt {
		return n.i.String()
	}
	return FloatRepr(n.f)
}

// SortedKeys returns m's keys in sorted order (Python dicts keep insertion order, which a Go map
// cannot: every rendering of a map in this layer is key-sorted).
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Repr is repr(v) for a decoded JSON value (dicts are rendered key-sorted).
func Repr(v any) string {
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
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = Repr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = contracts.PyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := SortedKeys(x)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = contracts.PyRepr(k) + ": " + Repr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]string:
		keys := SortedKeys(x)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = contracts.PyRepr(k) + ": " + contracts.PyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if n, ok := asNum(v); ok {
		return numStr(n)
	}
	return fmt.Sprint(v) // not a JSON value: no Python equivalent
}

// Str is str(v) for a decoded JSON value.
func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return Repr(v)
}

// Truthy is Python truthiness for a decoded JSON value.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case map[string]string:
		return len(x) > 0
	}
	if n, ok := asNum(v); ok {
		if n.isInt {
			return n.i.Sign() != 0
		}
		return n.f != 0
	}
	return true
}

// Equal is Python == for decoded JSON values (1 == 1.0 == True).
func Equal(a, b any) bool {
	na, aNum := numOrBool(a)
	nb, bNum := numOrBool(b)
	if aNum || bNum {
		if !(aNum && bNum) {
			return false
		}
		if na.isInt && nb.isInt {
			return na.i.Cmp(nb.i) == 0
		}
		return na.f == nb.f
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := toList(b)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case []string:
		xs, _ := toList(x)
		return Equal(xs, b)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

func numOrBool(v any) (num, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return num{true, big.NewInt(1), 1}, true
		}
		return num{true, big.NewInt(0), 0}, true
	}
	return asNum(v)
}

func toList(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// JSONDumps is json.dumps(v, ensure_ascii=True) with sort_keys=True; compact selects
// separators=(",", ":") instead of the default (", ", ": "). Non-JSON values are rendered with
// str() as a JSON string (default=str).
func JSONDumps(v any, compact bool) string {
	var b strings.Builder
	sep, kv := ", ", ": "
	if compact {
		sep, kv = ",", ":"
	}
	writeJSON(&b, v, sep, kv)
	return b.String()
}

func writeJSON(b *strings.Builder, v any, sep, kv string) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(JSONString(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(sep)
			}
			writeJSON(b, e, sep, kv)
		}
		b.WriteByte(']')
	case []string:
		l, _ := toList(x)
		writeJSON(b, l, sep, kv)
	case map[string]any:
		b.WriteByte('{')
		for i, k := range SortedKeys(x) {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString(JSONString(k))
			b.WriteString(kv)
			writeJSON(b, x[k], sep, kv)
		}
		b.WriteByte('}')
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = s
		}
		writeJSON(b, m, sep, kv)
	default:
		if n, ok := asNum(v); ok {
			switch {
			case n.isInt:
				b.WriteString(n.i.String())
			case math.IsNaN(n.f):
				b.WriteString("NaN")
			case math.IsInf(n.f, 1):
				b.WriteString("Infinity")
			case math.IsInf(n.f, -1):
				b.WriteString("-Infinity")
			default:
				b.WriteString(FloatRepr(n.f))
			}
			return
		}
		b.WriteString(JSONString(Str(v)))
	}
}

// JSONString renders s as a JSON string literal with Python's ensure_ascii=True escaping.
func JSONString(s string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	u4 := func(r rune) {
		b.WriteString(`\u`)
		b.WriteByte(hex[(r>>12)&0xf])
		b.WriteByte(hex[(r>>8)&0xf])
		b.WriteByte(hex[(r>>4)&0xf])
		b.WriteByte(hex[r&0xf])
	}
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		case r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			u4(hi)
			u4(lo)
		default:
			u4(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Len is len(s) for a Python str (code points).
func Len(s string) int { return utf8.RuneCountInString(s) }

// Head is s[:n] for a Python str (the first n code points).
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

// Tail is s[-n:] for a Python str (the last n code points; all of s if shorter).
func Tail(s string, n int) string {
	count := utf8.RuneCountInString(s)
	if count <= n {
		return s
	}
	skip := count - n
	i := 0
	for pos := range s {
		if i == skip {
			return s[pos:]
		}
		i++
	}
	return ""
}

// SplitLines is str.splitlines() (no keepends).
func SplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			i += size
			start = i
			continue
		case '\r':
			out = append(out, s[start:i])
			i += size
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
