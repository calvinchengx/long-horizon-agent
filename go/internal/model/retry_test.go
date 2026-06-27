package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestRetryClassification(t *testing.T) {
	for _, s := range []int{408, 409, 429, 500, 503, 529} {
		if !IsRetryable(statusError(s)) {
			t.Errorf("status %d should be retryable", s)
		}
	}
	for _, s := range []int{400, 401, 403, 404, 422} {
		if IsRetryable(statusError(s)) {
			t.Errorf("status %d should not be retryable", s)
		}
	}
	retryable := []error{
		&url.Error{Op: "Post", URL: "http://x", Err: errors.New("dial tcp: connection refused")},
		&net.OpError{Op: "dial", Err: errors.New("down")},
		&net.DNSError{Err: "no such host", Name: "x"},
		&transportError{op: "read", err: io.ErrUnexpectedEOF},
		context.DeadlineExceeded,
		io.ErrUnexpectedEOF,
		syscall.ECONNREFUSED,
		fmt.Errorf("wrapped: %w", syscall.ECONNRESET),
		fmt.Errorf("wrapped: %w", statusError(503)),
	}
	for _, err := range retryable {
		if !IsRetryable(err) {
			t.Errorf("%v should be retryable", err)
		}
	}
	for _, err := range []error{nil, errors.New("bug"), context.Canceled,
		&url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}} {
		if IsRetryable(err) {
			t.Errorf("%v should not be retryable", err)
		}
	}
}

func TestRetryAfterIsHonouredAndCapped(t *testing.T) {
	if v, ok := RetryAfterSeconds(statusError(429, "retry-after", "7")); !ok || v != 7 {
		t.Errorf("RetryAfterSeconds = %v, %v", v, ok)
	}
	if got := BackoffDelay(0, statusError(429, "retry-after", "7"), 1, 60); got != 7 {
		t.Errorf("BackoffDelay = %v", got)
	}
	if got := BackoffDelay(0, statusError(429, "retry-after", "9999"), 1, 60); got != 60 {
		t.Errorf("BackoffDelay = %v", got)
	}
	if got := BackoffDelay(3, statusError(503), 1, 60); got != 8 {
		t.Errorf("BackoffDelay = %v", got)
	}
	if got := BackoffDelay(2, nil, 0.5, 60); got != 2 {
		t.Errorf("BackoffDelay = %v", got)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{" 2.5 ", 2.5, true},
		{"-3", 0, true},
		{"1_0", 10, true},
		{"nan", 0, true},
		{"inf", math.Inf(1), true},
		{"1e3", 1000, true},
		{"0x10", 0, false},
		{"_1", 0, false},
		{"soon", 0, false},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0, true}, // in the past => clamped to 0
	}
	for _, c := range cases {
		got, ok := RetryAfterSeconds(statusError(429, "retry-after", c.raw))
		if ok != c.ok || got != c.want {
			t.Errorf("RetryAfterSeconds(%q) = %v, %v; want %v, %v", c.raw, got, ok, c.want, c.ok)
		}
	}
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got, ok := RetryAfterSeconds(statusError(429, "retry-after", future)); !ok || got < 80 || got > 91 {
		t.Errorf("future date = %v, %v", got, ok)
	}
	if _, ok := RetryAfterSeconds(errors.New("x")); ok {
		t.Error("non-HTTP error has no hint")
	}
	if _, ok := RetryAfterSeconds(statusError(429)); ok {
		t.Error("no header, no hint")
	}
}

func TestHTTPStatusErrorMessageMatchesHTTPX(t *testing.T) {
	e := &HTTPStatusError{URL: "http://x/v1/chat/completions", StatusCode: 429, Reason: "Too Many Requests"}
	want := "Client error '429 Too Many Requests' for url 'http://x/v1/chat/completions'\n" +
		"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/429"
	if e.Error() != want {
		t.Errorf("got %q", e.Error())
	}
	e = &HTTPStatusError{URL: "http://x/", StatusCode: 503, Reason: "Service Unavailable"}
	if e.Error()[:39] != "Server error '503 Service Unavailable' " {
		t.Errorf("got %q", e.Error())
	}
	e = &HTTPStatusError{URL: "http://x/", StatusCode: 302, Reason: "Found", Header: http.Header{"Location": {"http://y/"}}}
	want = "Redirect response '302 Found' for url 'http://x/'\nRedirect location: 'http://y/'\n" +
		"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/302"
	if e.Error() != want {
		t.Errorf("got %q", e.Error())
	}
	e = &HTTPStatusError{URL: "http://x/", StatusCode: 600, Reason: ""}
	if e.Error()[:25] != "Invalid status code '600 " {
		t.Errorf("got %q", e.Error())
	}
	e = &HTTPStatusError{URL: "http://x/", StatusCode: 101, Reason: "Switching Protocols"}
	if e.Error()[:23] != "Informational response " {
		t.Errorf("got %q", e.Error())
	}
}

func TestWithRetries(t *testing.T) {
	ctx := context.Background()
	s := &sleeps{}
	n := 0
	v, err := WithRetries(ctx, RetryPolicy{MaxRetries: 3, BaseDelaySeconds: 1, MaxDelaySeconds: 60, Sleep: s.sleep},
		func(context.Context) (int, error) {
			n++
			if n < 3 {
				return 0, statusError(503)
			}
			return 42, nil
		})
	if err != nil || v != 42 || n != 3 || !reflect.DeepEqual(s.calls, []float64{1, 2}) {
		t.Errorf("v=%v err=%v n=%d sleeps=%v", v, err, n, s.calls)
	}

	// A context that ends while sleeping stops the loop.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = WithRetries(cctx, DefaultRetryPolicy(), func(context.Context) (int, error) {
		return 0, statusError(503)
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	_, err = WithRetries(ctx, RetryPolicy{MaxRetries: 1, Sleep: func(context.Context, float64) error { return context.Canceled }},
		func(context.Context) (int, error) { return 0, statusError(503) })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestContextSleep(t *testing.T) {
	if err := ContextSleep(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := ContextSleep(context.Background(), 0.001); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ContextSleep(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if err := ContextSleep(ctx, math.Inf(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
