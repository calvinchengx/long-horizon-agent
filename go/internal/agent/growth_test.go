package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from python/tests/load/test_memory_growth.py: over a long local mission the trace
// recorder and the cost ledger stay at their caps while their totals keep counting.

// editor inserts a line at the top of one file per cycle (shifting its chunks), then signals done.
type editor struct {
	*model.StubModel
	dir   string
	files map[string][]string
	n     int
}

func (m *editor) Complete(_ context.Context, msgs []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == "tool" {
		return done, nil
	}
	m.n++
	name := fmt.Sprintf("src/mod%03d.py", m.n%len(m.files))
	m.files[name] = append([]string{fmt.Sprintf("# edit %d\n", m.n)}, m.files[name]...)
	return nativeWrite(fmt.Sprintf("w%d", m.n), name, strings.Join(m.files[name], "")), nil
}

func TestALongLocalMissionKeepsItsStructuresAtTheirCaps(t *testing.T) {
	const cycles, files = 30, 10
	defer func(a, b int) { obs.MaxTraceEvents, governor.MaxLedgerEntries = a, b }(obs.MaxTraceEvents, governor.MaxLedgerEntries)
	obs.MaxTraceEvents, governor.MaxLedgerEntries = 40, 30
	dir := t.TempDir()
	initial := map[string]string{}
	edited := map[string][]string{}
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("src/mod%03d.py", i)
		for j := 0; j < 60; j++ {
			edited[name] = append(edited[name], fmt.Sprintf("def f%d_%d(x):\n    return x + %d\n", i, j, j))
		}
		initial[name] = strings.Join(edited[name], "")
	}
	for name, text := range initial {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.InitRepo(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CommitAll(context.Background(), dir, "init"); err != nil {
		t.Fatal(err)
	}
	items := []contracts.ChecklistItem{}
	for i := 0; i < cycles; i++ {
		items = append(items, contracts.NewChecklistItem(fmt.Sprintf("%03d", i), fmt.Sprintf("edit %d", i)))
	}
	settings := runnerSettings(t, fmt.Sprintf("LHA_MAX_CYCLES=%d", cycles+5), "LHA_FLAKY_RETRIES=0", "LHA_BUDGET_USD_CEILING=1000000000")
	recorder := obs.NewTraceRecorder(nil)
	meter := BuildMeter(settings)
	o := runOpts(t, dir, settings, &editor{StubModel: model.NewStub(nil), dir: dir, files: edited}, passCheck)
	o.Checklist = contracts.Checklist{Items: items}
	o.Recorder, o.Meter = recorder, meter
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || !s.Completed || s.Cycles != cycles {
		t.Fatalf("%+v %v", s, err)
	}
	if n := len(recorder.Events()); n > 44 || recorder.Dropped() == 0 || recorder.Dropped()+n < cycles*3 {
		t.Fatal(n, recorder.Dropped())
	}
	if n := len(meter.Ledger.Entries()); n > 33 || meter.Ledger.TotalUSD() != 0 {
		t.Fatal(n, meter.Ledger.TotalUSD())
	}
}
