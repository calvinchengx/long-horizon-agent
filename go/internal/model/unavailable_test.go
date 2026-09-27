package model

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// A transient error that outlives its retries and fallbacks carries how many calls ended in it
// (python/tests/unit/test_model_unavailable.py).

// timingOutClient times out every request (Ollama under load) and counts them.
func timingOutClient(calls *atomic.Int32) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, os.ErrDeadlineExceeded
	})}
}

func TestRetriesRecordHowManyCallsTimedOut(t *testing.T) {
	var calls atomic.Int32
	m := mustOpenAI(t, OpenAICompatOptions{Client: timingOutClient(&calls), Sleep: (&sleeps{}).sleep})
	_, err := m.Complete(context.Background(), []contracts.ModelMessage{user("hi")}, nil, 0)
	reason, ok := UnavailableReason(err)
	if calls.Load() != 4 || AttemptsOf(err) != 4 || !ok || reason != "model unavailable: ReadTimeout after 4 attempts" {
		t.Fatalf("calls=%d attempts=%d reason=%q ok=%v err=%v", calls.Load(), AttemptsOf(err), reason, ok, err)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) || !IsRetryable(err) {
		t.Fatalf("the counted error must still be the transport error: %v", err)
	}
}

func TestOnlyTransientErrorsAreModelUnavailable(t *testing.T) {
	if _, ok := UnavailableReason(&HTTPStatusError{StatusCode: 401, Reason: "Unauthorized"}); ok {
		t.Fatal("401 is a misconfiguration, not an outage")
	}
	if _, ok := UnavailableReason(errors.New("malformed response")); ok {
		t.Fatal("a plain error is not an outage")
	}
	if reason, ok := UnavailableReason(&HTTPStatusError{StatusCode: 503, Reason: "Service Unavailable"}); !ok ||
		reason != "model unavailable: HTTPStatusError after 1 attempt" {
		t.Fatalf("%q %v", reason, ok)
	}
}

func TestFailoverCountsEveryCallOfTheChain(t *testing.T) {
	var calls atomic.Int32
	member := func() contracts.ModelProvider {
		return mustOpenAI(t, OpenAICompatOptions{Client: timingOutClient(&calls), Sleep: (&sleeps{}).sleep, MaxRetries: Int(1)})
	}
	f, err := NewFailover([]contracts.ModelProvider{member(), member()}, FailoverOptions{Sleep: (&sleeps{}).sleep})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Complete(context.Background(), []contracts.ModelMessage{user("hi")}, nil, 0)
	if calls.Load() != 8 || AttemptsOf(err) != 8 { // 2 rounds x 2 members x 2 calls
		t.Fatalf("calls=%d attempts=%d err=%v", calls.Load(), AttemptsOf(err), err)
	}
}

func TestModelTimeoutIsConfigurable(t *testing.T) {
	for _, backend := range []string{"ollama", "openai_compat"} {
		s, err := config.LoadFrom([]string{"LHA_MODEL_BACKEND=" + backend, "LHA_OPENAI_BASE_URL=http://x.test/v1",
			"LHA_MODEL_TIMEOUT_S=7.5"}, "")
		if err != nil {
			t.Fatal(err)
		}
		p, err := BuildProvider(s, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.(*OpenAICompatModel).http.client.Timeout; got.Seconds() != 7.5 {
			t.Fatalf("%s: timeout %v", backend, got)
		}
	}
	if _, err := config.LoadFrom([]string{"LHA_MODEL_TIMEOUT_S=0"}, ""); err == nil {
		t.Fatal("a zero timeout must be refused")
	}
}
