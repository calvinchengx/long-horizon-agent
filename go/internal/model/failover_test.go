package model

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// raises is a provider that always fails with err.
type raises struct {
	name   string
	err    error
	calls  int
	cost   float64
	closed int
}

func (r *raises) Name() string { return r.name }
func (r *raises) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	r.calls++
	return contracts.TurnResult{}, r.err
}
func (r *raises) EstimateCostUSD(contracts.Usage) (float64, error) { return r.cost, nil }
func (r *raises) Close() error                                     { r.closed++; return nil }
func (r *raises) DefaultMaxTokens() int                            { return 777 }

func mustFailover(t *testing.T, ps []contracts.ModelProvider, o FailoverOptions) *FailoverModel {
	t.Helper()
	f, err := NewFailover(ps, o)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFailoverRaisesImmediatelyOnAuthError(t *testing.T) {
	backup := NewStub([]contracts.TurnResult{{Text: "backup"}})
	primary := &raises{name: "raises", err: statusError(401)}
	f := mustFailover(t, []contracts.ModelProvider{primary, backup}, FailoverOptions{Sleep: (&sleeps{}).sleep})
	_, err := f.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	var status *HTTPStatusError
	if !errors.As(err, &status) || primary.calls != 1 || backup.Turns() != 0 {
		t.Errorf("err=%v calls=%d", err, primary.calls)
	}
}

func TestFailoverBacksOffBetweenRounds(t *testing.T) {
	primary := &raises{name: "p1", err: statusError(429, "retry-after", "3")}
	s := &sleeps{}
	f := mustFailover(t, []contracts.ModelProvider{primary}, FailoverOptions{MaxRounds: Int(3), Sleep: s.sleep})
	_, err := f.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	if !errors.Is(err, primary.err) || AttemptsOf(err) != 3 || primary.calls != 3 || !reflect.DeepEqual(s.calls, []float64{3, 3}) {
		t.Errorf("err=%v calls=%d sleeps=%v", err, primary.calls, s.calls)
	}
}

func TestFailoverExponentialBackoffWithoutHint(t *testing.T) {
	primary := &raises{name: "p1", err: statusError(503)}
	s := &sleeps{}
	f := mustFailover(t, []contracts.ModelProvider{primary}, FailoverOptions{
		MaxRounds: Int(4), BaseDelaySeconds: Float(0.5), MaxDelaySeconds: Float(1.5), Sleep: s.sleep})
	_, _ = f.Complete(ctx, nil, nil, 0)
	if !reflect.DeepEqual(s.calls, []float64{0.5, 1, 1.5}) {
		t.Errorf("sleeps = %v", s.calls)
	}
	// A cancelled sleep ends the loop.
	f = mustFailover(t, []contracts.ModelProvider{primary}, FailoverOptions{
		Sleep: func(context.Context, float64) error { return context.Canceled }})
	if _, err := f.Complete(ctx, nil, nil, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestFailoverPricesByServingProvider(t *testing.T) {
	primary := &raises{name: "expensive", err: statusError(503), cost: 100}
	backup := NewStub([]contracts.TurnResult{{Text: "backup", Usage: contracts.Usage{InputTokens: 5}}})
	f := mustFailover(t, []contracts.ModelProvider{primary, backup}, FailoverOptions{Sleep: (&sleeps{}).sleep})
	result, err := f.Complete(ctx, []contracts.ModelMessage{user("hi")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.Provider != backup.Name() {
		t.Errorf("provider = %q", result.Usage.Provider)
	}
	if c, err := f.EstimateCostUSD(result.Usage); c != 0 || err != nil {
		t.Errorf("cost = %v, %v (the stub's price, not the primary's)", c, err)
	}
	// Pre-call estimate (no provider): the most expensive provider bounds it.
	if c, _ := f.EstimateCostUSD(contracts.Usage{}); c != 100 {
		t.Errorf("worst case = %v", c)
	}
	// A provider already set by the serving provider is kept.
	pre := NewStub([]contracts.TurnResult{{Usage: contracts.Usage{Provider: "inner"}}})
	f2 := mustFailover(t, []contracts.ModelProvider{pre}, FailoverOptions{})
	r, _ := f2.Complete(ctx, nil, nil, 0)
	if r.Usage.Provider != "inner" {
		t.Errorf("provider = %q", r.Usage.Provider)
	}
	// Single provider: any usage is attributed to it.
	if c, err := f2.EstimateCostUSD(r.Usage); c != 0 || err != nil {
		t.Errorf("single = %v, %v", c, err)
	}
}

func TestFailoverUnattributableUsage(t *testing.T) {
	a, b := &raises{name: "a"}, &raises{name: "b"}
	f := mustFailover(t, []contracts.ModelProvider{a, b}, FailoverOptions{})
	_, err := f.EstimateCostUSD(contracts.Usage{Provider: "zzz", Model: "m"})
	if !errors.Is(err, contracts.ErrUnknownPrice) ||
		err.Error() != "cannot attribute usage (provider='zzz', model='m') to any provider of failover:a,b" {
		t.Errorf("err = %v", err)
	}
	// A pricing error during the worst-case estimate propagates.
	un := mustOpenAI(t, OpenAICompatOptions{})
	f = mustFailover(t, []contracts.ModelProvider{a, un}, FailoverOptions{})
	if _, err := f.EstimateCostUSD(contracts.Usage{}); !errors.Is(err, contracts.ErrUnknownPrice) {
		t.Errorf("err = %v", err)
	}
}

func TestFailoverConstructionAndMetadata(t *testing.T) {
	if _, err := NewFailover(nil, FailoverOptions{}); err == nil || err.Error() != "FailoverModel needs at least one provider" {
		t.Errorf("err = %v", err)
	}
	if _, err := NewFailover([]contracts.ModelProvider{NewStub(nil)}, FailoverOptions{MaxRounds: Int(0)}); err == nil ||
		err.Error() != "max_rounds must be >= 1" {
		t.Errorf("err = %v", err)
	}
	a := &raises{name: "a"}
	f := mustFailover(t, []contracts.ModelProvider{NewStub(nil), a}, FailoverOptions{})
	if f.Name() != "failover:stub:stub-1,a" || f.DefaultMaxTokens() != 777 {
		t.Errorf("name=%q max=%d", f.Name(), f.DefaultMaxTokens())
	}
	if err := f.Close(); err != nil || a.closed != 1 {
		t.Errorf("close: %v %d", err, a.closed)
	}
	if mustFailover(t, []contracts.ModelProvider{NewStub(nil)}, FailoverOptions{}).DefaultMaxTokens() != 0 {
		t.Error("stub has no default max tokens")
	}
}

type failClose struct{ raises }

func (f *failClose) Close() error { return errors.New("close failed") }

func TestFailoverCloseStopsAtFirstError(t *testing.T) {
	bad := &failClose{raises{name: "bad"}}
	after := &raises{name: "after"}
	f := mustFailover(t, []contracts.ModelProvider{bad, after}, FailoverOptions{})
	if err := f.Close(); err == nil || after.closed != 0 {
		t.Errorf("err=%v after.closed=%d", err, after.closed)
	}
}
