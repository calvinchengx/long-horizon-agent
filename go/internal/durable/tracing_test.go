package durable

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs/tracing"
)

// The durable "lha.activity.run_agent_cycle" span (python: _traced_cycle): one span per activity
// attempt, wrapping the cycle so the nested "lha.cycle" span is its child.

// traced installs an in-memory tracing provider and returns its exporter (python: the conftest
// exporter; the tracing package's own tests use the same pattern).
func traced(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	s, err := config.LoadFrom([]string{"LHA_OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318"}, "")
	if err != nil {
		t.Fatal(err)
	}
	names := tracing.Configure(s, tracing.Options{
		ExporterFactory: func(tracing.ExportTarget, time.Duration) (sdktrace.SpanExporter, error) {
			return exporter, nil
		},
	})
	if len(names) != 1 || names[0] != "otlp" {
		t.Fatalf("names = %v", names)
	}
	t.Cleanup(func() {
		tracing.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exporter
}

func finishedSpans(t *testing.T, e *tracetest.InMemoryExporter) []tracetest.SpanStub {
	t.Helper()
	tracing.ForceFlush(context.Background())
	return e.GetSpans()
}

func spanAttrs(s tracetest.SpanStub) map[string]any {
	out := map[string]any{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

func spanNamed(spans []tracetest.SpanStub, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

func TestRunAgentCycleOpensTheActivitySpan(t *testing.T) {
	exporter := traced(t)
	inp := initMission(t, 1)
	factory := func(*config.Settings, contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		return model.NewStub([]contracts.TurnResult{writeTurn("work/01.txt"), doneTurn()}), nil
	}
	// A direct call is not in a Temporal activity context, so the attempt is 1 (python:
	// activity.in_activity() is False => attempt 1).
	acts := &Activities{Settings: testSettings(t), ModelFactory: factory, OpenToolbox: testToolbox}
	res, err := acts.RunAgentCycle(context.Background(),
		CycleInput{MissionID: inp.MissionID, Workdir: inp.Workdir, CycleID: "c1", CheckCommands: checkCommands})
	if err != nil || !res.Advanced {
		t.Fatalf("%+v %v", res, err)
	}

	spans := finishedSpans(t, exporter)
	act, ok := spanNamed(spans, "lha.activity.run_agent_cycle")
	if !ok {
		t.Fatal("no lha.activity.run_agent_cycle span")
	}
	got := spanAttrs(act)
	want := map[string]any{
		"lha.mission_id": inp.MissionID, "lha.cycle_id": "c1", "lha.attempt": int64(1),
		"lha.verdict": res.Verdict, "lha.advanced": true,
	}
	if len(got) != len(want) {
		t.Fatalf("activity attrs %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("activity %s = %v, want %v", k, got[k], v)
		}
	}
	// The cycle span is a child of the activity span: ctx was reassigned.
	cycle, ok := spanNamed(spans, "lha.cycle")
	if !ok || cycle.Parent.SpanID() != act.SpanContext.SpanID() {
		t.Fatalf("lha.cycle not nested under the activity span: %+v", cycle.Parent)
	}
}
