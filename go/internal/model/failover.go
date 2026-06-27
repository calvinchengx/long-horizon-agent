package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// DefaultMaxTokenser is implemented by providers that always send a known max_tokens cap (the
// budget meter reserves for it). Python: the provider's default_max_tokens attribute.
type DefaultMaxTokenser interface {
	DefaultMaxTokens() int
}

// FailoverOptions configures NewFailover. Nil pointer fields take the Python defaults
// (max_rounds=2, base_delay_s=1.0, max_delay_s=60.0, real sleep).
type FailoverOptions struct {
	MaxRounds        *int
	BaseDelaySeconds *float64
	MaxDelaySeconds  *float64
	Sleep            SleepFunc
}

// FailoverModel tries providers in order on transient errors (429, 5xx, timeouts, connection
// errors) and returns non-transient ones immediately. After a full round of transient failures it
// backs off (exponentially, honouring Retry-After) and tries again, up to MaxRounds.
//
// Cost is computed by the provider that actually served the turn (Usage.Provider).
type FailoverModel struct {
	providers        []contracts.ModelProvider
	maxRounds        int
	baseDelaySeconds float64
	maxDelaySeconds  float64
	sleep            SleepFunc
	name             string
	defaultMaxTokens int
}

var _ contracts.ModelProvider = (*FailoverModel)(nil)

// NewFailover wraps providers (tried in order).
func NewFailover(providers []contracts.ModelProvider, opts FailoverOptions) (*FailoverModel, error) {
	if len(providers) == 0 {
		return nil, errors.New("FailoverModel needs at least one provider")
	}
	f := &FailoverModel{
		providers:        append([]contracts.ModelProvider(nil), providers...),
		maxRounds:        DefaultFailoverMaxRounds,
		baseDelaySeconds: DefaultBaseDelaySeconds,
		maxDelaySeconds:  DefaultMaxDelaySeconds,
		sleep:            opts.Sleep,
	}
	if opts.MaxRounds != nil {
		f.maxRounds = *opts.MaxRounds
	}
	if f.maxRounds < 1 {
		return nil, errors.New("max_rounds must be >= 1")
	}
	if opts.BaseDelaySeconds != nil {
		f.baseDelaySeconds = *opts.BaseDelaySeconds
	}
	if opts.MaxDelaySeconds != nil {
		f.maxDelaySeconds = *opts.MaxDelaySeconds
	}
	if f.sleep == nil {
		f.sleep = ContextSleep
	}
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name()
		if d, ok := p.(DefaultMaxTokenser); ok {
			f.defaultMaxTokens = max(f.defaultMaxTokens, d.DefaultMaxTokens())
		}
	}
	f.name = "failover:" + strings.Join(names, ",")
	return f, nil
}

// Name is "failover:<p1>,<p2>,...".
func (f *FailoverModel) Name() string { return f.name }

// DefaultMaxTokens is the largest DefaultMaxTokens of the wrapped providers (0 if none has one).
func (f *FailoverModel) DefaultMaxTokens() int { return f.defaultMaxTokens }

// Complete runs the turn on the first provider that succeeds; Usage.Provider names it.
func (f *FailoverModel) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	var lastErr error
	for round := 0; round < f.maxRounds; round++ {
		if round > 0 {
			delay := BackoffDelay(round-1, lastErr, f.baseDelaySeconds, f.maxDelaySeconds)
			if err := f.sleep(ctx, delay); err != nil {
				return contracts.TurnResult{}, err
			}
		}
		for _, p := range f.providers {
			result, err := p.Complete(ctx, messages, tools, maxTokens)
			if err != nil {
				if !IsRetryable(err) || ctx.Err() != nil {
					return contracts.TurnResult{}, err
				}
				lastErr = err
				continue
			}
			if result.Usage.Provider == "" {
				result.Usage.Provider = p.Name()
			}
			return result, nil
		}
	}
	return contracts.TurnResult{}, lastErr
}

func (f *FailoverModel) providerFor(u contracts.Usage) (contracts.ModelProvider, error) {
	for _, p := range f.providers {
		if p.Name() == u.Provider {
			return p, nil
		}
	}
	if len(f.providers) == 1 {
		return f.providers[0], nil
	}
	return nil, &contracts.UnknownPriceError{Message: fmt.Sprintf(
		"cannot attribute usage (provider=%s, model=%s) to any provider of %s",
		contracts.PyRepr(u.Provider), contracts.PyRepr(u.Model), f.name)}
}

// EstimateCostUSD prices usage with the provider that served the turn. Without a provider (a
// pre-call estimate) it is the most expensive provider's cost, a worst-case bound.
func (f *FailoverModel) EstimateCostUSD(u contracts.Usage) (float64, error) {
	if u.Provider == "" {
		var worst float64
		for i, p := range f.providers {
			c, err := p.EstimateCostUSD(u)
			if err != nil {
				return 0, err
			}
			if i == 0 || c > worst {
				worst = c
			}
		}
		return worst, nil
	}
	p, err := f.providerFor(u)
	if err != nil {
		return 0, err
	}
	return p.EstimateCostUSD(u)
}

// Close closes every wrapped provider that implements io.Closer, stopping at the first error
// (python: aclose).
func (f *FailoverModel) Close() error {
	for _, p := range f.providers {
		if c, ok := p.(io.Closer); ok {
			if err := c.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
