package tracing

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Ported from python/tests/unit/test_otel_tracing.py.

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func settings(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	s, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// exported configures tracing with an in-memory exporter and restores the no-op provider after.
func exported(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	names := Configure(settings(t, "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318"), Options{
		ExporterFactory: func(ExportTarget, time.Duration) (sdktrace.SpanExporter, error) { return exporter, nil },
	})
	if len(names) != 1 || names[0] != "otlp" {
		t.Fatalf("names = %v", names)
	}
	t.Cleanup(func() {
		Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exporter
}

func finished(e *tracetest.InMemoryExporter) tracetest.SpanStubs {
	ForceFlush(context.Background())
	return e.GetSpans()
}

func attrs(s tracetest.SpanStub) map[string]any {
	out := map[string]any{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

func TestNoTargetMeansNoProvider(t *testing.T) {
	if got := ExportTargets(settings(t)); len(got) != 0 {
		t.Fatal(got)
	}
	if got := Configure(settings(t), Options{}); got != nil {
		t.Fatal(got)
	}
	if provider != nil {
		t.Fatal("provider installed")
	}
}

func TestOTLPEndpointGetsTheTracesPathOnce(t *testing.T) {
	got := ExportTargets(settings(t, "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://c:4318/"))
	if len(got) != 1 || got[0].Name != "otlp" || got[0].Endpoint != "http://c:4318/v1/traces" || got[0].Headers != nil {
		t.Fatalf("%+v", got)
	}
	same := ExportTargets(settings(t, "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://c/v1/traces"))
	if same[0].Endpoint != "http://c/v1/traces" {
		t.Fatal(same)
	}
	std := ExportTargets(settings(t, "OTEL_EXPORTER_OTLP_ENDPOINT=http://std:4318"))
	if std[0].Endpoint != "http://std:4318/v1/traces" {
		t.Fatal(std)
	}
	both := ExportTargets(settings(t, "OTEL_EXPORTER_OTLP_ENDPOINT=http://std:4318", "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://lha:4318"))
	if both[0].Endpoint != "http://lha:4318/v1/traces" {
		t.Fatal(both)
	}
}

func TestLangfuseGoesThroughItsOTLPEndpointWithBasicAuth(t *testing.T) {
	got := ExportTargets(settings(t, "LHA_LANGFUSE_HOST=https://lf.example/", "LHA_LANGFUSE_PUBLIC_KEY=pk-lf-1",
		"LHA_LANGFUSE_SECRET_KEY=sk-lf-2"))
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf-1:sk-lf-2"))
	if len(got) != 1 || got[0].Name != "langfuse" || got[0].Endpoint != "https://lf.example/api/public/otel/v1/traces" ||
		got[0].Headers["Authorization"] != want {
		t.Fatalf("%+v", got)
	}
	if got := ExportTargets(settings(t, "LHA_LANGFUSE_HOST=https://lf.example")); len(got) != 0 {
		t.Fatal("incomplete Langfuse settings configured a target")
	}
}

func TestSDKDisabledAndBrokenExporterTurnTracingOff(t *testing.T) {
	if got := Configure(settings(t, "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://c", "OTEL_SDK_DISABLED=true"), Options{}); got != nil {
		t.Fatal(got)
	}
	boom := func(ExportTarget, time.Duration) (sdktrace.SpanExporter, error) {
		return nil, errors.New("bad exporter")
	}
	if got := Configure(settings(t, "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://c"), Options{ExporterFactory: boom}); got != nil {
		t.Fatal(got)
	}
	if provider != nil {
		t.Fatal("provider installed")
	}
}

func TestConfigureIsIdempotent(t *testing.T) {
	exported(t)
	first := provider
	if got := Configure(settings(t, "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://c"), Options{}); len(got) != 1 || provider != first {
		t.Fatal("second Configure replaced the provider")
	}
}

func TestSpansAreNoopsWithoutAProvider(t *testing.T) {
	_, s := Start(context.Background(), "x", map[string]any{"a": 1})
	s.Set(map[string]any{"b": 2})
	s.Error("nothing to mark")
	s.End(errors.New("x"))
	var nilSpan *Span
	nilSpan.Set(map[string]any{"a": 1})
	nilSpan.Error("x")
	nilSpan.End(nil)
}

func TestAttributesAreRedactedAndErrorsRecorded(t *testing.T) {
	e := exported(t)
	var missing *float64
	_, s := Start(context.Background(), "work", map[string]any{"api_key": "hunter2"})
	s.Set(map[string]any{"note": "Authorization: Bearer abc.def", "skipped": nil, "nilptr": missing,
		"obj": []any{"x"}, "n": 3, "f": 1.5, "ok": true})
	s.End(errors.New("boom sk-ant-api03-abcdefghijklmnopqrstuvwxyz"))
	spans := finished(e)
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	a := attrs(spans[0])
	if a["api_key"] != "***" || a["obj"] != "['x']" || a["n"] != int64(3) || a["f"] != 1.5 || a["ok"] != true {
		t.Fatalf("%v", a)
	}
	if note, _ := a["note"].(string); note == "" || strings.Contains(note, "abc.def") {
		t.Fatalf("note not redacted: %q", note)
	}
	for _, k := range []string{"skipped", "nilptr"} {
		if _, ok := a[k]; ok {
			t.Fatalf("%s recorded", k)
		}
	}
	if spans[0].Status.Code != codes.Error || strings.Contains(spans[0].Status.Description, "abcdefghijklmnop") {
		t.Fatalf("status %+v", spans[0].Status)
	}
	if len(spans[0].Events) == 0 || spans[0].Events[0].Name != "exception" {
		t.Fatal("no exception event")
	}
}

type fakeDispatcher struct{ ok bool }

func (fakeDispatcher) Specs() []contracts.ToolSpec { return nil }
func (d fakeDispatcher) Dispatch(context.Context, contracts.ToolCall, contracts.ToolContext) contracts.ToolResult {
	if d.ok {
		return contracts.Success("fine")
	}
	return contracts.Failure("refused by policy")
}

func TestTracedDispatchAndNamedSpans(t *testing.T) {
	e := exported(t)
	ctx, parent := StartActivityCycle(context.Background(), "m1", "c3", 1)
	TracedDispatch(ctx, fakeDispatcher{ok: false}, contracts.ToolCall{ID: "t1", Name: "write_file",
		Arguments: map[string]any{"content": "secret body"}}, contracts.ToolContext{MissionID: "m1"})
	TracedDispatch(ctx, fakeDispatcher{ok: true}, contracts.ToolCall{ID: "t2", Name: "read_file"}, contracts.ToolContext{MissionID: "m1"})
	_, chat := StartModelCall(ctx, "chat", "stub:stub-1", "lead", "c3")
	stop := "end_turn"
	chat.Set(ModelUsage(contracts.Usage{Model: "stub-1", InputTokens: 10, OutputTokens: 5}, &stop, nil))
	chat.End(nil)
	_, agentSpan := AgentSpan(ctx, "implement", map[string]any{"mission_id": "m1", "item": "01"})
	agentSpan.End(nil)
	parent.Set(ActivityResult("passed", true))
	parent.End(nil)

	byName := map[string]tracetest.SpanStub{}
	for _, s := range finished(e) {
		byName[s.Name] = s
	}
	act := byName["lha.activity.run_agent_cycle"]
	want := map[string]any{"lha.mission_id": "m1", "lha.cycle_id": "c3", "lha.attempt": int64(1),
		"lha.verdict": "passed", "lha.advanced": true}
	if got := attrs(act); len(got) != len(want) {
		t.Fatalf("activity attrs %v", got)
	} else {
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("activity %s = %v, want %v", k, got[k], v)
			}
		}
	}
	failed := byName["execute_tool write_file"]
	fa := attrs(failed)
	if fa["lha.tool.ok"] != false || fa["gen_ai.tool.call.id"] != "t1" || fa["gen_ai.operation.name"] != "execute_tool" ||
		failed.Status.Code != codes.Error || failed.Status.Description != "refused by policy" ||
		failed.Parent.SpanID() != act.SpanContext.SpanID() {
		t.Fatalf("failed tool span %+v %v", failed.Status, fa)
	}
	for _, kv := range failed.Attributes {
		if kv.Value.Type() == attribute.STRING && strings.Contains(kv.Value.AsString(), "secret body") {
			t.Fatal("tool arguments recorded")
		}
	}
	if okSpan := byName["execute_tool read_file"]; okSpan.Status.Code == codes.Error || attrs(okSpan)["lha.tool.ok"] != true {
		t.Fatal("ok tool span")
	}
	ca := attrs(byName["chat stub:stub-1"])
	if ca["gen_ai.request.model"] != "stub:stub-1" || ca["lha.role"] != "lead" || ca["gen_ai.usage.output_tokens"] != int64(5) ||
		ca["gen_ai.response.finish_reasons"] != "end_turn" {
		t.Fatalf("chat attrs %v", ca)
	}
	if _, ok := ca["lha.cost_usd"]; ok {
		t.Fatal("unknown cost recorded")
	}
	if ia := attrs(byName["implement"]); ia["gen_ai.item"] != "01" || ia["gen_ai.mission_id"] != "m1" {
		t.Fatalf("agent span %v", ia)
	}
}

// A real OTLP/HTTP export reaches the collector with the Langfuse path and Basic auth.
func TestRealExporterPostsToLangfuseWithBasicAuth(t *testing.T) {
	var mu sync.Mutex
	var paths, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	names := Configure(settings(t, "LHA_LANGFUSE_HOST="+srv.URL, "LHA_LANGFUSE_PUBLIC_KEY=pk", "LHA_LANGFUSE_SECRET_KEY=sk"), Options{})
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	if len(names) != 1 || names[0] != "langfuse" {
		t.Fatal(names)
	}
	_, s := Start(context.Background(), "work", nil)
	s.End(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	Shutdown(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || paths[0] != "/api/public/otel/v1/traces" ||
		auths[0] != "Basic "+base64.StdEncoding.EncodeToString([]byte("pk:sk")) {
		t.Fatalf("paths %v auths %v", paths, auths)
	}
}

func TestRealExporterToADeadBackendNeverBlocks(t *testing.T) {
	names := Configure(settings(t, "LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:9", "LHA_OTEL_EXPORT_TIMEOUT_S=1"), Options{})
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	if len(names) != 1 {
		t.Fatal(names)
	}
	started := time.Now()
	for i := 0; i < 20; i++ {
		_, s := Start(context.Background(), "work", map[string]any{"i": i})
		s.End(nil)
	}
	if time.Since(started) > time.Second {
		t.Fatal("span creation blocked on the exporter")
	}
	started = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	Shutdown(ctx)
	if time.Since(started) > 5*time.Second {
		t.Fatal("shutdown not bounded")
	}
}
