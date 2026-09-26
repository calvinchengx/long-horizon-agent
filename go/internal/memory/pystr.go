package memory

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Python string/value semantics the memory plane's text depends on.

func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// oneLine is python's _one_line: whitespace runs collapsed to one space, capped at limit code
// points ("..." marks a cut).
func oneLine(text string, limit int) string {
	flat := strings.Join(strings.FieldsFunc(text, isPySpace), " ")
	if pyfmt.RuneLen(flat) <= limit {
		return flat
	}
	return pyfmt.Head(flat, limit-3) + "..."
}

func isPyLineBoundary(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// splitLines is Python's str.splitlines().
func splitLines(s string) []string {
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
		out = append(out, s[start:i])
		start, i = end, end
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// decodeReplace is bytes.decode("utf-8", "replace") (each invalid byte becomes U+FFFD).
func decodeReplace(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.Write(data[:size])
		}
		data = data[size:]
	}
	return b.String()
}

// pyStr is str(v) for the values an episodic payload holds.
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
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return pyfmt.PyStr(x)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return pyfmt.PyReprValue(items)
	}
	return pyfmt.PyStr(v)
}

// truthy is Python truthiness for payload values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []string:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// get is payload.get(key, default) (ok=false when missing).
func get(payload map[string]any, key string) (any, bool) {
	v, ok := payload[key]
	return v, ok
}

// getStr is str(payload.get(key, def)).
func getStr(payload map[string]any, key, def string) string {
	if v, ok := payload[key]; ok {
		return pyStr(v)
	}
	return def
}

// list is `payload.get(key) or []` as strings (str() of each item).
func list(payload map[string]any, key string) []string {
	v := payload[key]
	if !truthy(v) {
		return nil
	}
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = pyStr(e)
		}
		return out
	}
	return nil
}

// dedupe is list(dict.fromkeys(items)).
func dedupe(items []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
