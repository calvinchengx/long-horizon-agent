package hitl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Gate notifications (python/src/lha/hitl/notify.py): an optional webhook that receives every
// gate event as a JSON POST. Off unless LHA_GATE_WEBHOOK_URL is set. Delivery is best-effort and
// never fails the gate: a slow or failing receiver delays a gate event by at most the timeout and
// can never approve, deny or stall anything. The URL (a secret: it often embeds a token) never
// appears in an outcome or a log line — failures are reported by an exception-style type name.

const (
	// WebhookOff is the outcome when no URL is configured.
	WebhookOff = "off"
	// WebhookSent is the outcome of a 2xx response.
	WebhookSent = "sent"
)

// Field is one key of a Payload.
type Field struct {
	Key   string
	Value any // string, []string, int or bool
}

// Payload is a JSON object with Python dict (insertion) key order.
type Payload []Field

// Get returns the value of key (nil when absent).
func (p Payload) Get(key string) any {
	for _, f := range p {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// JSON is the body httpx sends for client.post(url, json=payload): json.dumps(payload,
// ensure_ascii=False, separators=(",", ":"), allow_nan=False) encoded as UTF-8.
func (p Payload) JSON() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSONString(&b, f.Key)
		b.WriteByte(':')
		writeJSONValue(&b, f.Value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func writeJSONValue(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case string:
		writeJSONString(b, x)
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, s)
		}
		b.WriteByte(']')
	case int:
		b.WriteString(strconv.Itoa(x))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case Payload: // a nested object (e.g. a durable gate notice's request)
		b.Write(x.JSON())
	case nil:
		b.WriteString("null")
	default:
		writeJSONString(b, fmt.Sprint(x))
	}
}

// writeJSONString is Python's json string encoding with ensure_ascii=False: only '"', '\\' and
// the C0 controls are escaped.
func writeJSONString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
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
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Notify delivers one gate event; it never fails (the outcome is informational).
type Notify func(payload Payload) string

// PostWebhook POSTs payload as JSON and returns "off" (no URL), "sent" (2xx) or "failed: <why>"
// (python: post_webhook_sync). Like httpx it does not follow redirects. transport nil = the
// default transport.
func PostWebhook(rawURL string, payload Payload, timeout time.Duration, transport http.RoundTripper) (outcome string) {
	if rawURL == "" {
		return WebhookOff
	}
	defer func() {
		if r := recover(); r != nil {
			outcome = "failed: RuntimeError"
		}
	}()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "failed: InvalidURL"
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "failed: UnsupportedProtocol"
	}
	if parsed.Host == "" {
		return "failed: InvalidURL"
	}
	client := &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(payload.JSON()))
	if err != nil {
		return "failed: InvalidURL"
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "failed: " + transportErrorName(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return WebhookSent
	}
	return "failed: HTTP " + strconv.Itoa(resp.StatusCode)
}

// transportErrorName names a transport failure like the httpx exception Python would see.
func transportErrorName(err error) string {
	var op *net.OpError
	dial := errors.As(err, &op) && op.Op == "dial"
	timeout := errors.Is(err, context.DeadlineExceeded)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		timeout = true
	}
	switch {
	case dial && timeout:
		return "ConnectTimeout"
	case dial:
		return "ConnectError"
	case timeout:
		return "ReadTimeout"
	case strings.Contains(err.Error(), "malformed HTTP"):
		return "RemoteProtocolError"
	}
	return "ReadError"
}

// WebhookNotifier is a Notify posting to rawURL with timeoutS seconds (nil when rawURL is "").
func WebhookNotifier(rawURL string, timeoutS float64) Notify {
	if rawURL == "" {
		return nil
	}
	timeout := time.Duration(timeoutS * float64(time.Second))
	return func(payload Payload) string { return PostWebhook(rawURL, payload, timeout, nil) }
}
