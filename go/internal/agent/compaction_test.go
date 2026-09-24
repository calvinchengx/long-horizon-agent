package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// Ported from python/tests/unit/test_more_modules.py (compaction).

func TestCompactionSummarizesOldTurns(t *testing.T) {
	msgs := []contracts.ModelMessage{{Role: "system", Content: "sys"}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, contracts.ModelMessage{Role: "user", Content: fmt.Sprintf("turn %d", i)})
	}
	out, err := CompactMessages(context.Background(), model.NewStub([]contracts.TurnResult{{Text: "SUMMARY"}}), msgs, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Role != "system" || out[1].Content != "turn 0" || !strings.Contains(out[2].Content, "compacted summary") ||
		!strings.Contains(out[2].Content, "SUMMARY") || out[len(out)-1].Content != "turn 9" || len(out) != 5 {
		t.Fatalf("%+v", out)
	}
}

func TestCompactionNoopWhenShort(t *testing.T) {
	msgs := []contracts.ModelMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "a"}}
	out, err := CompactMessages(context.Background(), model.NewStub(nil), msgs, 4, true)
	if err != nil || !reflect.DeepEqual(out, msgs) {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestCompactionSummarizesNewestOlderTurnsAndKeepsPairs(t *testing.T) {
	body := []contracts.ModelMessage{{Role: "user", Content: "TASK"}}
	for i := 0; i < 40; i++ {
		body = append(body,
			contracts.ModelMessage{Role: "assistant", Content: fmt.Sprintf("action %d ", i) + strings.Repeat("x", 600)},
			contracts.ModelMessage{Role: "user", Content: fmt.Sprintf("OBSERVATION (t): result %d", i)})
	}
	rec := newRecording(contracts.TurnResult{Text: "S"})
	out, err := CompactMessages(context.Background(), rec, body, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	seen := rec.calls[0][len(rec.calls[0])-1].Content
	if !strings.Contains(seen, "result 36") || !strings.HasPrefix(seen, "...[older turns trimmed]...\n") {
		t.Fatalf("summarizer saw: %.200q", seen)
	}
	if out[0].Content != "TASK" || out[2].Role != "assistant" {
		t.Fatalf("%+v", out[:3])
	}
	if rec.calls[0][0].Content != SummaryInstructions {
		t.Fatal("summary instructions")
	}
}
