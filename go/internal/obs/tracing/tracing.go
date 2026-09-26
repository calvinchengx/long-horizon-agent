// Package tracing is OpenTelemetry trace export plus the spans every run path opens
// (python/src/lha/obs/otel.py).
//
// Configure is called once at process start by the CLI (every command; the component is "worker"
// for `lha worker`, else "cli"). It installs a TracerProvider with an OTLP/HTTP exporter for each
// configured backend:
//
//   - an OTLP collector at LHA_OTEL_EXPORTER_OTLP_ENDPOINT (or the standard
//     OTEL_EXPORTER_OTLP_ENDPOINT); OTEL_EXPORTER_OTLP_HEADERS is honoured by the exporter;
//   - Langfuse, when LHA_LANGFUSE_HOST + LHA_LANGFUSE_PUBLIC_KEY + LHA_LANGFUSE_SECRET_KEY are all
//     set, through Langfuse's OTLP endpoint (<host>/api/public/otel, Basic auth).
//
// Nothing is configured when neither is set or when OTEL_SDK_DISABLED=true: then every span is a
// no-op (the global no-op provider).
//
// Spans are opened around a local or orchestrated mission (SpanMission, "lha.mission"), every
// agent cycle (SpanCycle, "lha.cycle"), every durable cycle activity attempt (StartActivityCycle,
// "lha.activity.run_agent_cycle"), every metered model call (StartModelCall, "chat <model>" /
// "invoke_agent <model>") and every dispatched tool call (TracedDispatch, "execute_tool <name>").
// They carry metadata only (ids, model, token counts, verdicts, tool names, ok/error), never
// prompts, tool arguments or outputs, and every attribute goes through obs.RedactMapping first.
//
// Tracing never blocks or fails the agent: spans are exported by a background batch span
// processor (a bounded queue that drops spans when the backend is down), each export gives up
// after LHA_OTEL_EXPORT_TIMEOUT_S, and errors of the tracing machinery itself are logged, never
// returned. Errors of the traced work are recorded on the span (redacted) and returned unchanged.
//
// # API for other packages
//
//	ctx, span := tracing.Start(ctx, name, map[string]any{...}) // attributes: nil values skipped
//	span.Set(map[string]any{...})                             // add attributes after the work
//	span.Error("message")                                     // failed without a Go error
//	span.End(err)                                             // err != nil: recorded + ERROR
//
// The named helpers below (StartActivityCycle, StartModelCall, TracedDispatch, AgentSpan) fix
// the span names and attribute keys shared with Python, so a trace backend sees the same
// schema from either implementation.
package tracing

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// TracerName is the instrumentation scope of every LHA span.
const TracerName = "lha"

// ExportTarget is one OTLP/HTTP trace destination.
type ExportTarget struct {
	Name     string            // "otlp" | "langfuse"
	Endpoint string            // full traces URL (".../v1/traces")
	Headers  map[string]string // nil: the exporter reads OTEL_EXPORTER_OTLP_HEADERS
}

func tracesURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1/traces") {
		return base
	}
	return base + "/v1/traces"
}

func nonEmpty(p *string) bool { return p != nil && *p != "" }

// ExportTargets are the trace destinations settings configure (empty: tracing stays off).
func ExportTargets(s *config.Settings) []ExportTarget {
	var targets []ExportTarget
	if nonEmpty(s.OTelExporterOTLPEndpoint) {
		targets = append(targets, ExportTarget{Name: "otlp", Endpoint: tracesURL(*s.OTelExporterOTLPEndpoint)})
	}
	if nonEmpty(s.LangfuseHost) && nonEmpty(s.LangfusePublicKey) && s.LangfuseSecretKey.Value() != "" {
		token := base64.StdEncoding.EncodeToString([]byte(*s.LangfusePublicKey + ":" + s.LangfuseSecretKey.Value()))
		targets = append(targets, ExportTarget{
			Name:     "langfuse",
			Endpoint: tracesURL(strings.TrimRight(*s.LangfuseHost, "/") + "/api/public/otel"),
			Headers:  map[string]string{"Authorization": "Basic " + token},
		})
	}
	return targets
}

// ExporterFactory builds the exporter for one target (tests replace the OTLP/HTTP exporter).
type ExporterFactory func(target ExportTarget, timeout time.Duration) (sdktrace.SpanExporter, error)

// Options configure Configure.
type Options struct {
	Component       string          // resource attribute lha.component ("cli" | "worker"); "" = "cli"
	ExporterFactory ExporterFactory // nil = OTLP/HTTP
}

var (
	mu       sync.Mutex
	provider *sdktrace.TracerProvider // the provider Configure installed (nil until it exports)
)

func logger() interface {
	Info(string, ...any)
	Warn(string, ...any)
} {
	return obs.Logger("lha.obs")
}

// Configure installs a tracer provider exporting to every configured target and returns their
// names. It is idempotent (a second call keeps the first provider) and returns nil, changing
// nothing, when no target is configured or OTEL_SDK_DISABLED is true. A broken exporter setup is
// logged and leaves tracing off.
func Configure(s *config.Settings, o Options) []string {
	targets := ExportTargets(s)
	if len(targets) == 0 || s.OTelSDKDisabled {
		return nil
	}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	mu.Lock()
	defer mu.Unlock()
	if provider != nil {
		return names
	}
	component := o.Component
	if component == "" {
		component = "cli"
	}
	factory := o.ExporterFactory
	if factory == nil {
		factory = otlpExporter
	}
	timeout := time.Duration(s.OTelExportTimeoutS) * time.Second
	res, err := resource.New(context.Background(),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		resource.WithAttributes(
			attribute.String("service.name", s.OTelServiceName),
			attribute.String("lha.component", component),
		))
	if err != nil && res == nil {
		logger().Warn("tracing_not_configured", "reason", obs.RedactText(err.Error()))
		return nil
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	for _, target := range targets {
		exporter, err := factory(target, timeout)
		if err != nil {
			logger().Warn("tracing_not_configured", "reason", obs.RedactText(err.Error()))
			return nil
		}
		opts = append(opts, sdktrace.WithBatcher(exporter, sdktrace.WithExportTimeout(timeout)))
	}
	p := sdktrace.NewTracerProvider(opts...)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger().Warn("tracing_export_failed", "error", obs.RedactText(err.Error()))
	}))
	otel.SetTracerProvider(p)
	provider = p
	logger().Info("tracing_configured", "targets", names, "component", component)
	return names
}

func otlpExporter(target ExportTarget, timeout time.Duration) (sdktrace.SpanExporter, error) {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(target.Endpoint),
		otlptracehttp.WithTimeout(timeout),
	}
	if target.Headers != nil {
		opts = append(opts, otlptracehttp.WithHeaders(target.Headers))
	}
	// New does not connect: the first export does, in the batch processor's goroutine.
	return otlptracehttp.New(context.Background(), opts...)
}

// Shutdown flushes and stops the provider Configure installed (a no-op otherwise), waiting at
// most until ctx is done.
func Shutdown(ctx context.Context) {
	mu.Lock()
	p := provider
	provider = nil
	mu.Unlock()
	if p == nil {
		return
	}
	if err := p.Shutdown(ctx); err != nil {
		logger().Warn("tracing_shutdown_failed", "error", obs.RedactText(err.Error()))
	}
}

// ForceFlush exports every finished span now (tests and short-lived processes).
func ForceFlush(ctx context.Context) {
	mu.Lock()
	p := provider
	mu.Unlock()
	if p != nil {
		_ = p.ForceFlush(ctx)
	}
}

// Reset forgets the installed provider without shutting it down (tests only).
func Reset() {
	mu.Lock()
	provider = nil
	mu.Unlock()
}

// --- spans -----------------------------------------------------------------------------------

// Span lets traced code add attributes after the work ran (token usage, a verdict). A nil or
// non-recording Span ignores every call.
type Span struct{ span trace.Span }

// Start opens span name (a child of any span in ctx) with the redacted attrs. The returned ctx
// carries the span; always call End.
func Start(ctx context.Context, name string, attrs map[string]any) (context.Context, *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, s := otel.Tracer(TracerName).Start(ctx, name)
	h := &Span{span: s}
	h.Set(attrs)
	return ctx, h
}

func (s *Span) recording() bool { return s != nil && s.span != nil && s.span.IsRecording() }

// Set records attrs (nil values skipped) after redaction.
func (s *Span) Set(attrs map[string]any) {
	if !s.recording() || len(attrs) == 0 {
		return
	}
	defer func() { _ = recover() }() // tracing must never fail the traced work
	kept := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if v = deref(v); v != nil {
			kept[k] = v
		}
	}
	for k, v := range obs.RedactMapping(kept) {
		s.span.SetAttributes(attributeOf(k, v))
	}
}

// Error marks the span failed without an error value (a tool returned ok=false).
func (s *Span) Error(message string) {
	if !s.recording() {
		return
	}
	s.span.SetStatus(codes.Error, head(obs.RedactText(message), 200))
}

// End ends the span; a non-nil err is recorded (redacted) and sets the ERROR status.
func (s *Span) End(err error) {
	if s == nil || s.span == nil {
		return
	}
	if err != nil && s.span.IsRecording() {
		text := obs.RedactText(pyTypeName(err) + ": " + err.Error())
		s.span.AddEvent("exception", trace.WithAttributes(
			attribute.String("exception.type", pyTypeName(err)),
			attribute.String("exception.message", obs.RedactText(err.Error())),
		))
		s.span.SetStatus(codes.Error, text)
	}
	s.span.End()
}

func pyTypeName(err error) string {
	if n, ok := err.(interface{ PyTypeName() string }); ok {
		return n.PyTypeName()
	}
	return fmt.Sprintf("%T", err)
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// deref unwraps pointers (nil pointer = no value), like python's "v is not None" filter.
func deref(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	return rv.Interface()
}

func attributeOf(key string, v any) attribute.KeyValue {
	switch x := v.(type) {
	case string:
		return attribute.String(key, x)
	case bool:
		return attribute.Bool(key, x)
	case int:
		return attribute.Int(key, x)
	case int64:
		return attribute.Int64(key, x)
	case int32:
		return attribute.Int64(key, int64(x))
	case float64:
		return attribute.Float64(key, x)
	case float32:
		return attribute.Float64(key, float64(x))
	}
	return attribute.String(key, pyfmt.PyStr(v)) // python: str(value)
}

// --- named spans (the schema shared with the Python implementation) ------------------------

// SpanMission opens "lha.mission"; attrs are e.g. {"lha.run_path": "local", "lha.title": t}.
func SpanMission(ctx context.Context, attrs map[string]any) (context.Context, *Span) {
	return Start(ctx, "lha.mission", attrs)
}

// MissionResult are the attributes a finished mission's "lha.mission" span records
// (python: mission_span_attributes).
func MissionResult(missionID, stoppedReason string, completed bool, cycles, itemsDone, itemsTotal int, costUSD float64) map[string]any {
	return map[string]any{
		"lha.mission_id":     missionID,
		"lha.stopped_reason": stoppedReason,
		"lha.completed":      completed,
		"lha.cycles":         cycles,
		"lha.items_done":     itemsDone,
		"lha.items_total":    itemsTotal,
		"lha.cost_usd":       costUSD,
	}
}

// SpanCycle opens "lha.cycle" for one agent cycle.
func SpanCycle(ctx context.Context, missionID, cycleID string) (context.Context, *Span) {
	return Start(ctx, "lha.cycle", map[string]any{"lha.mission_id": missionID, "lha.cycle_id": cycleID})
}

// StartActivityCycle opens "lha.activity.run_agent_cycle": one span per durable activity
// ATTEMPT (retries are visible). After the cycle, record the outcome with
// span.Set(tracing.ActivityResult(verdict, advanced)) and end it with span.End(err).
func StartActivityCycle(ctx context.Context, missionID, cycleID string, attempt int) (context.Context, *Span) {
	return Start(ctx, "lha.activity.run_agent_cycle", map[string]any{
		"lha.mission_id": missionID, "lha.cycle_id": cycleID, "lha.attempt": attempt,
	})
}

// ActivityResult are the attributes a finished activity span records.
func ActivityResult(verdict string, advanced bool) map[string]any {
	return map[string]any{"lha.verdict": verdict, "lha.advanced": advanced}
}

// StartModelCall opens "<operation> <model>" for a metered model call; operation is "chat"
// (one completion) or "invoke_agent" (a whole external agent session, e.g. claude -p). After
// the call, record ModelUsage and End the span.
func StartModelCall(ctx context.Context, operation, model, role, cycleID string) (context.Context, *Span) {
	return Start(ctx, operation+" "+model, map[string]any{
		"gen_ai.operation.name": operation,
		"gen_ai.request.model":  model,
		"lha.role":              role,
		"lha.cycle_id":          cycleID,
	})
}

// ModelUsage are the attributes a finished model call records. finishReason and costUSD may be
// nil (unknown), and are then left out.
func ModelUsage(u contracts.Usage, finishReason *string, costUSD *float64) map[string]any {
	return map[string]any{
		"gen_ai.response.model":          u.Model,
		"gen_ai.usage.input_tokens":      u.InputTokens,
		"gen_ai.usage.output_tokens":     u.OutputTokens,
		"gen_ai.response.finish_reasons": finishReason,
		"lha.cost_usd":                   costUSD,
	}
}

// TracedDispatch is d.Dispatch(ctx, call, tctx) inside an "execute_tool <name>" span: tool
// name, call id, mission and ok. Arguments and output are not recorded. Every agent that runs
// tools dispatches through it, so each call, including one the policy refuses, is one span.
func TracedDispatch(ctx context.Context, d contracts.ToolDispatcher, call contracts.ToolCall, tctx contracts.ToolContext) contracts.ToolResult {
	ctx, span := Start(ctx, "execute_tool "+call.Name, map[string]any{
		"gen_ai.operation.name": "execute_tool",
		"gen_ai.tool.name":      call.Name,
		"gen_ai.tool.call.id":   call.ID,
		"lha.mission_id":        tctx.MissionID,
	})
	result := d.Dispatch(ctx, call, tctx)
	span.Set(map[string]any{"lha.tool.ok": result.OK})
	if !result.OK {
		msg := result.ErrorText()
		if msg == "" {
			msg = "tool failed"
		}
		span.Error(msg)
	}
	span.End(nil)
	return result
}

// AgentSpan opens span name with every attribute namespaced "gen_ai.<key>" (python: agent_span,
// used by the orchestrator's "cycle" and "implement" spans).
func AgentSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, *Span) {
	prefixed := make(map[string]any, len(attrs))
	for k, v := range attrs {
		prefixed["gen_ai."+k] = v
	}
	return Start(ctx, name, prefixed)
}
