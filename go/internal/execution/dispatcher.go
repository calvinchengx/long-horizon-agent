package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// AllowListDispatcher is the safety gate every tool call passes through
// (python/src/lha/execution/dispatcher.py).
//
// It enforces, in code (never via a prompt that can be injected):
//   - an explicit allow-list of tool names (registered AND named in Allow; empty => nothing);
//   - mutating-tool and egress (network) policy, both default-deny;
//   - argument validation against each tool's JSON Schema (required keys AND types);
//   - workspace path containment for PathArgs, and no mutation of the harness-owned .lha/ and
//     .git/ directories;
//   - human gates: commands safety.ClassifyCommand gates are sent to the configured HITLGate;
//     with no gate they are DENIED (fail closed). Every answer is kept as a tool_approval event
//     (DrainEvents) that the agent loop commits with the checkpoint;
//   - Meta's Rule of Two: the dispatcher derives its capability set from its config and refuses
//     to be built holding the full trifecta unless a gate is present (then every egress call is
//     gated);
//
// and guarantees the agent loop never crashes on a tool error (failures return as ToolResult).
type AllowListDispatcher struct {
	order         []string // registration order (first occurrence of each name)
	tools         map[string]contracts.Tool
	allow         map[string]struct{}
	allowMutating bool
	allowEgress   bool
	gate          contracts.HITLGate
	events        []contracts.EventRecord
	capabilities  map[safety.Capability]struct{}
	gateAllEgress bool
}

var (
	_ contracts.ToolDispatcher = (*AllowListDispatcher)(nil)
	_ contracts.EventDrainer   = (*AllowListDispatcher)(nil)
)

// DispatcherOptions configures an AllowListDispatcher.
type DispatcherOptions struct {
	// Allow names the tools that may be called. FAIL-CLOSED: empty allows nothing (ForTools
	// allows every registered tool instead).
	Allow []string
	// AllowMutating permits tools flagged Mutating (default false: read-only).
	AllowMutating bool
	// AllowEgress permits tools flagged Egress (default-deny).
	AllowEgress bool
	// Gate receives irreversible commands (and, under the trifecta, egress calls); nil means
	// such calls are denied.
	Gate contracts.HITLGate
	// Capabilities the caller knows the session holds beyond what the tools imply — typically
	// safety.PrivateData when the workspace or the sandbox exposes secrets / customer data.
	Capabilities []safety.Capability
}

// NewAllowListDispatcher builds a dispatcher over tools (registration is itself an allow-list).
// It returns safety.ErrRuleOfTwoViolation if the derived capability set is the full trifecta and
// no gate is set.
func NewAllowListDispatcher(tools []contracts.Tool, opts DispatcherOptions) (*AllowListDispatcher, error) {
	d := &AllowListDispatcher{
		tools:         map[string]contracts.Tool{},
		allow:         map[string]struct{}{},
		allowMutating: opts.AllowMutating,
		allowEgress:   opts.AllowEgress,
		gate:          opts.Gate,
		capabilities:  map[safety.Capability]struct{}{},
	}
	for _, t := range tools {
		name := t.Spec().Name
		if _, seen := d.tools[name]; !seen {
			d.order = append(d.order, name)
		}
		d.tools[name] = t
	}
	for _, name := range opts.Allow {
		d.allow[name] = struct{}{}
	}
	for _, c := range opts.Capabilities {
		d.capabilities[c] = struct{}{}
	}
	for _, c := range d.derivedCapabilities() {
		d.capabilities[c] = struct{}{}
	}
	caps := d.Capabilities()
	if opts.Gate == nil {
		if err := safety.CheckRuleOfTwo(caps...); err != nil {
			return nil, err
		}
	}
	d.gateAllEgress = !safety.Permits(caps...)
	return d, nil
}

// ForTools allows every tool in tools by name; the mutating policy must still be stated
// (opts.Allow is ignored).
func ForTools(tools []contracts.Tool, opts DispatcherOptions) (*AllowListDispatcher, error) {
	opts.Allow = nil
	for _, t := range tools {
		opts.Allow = append(opts.Allow, t.Spec().Name)
	}
	return NewAllowListDispatcher(tools, opts)
}

// Capabilities is the dispatcher's Rule-of-Two capability set (sorted).
func (d *AllowListDispatcher) Capabilities() []safety.Capability {
	out := make([]safety.Capability, 0, len(d.capabilities))
	for c := range d.capabilities {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (d *AllowListDispatcher) derivedCapabilities() []safety.Capability {
	var caps []safety.Capability
	for _, name := range d.order {
		spec := d.tools[name].Spec()
		if !d.usable(name, spec) {
			continue
		}
		if spec.Egress {
			caps = append(caps, safety.ExternalComms)
		}
		if spec.UntrustedInput {
			caps = append(caps, safety.UntrustedContent)
		}
	}
	return caps
}

func (d *AllowListDispatcher) permitted(name string) bool {
	_, ok := d.allow[name]
	return ok
}

func (d *AllowListDispatcher) usable(name string, spec contracts.ToolSpec) bool {
	return d.permitted(name) && (d.allowMutating || !spec.Mutating) && (d.allowEgress || !spec.Egress)
}

// DrainEvents returns the gate events since the last drain (decisions, then the gate's own
// events when it is an EventDrainer), oldest first. The agent loop commits them with the
// cycle's checkpoint (.lha/events.ndjson).
func (d *AllowListDispatcher) DrainEvents() []contracts.EventRecord {
	events := d.events
	d.events = nil
	if drainer, ok := d.gate.(contracts.EventDrainer); ok && d.gate != nil {
		events = append(events, drainer.DrainEvents()...)
	}
	if events == nil {
		events = []contracts.EventRecord{}
	}
	return events
}

// Specs are the specs of the permitted tools (what the model is told it can call).
func (d *AllowListDispatcher) Specs() []contracts.ToolSpec {
	specs := []contracts.ToolSpec{}
	for _, name := range d.order {
		if d.permitted(name) {
			specs = append(specs, d.tools[name].Spec())
		}
	}
	return specs
}

// Dispatch validates and executes a tool call; it never panics or errors (failures come back as
// a ToolResult).
func (d *AllowListDispatcher) Dispatch(ctx context.Context, call contracts.ToolCall, tctx contracts.ToolContext) (result contracts.ToolResult) {
	tool, ok := d.tools[call.Name]
	if !ok {
		return contracts.Failure("unknown tool: " + contracts.PyRepr(call.Name))
	}
	if !d.permitted(call.Name) {
		return contracts.Failure("tool not allowed: " + contracts.PyRepr(call.Name))
	}
	spec := tool.Spec()
	if spec.Mutating && !d.allowMutating {
		return contracts.Failure("mutating tools are disabled: " + contracts.PyRepr(call.Name))
	}
	if spec.Egress && !d.allowEgress {
		return contracts.Failure("egress is disabled (default-deny): " + contracts.PyRepr(call.Name))
	}
	arguments := call.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	if missing := MissingRequired(spec.Parameters, arguments); len(missing) > 0 {
		return contracts.Failure(fmt.Sprintf("missing required args for %s: %s",
			contracts.PyRepr(call.Name), pyval.Repr(missing)))
	}
	if typeErr := ValidateArguments(spec.Parameters, arguments, ""); typeErr != "" {
		return contracts.Failure(fmt.Sprintf("invalid args for %s: %s", contracts.PyRepr(call.Name), typeErr))
	}
	if pathErr := checkPaths(spec, arguments, tctx); pathErr != "" {
		return contracts.Failure(pathErr)
	}
	if reason, gated := d.gateReason(spec, arguments, tctx); gated {
		if denied := d.askGate(ctx, call, arguments, tctx, reason); denied != "" {
			return contracts.Failure(denied)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			result = contracts.Failure(fmt.Sprintf("RuntimeError: %v", r))
		}
	}()
	return tool.Run(ctx, arguments, tctx)
}

func (d *AllowListDispatcher) gateReason(spec contracts.ToolSpec, arguments map[string]any, tctx contracts.ToolContext) (string, bool) {
	if spec.CommandArg != nil {
		if argv, ok := asList(arguments[*spec.CommandArg]); ok {
			tokens := make([]string, len(argv))
			for i, t := range argv {
				tokens[i] = pyval.Str(t)
			}
			// A sandbox whose workdir is not the host checkout (Docker, E2B) has its own /tmp.
			scope := safety.Scope{}
			if tctx.Session != nil {
				workdir := tctx.Session.Workdir()
				scope = safety.Scope{Workspace: workdir, PrivateTmp: contracts.HostRoot(tctx.Session) != workdir}
			}
			if reason, gated := safety.ClassifyCommandIn(tokens, scope); gated {
				return reason, true
			}
		}
	}
	if spec.Egress && d.gateAllEgress {
		return "egress while holding untrusted input + private data + external comms", true
	}
	return "", false
}

// askGate returns "" if a human approved, else the denial message (fail closed).
func (d *AllowListDispatcher) askGate(ctx context.Context, call contracts.ToolCall, arguments map[string]any, tctx contracts.ToolContext, reason string) string {
	gctx := map[string]string{
		"tool":        call.Name,
		"arguments":   pyval.Head(pyval.Repr(arguments), 2000),
		"reason":      reason,
		"fingerprint": ActionFingerprint(call.Name, arguments),
	}
	spec := d.tools[call.Name].Spec()
	if spec.CommandArg != nil {
		if argv, ok := asList(arguments[*spec.CommandArg]); ok { // the exact command, for a human
			tokens := make([]any, len(argv))
			for i, t := range argv {
				tokens[i] = pyval.Str(t)
			}
			gctx["argv"] = pyval.JSONDumps(tokens, false)
		}
	}
	id := call.ID
	if id == "" {
		id = call.Name
	}
	request := contracts.GateRequest{
		GateID:        tctx.MissionID + ":tool:" + id,
		Question:      fmt.Sprintf("Allow %s? %s", contracts.PyRepr(call.Name), reason),
		Risk:          contracts.RiskIrreversible,
		DefaultAction: contracts.GateReject,
		Options:       []contracts.GateDecision{contracts.GateApprove, contracts.GateReject},
		Context:       gctx,
	}
	if d.gate == nil {
		d.record(request, nil, "no human gate configured")
		return "irreversible action denied (no human gate configured): " + reason
	}
	resolution, err := d.requestGate(ctx, request)
	if err != nil {
		name := pyval.ExcTypeName(err)
		d.record(request, nil, "gate error: "+name)
		return fmt.Sprintf("irreversible action denied (gate error: %s): %s", name, reason)
	}
	d.record(request, &resolution, "")
	if resolution.ResolvedBy == contracts.PendingApproval {
		return "queued for human approval: " + reason + ". It is NOT done. An operator will be asked; " +
			"if approved, this exact call is allowed in a later cycle. Continue with other " +
			"work meanwhile and do not try to work around the gate."
	}
	if resolution.Decision != contracts.GateApprove {
		how := "by default"
		if !resolution.Defaulted {
			by := resolution.ResolvedBy
			if by == "" {
				by = "gate"
			}
			how = "by " + by
		}
		return fmt.Sprintf("irreversible action denied (%s %s): %s", resolution.Decision, how, reason)
	}
	return ""
}

func (d *AllowListDispatcher) requestGate(ctx context.Context, req contracts.GateRequest) (res contracts.GateResolution, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = pyval.NewError("RuntimeError", fmt.Sprint(r))
		}
	}()
	return d.gate.Request(ctx, req)
}

// record keeps the gate's answer as a tool_approval event (secrets redacted).
func (d *AllowListDispatcher) record(req contracts.GateRequest, resolution *contracts.GateResolution, failure string) {
	decision := string(contracts.GateReject)
	resolvedBy := failure
	defaulted := true
	if resolution != nil {
		decision = string(resolution.Decision)
		if resolution.ResolvedBy == contracts.PendingApproval {
			decision = "pending"
		}
		resolvedBy = resolution.ResolvedBy
		defaulted = resolution.Defaulted
	}
	d.events = append(d.events, contracts.EventRecord{
		Kind: "tool_approval",
		Payload: contracts.Payload(
			"tool", req.Context["tool"],
			"arguments", obs.RedactText(req.Context["arguments"]),
			"reason", req.Context["reason"],
			"fingerprint", req.Context["fingerprint"],
			"decision", decision,
			"approved", decision == string(contracts.GateApprove),
			"resolved_by", resolvedBy,
			"defaulted", defaulted,
		),
	})
}

// ActionFingerprint is a stable id for one exact tool call: the first 32 hex chars of the SHA-256
// of the canonical JSON {"arguments": ..., "tool": ...} (python: lha.hitl.approvals).
func ActionFingerprint(tool string, arguments map[string]any) string {
	if arguments == nil {
		arguments = map[string]any{}
	}
	canonical := pyval.JSONDumps(map[string]any{"tool": tool, "arguments": arguments}, true)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:32]
}

// checkPaths enforces containment for every path argument; mutating tools may not touch .lha/
// or .git/. It returns the failure message, or "".
func checkPaths(spec contracts.ToolSpec, arguments map[string]any, tctx contracts.ToolContext) string {
	localRoot := ""
	if tctx.Session != nil {
		localRoot = contracts.HostRoot(tctx.Session)
	}
	st, err := os.Stat(localRoot)
	hostLocal := localRoot != "" && err == nil && st.IsDir()
	for _, name := range spec.PathArgs {
		value, ok := arguments[name].(string)
		if !ok || value == "" {
			continue
		}
		var protected bool
		if hostLocal {
			protected, err = IsProtectedResolved(localRoot, value)
		} else {
			protected, err = IsProtected(value)
		}
		if err != nil {
			return fmt.Sprintf("path not allowed for %s: %s", contracts.PyRepr(spec.Name), err.Error())
		}
		if protected && spec.Mutating {
			return fmt.Sprintf("%s may not modify harness-owned path %s (.lha/ or .git/)",
				contracts.PyRepr(spec.Name), contracts.PyRepr(value))
		}
	}
	return ""
}

// MissingRequired returns the required parameter names (per JSON Schema) absent from arguments,
// sorted.
func MissingRequired(parameters map[string]any, arguments map[string]any) []string {
	required, ok := parameters["required"].([]any)
	if !ok {
		if strs, isStrs := parameters["required"].([]string); isStrs {
			for _, s := range strs {
				required = append(required, s)
			}
		} else {
			return nil
		}
	}
	set := map[string]struct{}{}
	for _, name := range required {
		key, isStr := name.(string)
		if _, present := arguments[key]; !isStr || !present {
			set[pyval.Str(name)] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var jsonTypes = map[string]func(any) bool{
	"string":  func(v any) bool { _, ok := v.(string); return ok },
	"integer": pyval.IsInt,
	"number":  pyval.IsNumber,
	"boolean": func(v any) bool { _, ok := v.(bool); return ok },
	"array":   func(v any) bool { _, ok := asList(v); return ok },
	"object":  func(v any) bool { _, ok := asDict(v); return ok },
	"null":    func(v any) bool { return v == nil },
}

func asList(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

func asDict(v any) (map[string]any, bool) {
	switch x := v.(type) {
	case map[string]any:
		return x, true
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, s := range x {
			out[k] = s
		}
		return out, true
	case *pyfmt.OrderedMap: // a tool's "properties" in declaration order
		out := make(map[string]any, len(x.Values))
		for k, v := range x.Values {
			out[k] = v
		}
		return out, true
	}
	return nil, false
}

// ValidateArguments validates value against the JSON-Schema subset tools use and returns the
// first error ("" when valid). Supports type (a bool is never an integer/number), properties,
// required (nested objects only: the top level is MissingRequired's job), additionalProperties
// (bool or schema), items, enum, minimum/maximum. where is the value's path ("" = the
// arguments object).
//
// Object members are visited in sorted key order (Python visits them in insertion order, which a
// Go map does not keep): with several invalid members the first error reported can differ.
func ValidateArguments(schema map[string]any, value any, where string) string {
	label := where
	if label == "" {
		label = "arguments"
	}
	if expected, ok := schema["type"].(string); ok {
		if check, known := jsonTypes[expected]; known && !check(value) {
			return fmt.Sprintf("%s must be of type %s, got %s", label, expected, pyval.TypeName(value))
		}
	}
	if enum, ok := asList(schema["enum"]); ok {
		found := false
		for _, e := range enum {
			if pyval.Equal(value, e) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("%s must be one of %s", label, pyval.Repr(enum))
		}
	}
	if pyval.IsNumber(value) {
		if minimum, ok := schema["minimum"]; ok && pyval.IsNumber(minimum) && pyval.Compare(value, minimum) < 0 {
			return fmt.Sprintf("%s must be >= %s", label, pyval.Repr(minimum))
		}
		if maximum, ok := schema["maximum"]; ok && pyval.IsNumber(maximum) && pyval.Compare(value, maximum) > 0 {
			return fmt.Sprintf("%s must be <= %s", label, pyval.Repr(maximum))
		}
	}
	if dict, ok := asDict(value); ok {
		props, _ := asDict(schema["properties"])
		if where != "" {
			if required, ok := asList(schema["required"]); ok {
				var missing []any
				for _, k := range required {
					key, isStr := k.(string)
					if _, present := dict[key]; !isStr || !present {
						missing = append(missing, pyval.Str(k))
					}
				}
				if len(missing) > 0 {
					return fmt.Sprintf("%s is missing %s", label, pyval.Repr(missing))
				}
			}
		}
		extra, hasExtra := schema["additionalProperties"]
		for _, key := range pyval.SortedKeys(dict) {
			item := dict[key]
			child := key
			if where != "" {
				child = where + "." + key
			}
			var errText string
			if sub, isSchema := asDict(props[key]); isSchema {
				errText = ValidateArguments(sub, item, child)
			} else if b, isBool := extra.(bool); hasExtra && isBool && !b {
				errText = "unexpected argument " + contracts.PyRepr(child)
			} else if extraSchema, isSchema := asDict(extra); hasExtra && isSchema {
				errText = ValidateArguments(extraSchema, item, child)
			}
			if errText != "" {
				return errText
			}
		}
	}
	if list, ok := asList(value); ok {
		if items, isSchema := asDict(schema["items"]); isSchema {
			for i, item := range list {
				if errText := ValidateArguments(items, item, fmt.Sprintf("%s[%d]", label, i)); errText != "" {
					return errText
				}
			}
		}
	}
	return ""
}
