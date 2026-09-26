package tools

import (
	"context"
	"fmt"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// request_lease lets a parallel implementer ask for a file outside its write-set
// (python/src/lha/execution/tools/leases.py).
//
// The tool does not touch the workspace. It hands (path, reason) to a LeaseHandler (in practice
// coordination.NewLeaseHandler: the orchestrator's decision, committed to the mission anchor)
// and returns the decision as the tool result. WithLeaseTool wraps any ToolDispatcher so the
// tool is served next to the dispatcher's own tools; put it OUTSIDE the ownership guard (the
// guard only checks the tools it wraps).

// RequestLease is the tool's name.
const RequestLease = "request_lease"

// LeaseHandler decides one request: (path, reason) -> (granted, message for the agent).
type LeaseHandler func(ctx context.Context, path, reason string) (bool, string, error)

// LeaseSpec is the request_lease tool spec.
func LeaseSpec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name: RequestLease,
		Description: "Ask for a lease on ONE file outside your write-set, when your item cannot be finished " +
			"without changing it. The orchestrator grants it if nobody else is working on that file " +
			"and refuses otherwise (another open item owns it, or it is a shared or harness file). " +
			"Only write the file after a grant.",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"path", pyfmt.NewOrderedMap("type", "string", "description", "Repo-relative path of the file."),
				"reason", pyfmt.NewOrderedMap("type", "string", "description", "Why your item needs this file."),
			),
			"required":             []any{"path", "reason"},
			"additionalProperties": false,
		},
		// It changes durable mission state (the ownership map): read-only roles never get it.
		Mutating: true,
	}
}

// LeaseToolDispatcher serves request_lease itself and delegates everything else.
type LeaseToolDispatcher struct {
	inner   contracts.ToolDispatcher
	handler LeaseHandler
}

var (
	_ contracts.ToolDispatcher = (*LeaseToolDispatcher)(nil)
	_ contracts.EventDrainer   = (*LeaseToolDispatcher)(nil)
)

// Inner is the wrapped dispatcher.
func (d *LeaseToolDispatcher) Inner() contracts.ToolDispatcher { return d.inner }

// Specs is the inner specs plus request_lease.
func (d *LeaseToolDispatcher) Specs() []contracts.ToolSpec {
	return append(append([]contracts.ToolSpec{}, d.inner.Specs()...), LeaseSpec())
}

// DrainEvents forwards the wrapped dispatcher's gate events.
func (d *LeaseToolDispatcher) DrainEvents() []contracts.EventRecord {
	if drainer, ok := d.inner.(contracts.EventDrainer); ok {
		return append([]contracts.EventRecord{}, drainer.DrainEvents()...)
	}
	return []contracts.EventRecord{}
}

// Dispatch serves request_lease (validated like the AllowListDispatcher does) or delegates.
func (d *LeaseToolDispatcher) Dispatch(ctx context.Context, call contracts.ToolCall, tctx contracts.ToolContext) (result contracts.ToolResult) {
	if call.Name != RequestLease {
		return d.inner.Dispatch(ctx, call, tctx)
	}
	arguments := call.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	if errText := execution.ValidateArguments(LeaseSpec().Parameters, arguments, ""); errText != "" {
		return contracts.Failure("invalid args for " + contracts.PyRepr(RequestLease) + ": " + errText)
	}
	path := pystr.Strip(strArg(arguments, "path"))
	reason := pystr.Strip(strArg(arguments, "reason"))
	if path == "" || reason == "" {
		return contracts.Failure("request_lease needs a non-empty path and reason")
	}
	defer func() {
		if r := recover(); r != nil { // the loop never crashes on a tool error
			result = contracts.Failure(fmt.Sprintf("lease request failed: RuntimeError: %v", r))
		}
	}()
	granted, message, err := d.handler(ctx, path, reason)
	if err != nil {
		return contracts.Failure("lease request failed: " + pyval.ExcTypeName(err) + ": " + err.Error())
	}
	if granted {
		return contracts.Success(message)
	}
	return contracts.Failure(message)
}

// WithLeaseTool returns dispatcher plus request_lease bound to handler (unchanged if it already
// offers the tool).
func WithLeaseTool(dispatcher contracts.ToolDispatcher, handler LeaseHandler) contracts.ToolDispatcher {
	for _, spec := range dispatcher.Specs() {
		if spec.Name == RequestLease {
			return dispatcher
		}
	}
	return &LeaseToolDispatcher{inner: dispatcher, handler: handler}
}
