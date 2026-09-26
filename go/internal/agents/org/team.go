package org

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"unicode"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Parallel read fan-out (python: lha.agents.team): N read-only sub-agents run concurrently, each
// an isolated investigation, and their condensed briefs come back in query order. Coupled writes
// stay single-threaded on the Lead.
//
// Failures are never silently dropped: a failed researcher comes back with Error set (and is
// logged). Failures no other researcher could fix — authentication / permission errors, budget
// refusals and cancellation — are returned as the fan-out's error.

// ErrMutatingResearchRole is ResearchFanout given a role that can mutate.
var ErrMutatingResearchRole = errors.New("research fan-out requires a read-only role")

func isFatal(err error) bool {
	var budget *governor.BudgetExceeded
	if errors.As(err, &budget) || errors.Is(err, context.Canceled) {
		return true
	}
	var status *model.HTTPStatusError
	return errors.As(err, &status) && (status.StatusCode == 401 || status.StatusCode == 403)
}

// ExcTypeName names an error like Python's type(exc).__name__: a PyTypeName() when the error has
// one, else its Go type name when that reads like an exception class, else "RuntimeError".
func ExcTypeName(err error) string {
	var typed interface{ PyTypeName() string }
	if errors.As(err, &typed) {
		return typed.PyTypeName()
	}
	t := reflect.TypeOf(err)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t != nil && t.Name() != "" && unicode.IsUpper([]rune(t.Name())[0]) {
		return t.Name()
	}
	return "RuntimeError"
}

// ErrorSummary is f"{type(exc).__name__}: {exc}"[:500].
func ErrorSummary(err error) string {
	return pyfmt.Head(ExcTypeName(err)+": "+err.Error(), 500)
}

// ResearchFanout investigates queries in parallel with the researcher role (or role, when
// non-nil): one result per query, failures marked with Error.
func ResearchFanout(ctx context.Context, model contracts.ModelProvider, dispatcher contracts.ToolDispatcher, tctx contracts.ToolContext, queries []string, role *agents.RoleSpec) ([]SubAgentResult, error) {
	spec := agents.Roles["researcher"]
	if role != nil {
		spec = *role
	}
	if spec.AllowMutating {
		return nil, fmt.Errorf("%w; %s can mutate", ErrMutatingResearchRole, contracts.PyRepr(spec.Name))
	}
	results := make([]SubAgentResult, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, query := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = NewSubAgent(spec, model, dispatcher, 0).Run(ctx, query, tctx, "")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			continue
		}
		if isFatal(err) {
			return nil, err // cancellation / auth / budget: stop, don't paper over it
		}
		obs.Logger("lha.agents.team").Warn("researcher failed", "query", pyfmt.Head(queries[i], 120), "error", err.Error())
		results[i] = SubAgentResult{Role: spec.Name, Error: ErrorSummary(err)}
	}
	return results, nil
}
