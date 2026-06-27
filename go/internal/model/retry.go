package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Retry classification + bounded exponential backoff for model HTTP calls (python: retry.py).
//
// Only transient failures are retried (or failed over): HTTP 408/409/429/5xx, timeouts and
// connection/transport errors. Client errors such as 400, 401 and 403 are NOT retried: retrying
// or failing over cannot fix them and would only hide a misconfiguration. A server-supplied
// Retry-After is honoured (capped).

// Default retry/backoff parameters (python defaults).
const (
	DefaultMaxRetries        = 3
	DefaultBaseDelaySeconds  = 1.0
	DefaultMaxDelaySeconds   = 60.0
	DefaultFailoverMaxRounds = 2
)

// SleepFunc waits for the given number of seconds (injectable so tests never really sleep). It
// returns ctx.Err() if the context ends first.
type SleepFunc func(ctx context.Context, seconds float64) error

// ContextSleep is the default SleepFunc: a real, context-aware sleep.
func ContextSleep(ctx context.Context, seconds float64) error {
	if seconds <= 0 || math.IsNaN(seconds) {
		return ctx.Err()
	}
	d := time.Duration(seconds * float64(time.Second))
	if seconds*float64(time.Second) >= math.MaxInt64 {
		d = time.Duration(math.MaxInt64)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// HTTPStatusError is a non-2xx response (python: httpx.HTTPStatusError). Its message is exactly
// httpx's raise_for_status message.
type HTTPStatusError struct {
	Method     string
	URL        string
	StatusCode int
	Reason     string // reason phrase, e.g. "Too Many Requests"
	Header     http.Header
	Body       []byte
}

func (e *HTTPStatusError) Error() string {
	errorType := map[int]string{
		1: "Informational response",
		3: "Redirect response",
		4: "Client error",
		5: "Server error",
	}[e.StatusCode/100]
	if errorType == "" {
		errorType = "Invalid status code"
	}
	msg := fmt.Sprintf("%s '%d %s' for url '%s'\n", errorType, e.StatusCode, e.Reason, e.URL)
	if loc, ok := e.redirectLocation(); ok {
		msg += fmt.Sprintf("Redirect location: '%s'\n", loc)
	}
	return msg + fmt.Sprintf(
		"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d",
		e.StatusCode)
}

func (e *HTTPStatusError) redirectLocation() (string, bool) {
	switch e.StatusCode {
	case 301, 302, 303, 307, 308:
		if vals, ok := e.Header[http.CanonicalHeaderKey("location")]; ok && len(vals) > 0 {
			return strings.Join(vals, ", "), true
		}
	}
	return "", false
}

// transportError is a failure while exchanging bytes with the server (python: httpx.ReadError and
// friends, all httpx.TransportError).
type transportError struct {
	op  string
	err error
}

func (e *transportError) Error() string { return e.op + ": " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// IsRetryable reports whether err is a transient error worth retrying / failing over on:
// HTTP 408/409/429/5xx, timeouts and connection/transport errors. Cancellation of the caller's
// context is never retryable.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var status *HTTPStatusError
	if errors.As(err, &status) {
		s := status.StatusCode
		return s == 408 || s == 409 || s == 429 || s >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var urlErr *url.Error
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var tErr *transportError
	switch {
	case errors.As(err, &urlErr), errors.As(err, &opErr), errors.As(err, &dnsErr),
		errors.As(err, &tErr):
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return true
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ETIMEDOUT):
		return true
	}
	return false
}

// RetryAfterSeconds is the server's Retry-After hint (seconds or HTTP-date) carried by an
// *HTTPStatusError, clamped at 0. ok is false when there is no usable hint.
func RetryAfterSeconds(err error) (seconds float64, ok bool) {
	var status *HTTPStatusError
	if !errors.As(err, &status) || status.Header == nil {
		return 0, false
	}
	vals, present := status.Header[http.CanonicalHeaderKey("retry-after")]
	if !present || len(vals) == 0 {
		return 0, false
	}
	raw := strings.Join(vals, ", ") // httpx joins repeated headers with ", "
	if v, ok := pyFloat(raw); ok {
		return pyMax0(v), true
	}
	when, err2 := parseHTTPDate(raw)
	if err2 != nil {
		return 0, false
	}
	return pyMax0(time.Until(when).Seconds()), true
}

// pyMax0 is Python's max(0.0, v): NaN compares false, so it yields 0.0.
func pyMax0(v float64) float64 {
	if v > 0 {
		return v
	}
	return 0
}

// parseHTTPDate approximates email.utils.parsedate_to_datetime (RFC 5322 dates, plus the other
// HTTP date formats); a zone-less date is taken as UTC.
func parseHTTPDate(raw string) (time.Time, error) {
	if t, err := mail.ParseDate(raw); err == nil {
		return t, nil
	}
	return http.ParseTime(strings.TrimSpace(raw))
}

// pyFloat parses a string the way Python's float() does (surrounding whitespace, underscores
// between digits, inf/infinity/nan); Go-only syntaxes such as hex floats are rejected.
func pyFloat(raw string) (float64, bool) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	if s == "" {
		return 0, false
	}
	lower := strings.ToLower(strings.TrimLeft(s, "+-"))
	switch lower {
	case "inf", "infinity", "nan":
		v, err := strconv.ParseFloat(s, 64)
		return v, err == nil
	}
	if strings.ContainsAny(lower, "xpinfa") {
		return 0, false
	}
	if strings.Contains(s, "_") {
		for i := 0; i < len(s); i++ {
			if s[i] != '_' {
				continue
			}
			if i == 0 || i == len(s)-1 || !isDigit(s[i-1]) || !isDigit(s[i+1]) {
				return 0, false
			}
		}
		s = strings.ReplaceAll(s, "_", "")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
			return v, true // Python overflows to +/-inf and underflows to 0 as well
		}
		return 0, false
	}
	return v, true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// BackoffDelay is the delay in seconds before retry attempt (0-based): the Retry-After hint of
// err if any, else baseSeconds * 2^attempt; capped at maxSeconds.
func BackoffDelay(attempt int, err error, baseSeconds, maxSeconds float64) float64 {
	delay := baseSeconds * math.Pow(2, float64(attempt))
	if err != nil {
		if hinted, ok := RetryAfterSeconds(err); ok {
			delay = hinted
		}
	}
	return math.Min(delay, maxSeconds)
}

// RetryPolicy configures WithRetries. Construct it with DefaultRetryPolicy and override fields.
type RetryPolicy struct {
	MaxRetries       int
	BaseDelaySeconds float64
	MaxDelaySeconds  float64
	Sleep            SleepFunc // nil means ContextSleep
}

// DefaultRetryPolicy is python's with_retries defaults with max_retries=3.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:       DefaultMaxRetries,
		BaseDelaySeconds: DefaultBaseDelaySeconds,
		MaxDelaySeconds:  DefaultMaxDelaySeconds,
	}
}

// WithRetries runs call and retries transient failures up to p.MaxRetries times with backoff.
// Non-retryable errors, and the last error once retries are exhausted, are returned unchanged. A
// context that ends while sleeping ends the loop with ctx.Err().
func WithRetries[T any](ctx context.Context, p RetryPolicy, call func(context.Context) (T, error)) (T, error) {
	sleep := p.Sleep
	if sleep == nil {
		sleep = ContextSleep
	}
	for attempt := 0; ; attempt++ {
		v, err := call(ctx)
		if err == nil {
			return v, nil
		}
		if attempt >= p.MaxRetries || !IsRetryable(err) || ctx.Err() != nil {
			return v, err
		}
		if serr := sleep(ctx, BackoffDelay(attempt, err, p.BaseDelaySeconds, p.MaxDelaySeconds)); serr != nil {
			var zero T
			return zero, serr
		}
	}
}
