package agent

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// Ported from test_local_mission_traces_mission_cycle_model_and_tool
// (python/tests/unit/test_otel_tracing.py).
func TestLocalMissionTracesMissionCycleModelAndTool(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	stub := model.NewStub([]contracts.TurnResult{
		{ToolCalls: []contracts.ToolCall{{ID: "t1", Name: "no_such_tool", Arguments: map[string]any{"path": "a.txt"}}}},
		done,
	})
	summary, err := RunMissionLocal(context.Background(), runOpts(t, t.TempDir(), runnerSettings(t), stub, passCheck))
	if err != nil || !summary.Completed {
		t.Fatalf("%+v %v", summary, err)
	}
	spans := exporter.GetSpans()
	byPrefix := func(prefix string) []tracetest.SpanStub {
		var out []tracetest.SpanStub
		for _, s := range spans {
			if strings.HasPrefix(s.Name, prefix) {
				out = append(out, s)
			}
		}
		return out
	}
	attr := func(s tracetest.SpanStub, key string) any {
		for _, kv := range s.Attributes {
			if string(kv.Key) == key {
				return kv.Value.AsInterface()
			}
		}
		return nil
	}
	missions, cycles, chats, tools := byPrefix("lha.mission"), byPrefix("lha.cycle"), byPrefix("chat "), byPrefix("execute_tool no_such_tool")
	if len(missions) != 1 || len(cycles) != 1 || len(chats) != 2 || len(tools) != 1 {
		t.Fatalf("spans: %d missions, %d cycles, %d chats, %d tools", len(missions), len(cycles), len(chats), len(tools))
	}
	mission, cycle, tool := missions[0], cycles[0], tools[0]
	if attr(mission, "lha.mission_id") != summary.MissionID || attr(mission, "lha.completed") != true ||
		attr(mission, "lha.run_path") != "local" || attr(mission, "lha.title") != "t" {
		t.Fatalf("mission attrs %v", mission.Attributes)
	}
	if cycle.Parent.SpanID() != mission.SpanContext.SpanID() || attr(cycle, "lha.verdict") != "passed" ||
		attr(cycle, "lha.item_id") != "01" {
		t.Fatalf("cycle %v", cycle.Attributes)
	}
	for _, c := range chats {
		if c.Parent.SpanID() != cycle.SpanContext.SpanID() || attr(c, "gen_ai.operation.name") != "chat" ||
			attr(c, "lha.role") != "lead" || attr(c, "gen_ai.usage.output_tokens") == nil {
			t.Fatalf("chat %v", c.Attributes)
		}
	}
	if attr(tool, "lha.tool.ok") != false || tool.Status.Code != codes.Error || tool.Parent.SpanID() != cycle.SpanContext.SpanID() {
		t.Fatalf("tool %+v %v", tool.Status, tool.Attributes)
	}
}
