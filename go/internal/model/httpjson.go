package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// errClientClosed is httpx's error for a request on a closed client.
var errClientClosed = errors.New("Cannot send a request, as the client has been closed.")

// httpClient is an *http.Client the provider either owns (created by it, closed by Close) or
// borrows from the caller (never closed by the provider).
type httpClient struct {
	client *http.Client
	owned  bool
	closed atomic.Bool
}

func newHTTPClient(client *http.Client, timeout time.Duration) *httpClient {
	if client != nil {
		return &httpClient{client: client}
	}
	return &httpClient{
		client: &http.Client{
			Timeout: timeout,
			// httpx does not follow redirects by default: a 3xx is an HTTPStatusError.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		owned: true,
	}
}

// Close releases an owned client's connections; later requests fail as on a closed httpx client.
// A borrowed client belongs to the caller and is left untouched.
func (c *httpClient) Close() error {
	if c.owned {
		c.closed.Store(true)
		c.client.CloseIdleConnections()
	}
	return nil
}

// encodeJSON is httpx's request encoding: compact separators, non-ASCII kept as UTF-8, no HTML
// escaping.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// postJSON POSTs body with retries and returns the (2xx) response body. Non-2xx responses become
// *HTTPStatusError (python: resp.raise_for_status()).
func (c *httpClient) postJSON(ctx context.Context, endpoint string, header http.Header, body []byte, p RetryPolicy) ([]byte, error) {
	return WithRetries(ctx, p, func(ctx context.Context) ([]byte, error) {
		if c.closed.Load() {
			return nil, errClientClosed
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = append([]string(nil), v...)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, &transportError{op: "read response body", err: err}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, &HTTPStatusError{
				Method:     http.MethodPost,
				URL:        req.URL.String(),
				StatusCode: resp.StatusCode,
				Reason:     reasonPhrase(resp),
				Header:     resp.Header,
				Body:       data,
			}
		}
		return data, nil
	})
}

func reasonPhrase(resp *http.Response) string {
	code := strconv.Itoa(resp.StatusCode)
	if r, ok := strings.CutPrefix(resp.Status, code+" "); ok {
		return r
	}
	if resp.Status != "" && resp.Status != code {
		return resp.Status
	}
	return http.StatusText(resp.StatusCode)
}

// decodeJSON decodes a response body keeping numbers exact (json.Number).
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("invalid JSON: extra data after the top-level value")
	}
	return v, nil
}

// plainNumbers converts json.Number values (from decodeJSON) to float64, recursively, so values
// handed to callers (tool arguments) look like ordinary encoding/json output.
func plainNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}
		return f
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = plainNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plainNumbers(e)
		}
		return out
	}
	return v
}

// pyTruthy is Python truthiness for decoded JSON values.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}

// pyIntOr0 is Python's int(v or 0) for a decoded JSON value.
func pyIntOr0(v any) (int, error) {
	if !pyTruthy(v) {
		return 0, nil
	}
	switch x := v.(type) {
	case bool:
		return 1, nil
	case json.Number:
		if n, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return int(n), nil
		}
		f, err := x.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("cannot convert float %s to integer", x.String())
		}
		return int(math.Trunc(f)), nil
	case float64:
		return int(math.Trunc(x)), nil
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(x), "_", "")
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyReprValue(x))
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
}

// pyStr is Python's str() of a decoded JSON value.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		if _, err := strconv.ParseInt(x.String(), 10, 64); err == nil {
			return x.String()
		}
		if f, err := x.Float64(); err == nil {
			return pyFloatRepr(f)
		}
		return x.String()
	case float64:
		return pyFloatRepr(x)
	}
	return pyReprValue(v)
}

// pyReprValue is repr() of a decoded JSON value (enough for error messages).
func pyReprValue(v any) string {
	switch x := v.(type) {
	case string:
		return contracts.PyRepr(x)
	case map[string]any:
		keys := sortedKeys(x)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = contracts.PyRepr(k) + ": " + pyReprValue(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyReprValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return pyStr(v)
}

func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case map[string]any:
		return "dict"
	case []any:
		return "list"
	}
	return "float"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pyFloatRepr is Python's repr() of a float: shortest round-trip digits, positional notation for
// decimal exponents in [-4, 16), scientific ("1e+16", "1.5e-05") otherwise, and ".0" on integers.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64) // e.g. "-1.2345e+06"
	mant, expStr, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expStr)
	sign := ""
	if strings.HasPrefix(mant, "-") {
		sign, mant = "-", mant[1:]
	}
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
	}
	var out string
	switch {
	case exp < 0:
		out = "0." + strings.Repeat("0", -exp-1) + digits
	case exp+1 >= len(digits):
		out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	default:
		out = digits[:exp+1] + "." + digits[exp+1:]
	}
	return sign + out
}

// pyJSONDumps is Python's json.dumps(v) with default arguments: ", " and ": " separators and
// ensure_ascii=True. Map keys are emitted sorted (Go maps have no insertion order). A float64
// with an integral value below 1e16 is written as an integer, since encoding/json decodes JSON
// integers to float64.
func pyJSONDumps(v any) (string, error) {
	var b strings.Builder
	if err := writePyJSON(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writePyJSON(b *strings.Builder, v any) error {
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
		writePyJSONString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		switch {
		case math.IsNaN(x):
			b.WriteString("NaN")
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		case x == math.Trunc(x) && math.Abs(x) < 1e16:
			b.WriteString(strconv.FormatInt(int64(x), 10))
		default:
			b.WriteString(pyFloatRepr(x))
		}
	case map[string]any:
		b.WriteByte('{')
		for i, k := range sortedKeys(x) {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSONString(b, k)
			b.WriteString(": ")
			if err := writePyJSON(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writePyJSON(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		// Any other Go value: normalise through encoding/json first.
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		norm, err := decodeJSON(raw)
		if err != nil {
			return err
		}
		return writePyJSON(b, norm)
	}
	return nil
}

// writePyJSONString mirrors json.encoder.py_encode_basestring_ascii.
func writePyJSONString(b *strings.Builder, s string) {
	const hexd = "0123456789abcdef"
	u4 := func(r rune) {
		b.WriteString(`\u`)
		for shift := 12; shift >= 0; shift -= 4 {
			b.WriteByte(hexd[(r>>uint(shift))&0xf])
		}
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= ' ' && r <= '~':
				b.WriteRune(r)
			case r > 0xffff:
				r -= 0x10000
				u4(0xd800 | (r>>10)&0x3ff)
				u4(0xdc00 | r&0x3ff)
			default:
				u4(r)
			}
		}
	}
	b.WriteByte('"')
}
