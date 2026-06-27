package governor

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// pyJSONDumps renders v the way Python's json.dumps(v) does with default arguments
// (ensure_ascii=True, separators ", " and ": ", NaN/Infinity allowed). Only its LENGTH feeds the
// token estimate, so map key order (lost in Go maps; sorted here) does not matter.
//
// Numbers: Python distinguishes int from float, Go's decoded float64 does not. An integral
// float64 is rendered as an int (what a model's JSON "1" decodes to in Python); json.Number keeps
// the distinction exactly.
func pyJSONDumps(v any) string {
	var b strings.Builder
	writePyJSON(&b, v)
	return b.String()
}

func writePyJSON(b *strings.Builder, v any) {
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
		writePyNumber(b, x)
	case float64:
		writePyFloat(b, x, true)
	case float32:
		writePyFloat(b, float64(x), true)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyString(b, k)
			b.WriteString(": ")
			writePyJSON(b, x[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSON(b, e)
		}
		b.WriteByte(']')
	default:
		// Other Go values (typed slices/maps, structs): normalize through encoding/json.
		rv := reflect.ValueOf(v)
		if (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) && rv.IsNil() {
			b.WriteString("null")
			return
		}
		raw, err := json.Marshal(v)
		if err != nil {
			b.WriteString("null")
			return
		}
		var norm any
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&norm); err != nil {
			b.WriteString("null")
			return
		}
		writePyJSON(b, norm)
	}
}

func writePyNumber(b *strings.Builder, n json.Number) {
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		if i, ok := new(big.Int).SetString(s, 10); ok {
			b.WriteString(i.String())
			return
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) { // out-of-range -> inf / 0.0 like float()
		b.WriteString(s)
		return
	}
	writePyFloat(b, f, false)
}

func writePyFloat(b *strings.Builder, f float64, integralAsInt bool) {
	switch {
	case math.IsNaN(f):
		b.WriteString("NaN")
	case math.IsInf(f, 1):
		b.WriteString("Infinity")
	case math.IsInf(f, -1):
		b.WriteString("-Infinity")
	case integralAsInt && f == math.Trunc(f) && math.Abs(f) < 1e21:
		b.WriteString(strconv.FormatFloat(f+0, 'f', 0, 64)) // f+0 turns -0 into 0 (Python int)
	default:
		b.WriteString(pyFloatRepr(f))
	}
}

// pyFloatRepr renders f the way Python's repr(float) does (finite values).
func pyFloatRepr(f float64) string {
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

// writePyString writes s as an ASCII-only JSON string (Python's ensure_ascii=True).
func writePyString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
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
}
