package org

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
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs/tracing"
)

// The orchestrate run path opens a "lha.mission" span (run_path orchestrate + the result), a
// "cycle" agent span around the Lead's cycle and an "implement" agent span around each parallel
// implementer (python: Orchestrator.run_mission / _serial_round / waves.implement_in_worktree).

// tracingExporter installs an in-memory tracing provider and returns its exporter.
func tracingExporter(t *testing.T) *tracetest.InMemoryExporter {
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

func flushedSpans(t *testing.T, e *tracetest.InMemoryExporter) []tracetest.SpanStub {
	t.Helper()
	tracing.ForceFlush(context.Background())
	return e.GetSpans()
}

func attributesOf(s tracetest.SpanStub) map[string]any {
	out := map[string]any{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

func spansNamed(spans []tracetest.SpanStub, name string) []tracetest.SpanStub {
	out := []tracetest.SpanStub{}
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func TestOrchestrateMissionAndCycleSpans(t *testing.T) {
	exporter := tracingExporter(t)
	dir := t.TempDir()
	summary := run(t, nil, OrchestratorOptions{ResearchPerItem: 0, DoReview: false,
		Models: map[string]contracts.ModelProvider{"lead": stub(doneTurn)}},
		MissionOptions{Workdir: dir, Title: "Traced", Description: "d", Checklist: checklist2(),
			Checks: []contracts.Check{pass}})
	if !summary.Completed || summary.StoppedReason != "complete" || summary.Cycles != 2 || summary.ItemsDone != 2 {
		t.Fatalf("%+v", summary)
	}

	spans := flushedSpans(t, exporter)
	missions := spansNamed(spans, "lha.mission")
	if len(missions) != 1 {
		t.Fatalf("%d lha.mission spans", len(missions))
	}
	got := attributesOf(missions[0])
	want := map[string]any{
		"lha.run_path": "orchestrate", "lha.title": "Traced", "lha.resume": false,
		"lha.mission_id": summary.MissionID, "lha.stopped_reason": "complete", "lha.completed": true,
		"lha.cycles": int64(2), "lha.items_done": int64(2), "lha.items_total": int64(2), "lha.cost_usd": summary.TotalUSD,
	}
	if len(got) != len(want) {
		t.Fatalf("mission attrs %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("mission %s = %v, want %v", k, got[k], v)
		}
	}

	cycles := spansNamed(spans, "cycle")
	if len(cycles) != 2 {
		t.Fatalf("%d cycle spans", len(cycles))
	}
	items := map[string]bool{}
	for _, s := range cycles {
		a := attributesOf(s)
		if a["gen_ai.mission_id"] != summary.MissionID {
			t.Fatalf("cycle attrs %v", a)
		}
		item, _ := a["gen_ai.item"].(string)
		items[item] = true
	}
	if !items["01"] || !items["02"] {
		t.Fatalf("cycle items %v", items)
	}
}

func TestParallelWaveOpensImplementAgentSpans(t *testing.T) {
	exporter := tracingExporter(t)
	dir := t.TempDir()
	checklist, owners := twoItems()
	wroteCode := contracts.Check{Name: "wrote_code", Command: []string{"sh", "-c", "ls *.py"}, Gating: true, Where: "sandbox"}
	summary := run(t, nil, OrchestratorOptions{ResearchPerItem: 0, DoReview: false,
		Models: map[string]contracts.ModelProvider{"implementer": newImplementers(nil), "lead": stub(doneTurn)}},
		MissionOptions{Workdir: dir, Title: "Wave", Description: "two disjoint items", Checklist: &checklist,
			Checks: []contracts.Check{pass, wroteCode}, Ownership: owners})
	if !summary.Completed || summary.Cycles != 2 {
		t.Fatalf("%+v", summary)
	}

	spans := flushedSpans(t, exporter)
	implements := spansNamed(spans, "implement")
	if len(implements) != 2 {
		t.Fatalf("%d implement spans", len(implements))
	}
	items := map[string]bool{}
	for _, s := range implements {
		a := attributesOf(s)
		if a["gen_ai.mission_id"] != summary.MissionID {
			t.Fatalf("implement attrs %v", a)
		}
		item, _ := a["gen_ai.item"].(string)
		items[item] = true
	}
	if !items["01"] || !items["02"] {
		t.Fatalf("implement items %v", items)
	}
}
