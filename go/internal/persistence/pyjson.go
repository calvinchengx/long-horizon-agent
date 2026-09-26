package persistence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// PyDumps renders v the way Python's json.dumps(v, sort_keys=True) does (ensure_ascii=True,
// separators ", " and ": "), so the JSON text a Go run stores is byte-identical to a Python
// run's. float64 is a Python float (repr, "1.0"); ints and json.Number keep their text.
func PyDumps(v any) string {
	var b strings.Builder
	writePy(&b, v)
	return b.String()
}

func writePy(b *strings.Builder, v any) {
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
		writePyString(b, x)
	case json.Number:
		b.WriteString(string(x))
	case float64:
		b.WriteString(pyFloat(x))
	case float32:
		b.WriteString(pyFloat(float64(x)))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case []string:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyString(b, e)
		}
		b.WriteByte(']')
	case []float64:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyFloat(e))
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writePy(b, e)
		}
		b.WriteByte(']')
	case map[string]string:
		keys := sortedKeys(x)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyString(b, k)
			b.WriteString(": ")
			writePyString(b, x[k])
		}
		b.WriteByte('}')
	case map[string]any:
		keys := sortedKeys(x)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyString(b, k)
			b.WriteString(": ")
			writePy(b, x[k])
		}
		b.WriteByte('}')
	case fmt.Stringer:
		writePyString(b, x.String()) // python: default=str
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice {
			items := make([]any, rv.Len())
			for i := range items {
				items[i] = rv.Index(i).Interface()
			}
			writePy(b, items)
			return
		}
		writePyString(b, fmt.Sprint(v))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pyFloat is Python's repr(float) (json.dumps writes NaN/Infinity for non-finite values).
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
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

// writePyString writes s as an ASCII-only JSON string (ensure_ascii=True).
func writePyString(b *strings.Builder, s string) {
	const hexdigits = "0123456789abcdef"
	u4 := func(r rune) {
		b.WriteString(`\u`)
		b.WriteByte(hexdigits[(r>>12)&0xf])
		b.WriteByte(hexdigits[(r>>8)&0xf])
		b.WriteByte(hexdigits[(r>>4)&0xf])
		b.WriteByte(hexdigits[r&0xf])
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
}

// decodeObject decodes a JSON object keeping numbers as json.Number (nil/"" => empty map).
func decodeObject(text string) (map[string]any, error) {
	out := map[string]any{}
	if strings.TrimSpace(text) == "" {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// decodeStringMap decodes a JSON object into strings (python: {str(k): str(v)}).
func DecodeStringMap(text string) (map[string]string, error) {
	raw, err := decodeObject(text)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = pyStr(v)
	}
	return out, nil
}

// decodeStringList decodes a JSON array into strings (python: [str(o) for o in ...]).
func decodeStringList(text string) ([]string, error) {
	out := []string{}
	if strings.TrimSpace(text) == "" {
		return out, nil
	}
	var raw []any
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	for _, v := range raw {
		out = append(out, pyStr(v))
	}
	return out, nil
}

func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return string(x)
	}
	return PyDumps(v)
}

// IdempotencyKey derives a deterministic key from parts (python: lha.ids.idempotency_key):
// sha256 of the parts joined by U+001F, first 32 hex characters.
func IdempotencyKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])[:32]
}

// CostIdempotencyKey is the ledger row key (identical in both backends and in Python).
func CostIdempotencyKey(missionID, cycleID, callKey string) string {
	return IdempotencyKey("cost", missionID, cycleID, callKey)
}

// NowISO is Python's datetime.now(UTC).isoformat(timespec="microseconds").
func NowISO() string { return ISOMicro(time.Now()) }

// ISOMicro formats t (in UTC) like isoformat(timespec="microseconds").
func ISOMicro(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000+00:00") }

// ISOAuto formats t (in UTC) like Python's default isoformat(): microseconds only when non-zero.
func ISOAuto(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

// ParseISO parses an ISO-8601 timestamp (naive = UTC), as Python's datetime.fromisoformat.
func ParseISO(value string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999", "2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("Invalid isoformat string: %s", strconv.Quote(value))
}
