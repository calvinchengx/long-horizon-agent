package governor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func fp(f float64) *float64 { return &f }

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostLedgerTotals(t *testing.T) {
	l := NewCostLedger()
	l.Record("c1", contracts.Usage{InputTokens: 100, OutputTokens: 50, Model: "m"}, fp(0.5), "")
	l.Record("c2", contracts.Usage{InputTokens: 200, OutputTokens: 20, Model: "m"}, fp(1.5), "")
	if l.TotalUSD() != 2.0 || l.TotalInputTokens() != 300 || l.TotalOutputTokens() != 70 || l.MeanUSDPerCycle() != 1.0 {
		t.Fatal(l.TotalUSD(), l.TotalInputTokens(), l.TotalOutputTokens(), l.MeanUSDPerCycle())
	}
	if l.MaxUSDPerCycle() != 1.5 {
		t.Fatal(l.MaxUSDPerCycle())
	}
	empty := NewCostLedger()
	if empty.MeanUSDPerCycle() != 0 || empty.MaxUSDPerCycle() != 0 {
		t.Fatal("empty ledger")
	}
}

func TestCostLedgerJSONRoundTrip(t *testing.T) {
	l := NewCostLedger()
	l.Record("c1", contracts.Usage{InputTokens: 7, Model: "m", CacheReadInputTokens: 2, CacheCreationInputTokens: 3}, nil, "lead")
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"entries":[{"cycle_id":"c1","model":"m","input_tokens":7,"output_tokens":0,"usd":0,"cache_read_input_tokens":2,"cache_creation_input_tokens":3,"cost_known":false,"role":"lead"}]}`
	if string(raw) != want {
		t.Fatalf("got %s", raw)
	}
	var back CostLedger
	if err := json.Unmarshal([]byte(`{"entries":[{"cycle_id":"c","model":"m","input_tokens":1,"output_tokens":2,"usd":0.5}]}`), &back); err != nil {
		t.Fatal(err)
	}
	e := back.Entries()
	if len(e) != 1 || !e[0].CostKnown || e[0].USD != 0.5 { // pydantic default cost_known=True
		t.Fatalf("%+v", e)
	}
	if err := back.UnmarshalJSON([]byte("nope")); err == nil {
		t.Fatal("bad JSON accepted")
	}
	var ce CostEntry
	if err := ce.UnmarshalJSON([]byte("[")); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

func TestGovernorAllowsWithinBudget(t *testing.T) {
	l := NewCostLedger()
	l.Record("c1", contracts.Usage{Model: "m"}, fp(1.0), "")
	g := NewBudgetGovernor(10, 100, false)
	d := g.AuthorizeNext(l, 1, fp(2.0))
	if !d.Allow || d.ProjectedUSD != 3.0 || d.Reason != "within budget" || d.CeilingUSD != 10 || g.CeilingUSD() != 10 {
		t.Fatalf("%+v", d)
	}
}

func TestGovernorDeniesWhenProjectedExceedsCeiling(t *testing.T) {
	l := NewCostLedger()
	l.Record("c1", contracts.Usage{Model: "m"}, fp(9.5), "")
	d := NewBudgetGovernor(10, 100, false).AuthorizeNext(l, 1, fp(1.0))
	if d.Allow || d.Reason != "projected spend exceeds ceiling" {
		t.Fatalf("%+v", d)
	}
}

func TestGovernorDeniesAtMaxCycles(t *testing.T) {
	d := NewBudgetGovernor(1_000_000, 3, false).AuthorizeNext(NewCostLedger(), 3, fp(0))
	if d.Allow || d.Reason != "max cycles reached" {
		t.Fatalf("%+v", d)
	}
}

func TestGovernorDeniesNextCycleWhenSpendIsUnverifiable(t *testing.T) {
	l := NewCostLedger()
	l.Record("c1", contracts.Usage{InputTokens: 5, Model: "m"}, nil, "")
	d := NewBudgetGovernor(100, 10, false).AuthorizeNext(l, 1, nil)
	want := "spend is unverifiable: 1 call(s) with unknown price (configure prices or explicitly allow unknown cost)"
	if d.Allow || d.Reason != want {
		t.Fatalf("%+v", d)
	}
	if !NewBudgetGovernor(100, 10, true).AuthorizeNext(l, 1, nil).Allow {
		t.Fatal("allow_unknown_cost ignored")
	}
}

func TestGovernorProjectsFromMostExpensiveCycle(t *testing.T) {
	l := NewCostLedger()
	l.Record("c1", contracts.Usage{Model: "m"}, fp(0.1), "")
	l.Record("c2", contracts.Usage{Model: "m"}, fp(5.0), "")
	// spent 5.1; mean 2.55 would allow, the max cycle (5.0) projects 10.1 > 10.
	d := NewBudgetGovernor(10, 10, false).AuthorizeNext(l, 2, nil)
	if d.Allow || !approx(d.ProjectedUSD, 10.1) {
		t.Fatalf("%+v", d)
	}
}

func TestAuthorizeCallBranches(t *testing.T) {
	l := NewCostLedger()
	strict := NewBudgetGovernor(1, 10, false)
	d := strict.AuthorizeCall(l, nil, 0.25)
	if d.Allow || !strings.HasPrefix(d.Reason, "cannot price this model call") || d.ProjectedUSD != 0.25 {
		t.Fatalf("%+v", d)
	}
	if d := NewBudgetGovernor(1, 10, true).AuthorizeCall(l, nil, 0.25); !d.Allow || d.ProjectedUSD != 0.25 {
		t.Fatalf("%+v", d)
	}
	if d := strict.AuthorizeCall(l, fp(0.5), 0.25); !d.Allow || d.ProjectedUSD != 0.75 {
		t.Fatalf("%+v", d)
	}
	if d := strict.AuthorizeCall(l, fp(0.8), 0.25); d.Allow || d.Reason != "worst-case cost of this call exceeds the budget" {
		t.Fatalf("%+v", d)
	}
	l.Record("c", contracts.Usage{}, nil, "")
	if d := strict.AuthorizeCall(l, fp(0), 0); d.Allow || !strings.HasPrefix(d.Reason, "spend is unverifiable: 1 call(s)") {
		t.Fatalf("%+v", d)
	}
}

func TestLoopDetectorTripsAfterThreshold(t *testing.T) {
	d := NewLoopDetector(3)
	if d.Observe("01:edit", true) || d.Observe("01:edit", true) || !d.Observe("01:edit", true) {
		t.Fatal("third consecutive failure must trip")
	}
	d.Reset("01:edit")
	if d.Observe("01:edit", true) {
		t.Fatal("reset ignored")
	}
}

func TestLoopDetectorCountsConsecutiveFailuresOnly(t *testing.T) {
	d := NewLoopDetector(2)
	// fail, pass, fail, pass: two failures but never two IN A ROW.
	for i, failed := range []bool{true, false, true, false} {
		if d.Observe("item", failed) {
			t.Fatalf("tripped at step %d", i)
		}
	}
	d.Observe("a", true)
	d.Observe("b", true)
	d.ResetAll()
	if d.Observe("a", true) || d.Observe("b", true) {
		t.Fatal("ResetAll ignored")
	}
	if !d.Observe("a", true) {
		t.Fatal("second consecutive failure must trip")
	}
}

// --- metering --------------------------------------------------------------------------------

// priced is a fake provider at $1 per 1K tokens (input or output) with a fixed usage per call.
type priced struct {
	usage            contracts.Usage
	defaultMaxTokens int
	unpriced         bool
	priceErr         error
	completeErr      error

	mu      sync.Mutex
	calls   int
	gate    chan struct{}
	started chan struct{}
}

func (p *priced) Name() string          { return "fake:priced" }
func (p *priced) DefaultMaxTokens() int { return p.defaultMaxTokens }
func (p *priced) Complete(ctx context.Context, _ []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.started != nil {
		p.started <- struct{}{}
	}
	if p.gate != nil {
		<-p.gate
	}
	if p.completeErr != nil {
		return contracts.TurnResult{}, p.completeErr
	}
	return contracts.TurnResult{Text: "ok", Usage: p.usage}, nil
}
func (p *priced) EstimateCostUSD(u contracts.Usage) (float64, error) {
	if p.priceErr != nil {
		return 0, p.priceErr
	}
	if p.unpriced {
		return 0, contracts.NewUnknownPriceError("m", "p")
	}
	return float64(u.InputTokens+u.OutputTokens) / 1000.0, nil
}
func (p *priced) nCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// free is a $0 provider without a default max_tokens (like the Python StubModel).
type free struct{}

func (free) Name() string { return "stub" }
func (free) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{Text: "{}", Usage: contracts.Usage{InputTokens: 3, OutputTokens: 4, Model: "stub"}}, nil
}
func (free) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

func meter(ceiling float64, allowUnknown bool) *CostMeter {
	return NewCostMeter(NewCostLedger(), NewBudgetGovernor(ceiling, 100, allowUnknown))
}

var hi = []contracts.ModelMessage{{Role: "user", Content: "hi"}}

func TestEveryCallIsRecordedWithRoleAndCycle(t *testing.T) {
	ctx := context.Background()
	m := meter(100, false)
	reviewer := m.Wrap(&priced{usage: contracts.Usage{InputTokens: 500, OutputTokens: 500}, defaultMaxTokens: 100}, "reviewer")
	planner := m.Wrap(free{}, "planner")
	if m.CycleID() != "c0" {
		t.Fatal(m.CycleID())
	}
	m.SetCycleID("c3")
	for _, mm := range []*MeteredModel{reviewer, planner} {
		if _, err := mm.Complete(ctx, hi, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	entries := m.Ledger.Entries()
	if len(entries) != 2 || entries[0].Role != "reviewer" || entries[1].Role != "planner" ||
		entries[0].CycleID != "c3" || entries[1].CycleID != "c3" {
		t.Fatalf("%+v", entries)
	}
	if !approx(m.Ledger.TotalUSD(), 1.0) {
		t.Fatal(m.Ledger.TotalUSD())
	}
	if reviewer.Name() != "fake:priced" || planner.DefaultMaxTokens() != 0 || reviewer.DefaultMaxTokens() != 100 {
		t.Fatal("forwarding")
	}
	if _, ok := reviewer.Inner().(*priced); !ok {
		t.Fatal("Inner")
	}
	if usd, err := reviewer.EstimateCostUSD(contracts.Usage{InputTokens: 1000}); err != nil || usd != 1 {
		t.Fatal(usd, err)
	}
	if err := reviewer.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHardStopRefusesCallWhoseWorstCaseBreachesBudget(t *testing.T) {
	ctx := context.Background()
	p := &priced{usage: contracts.Usage{InputTokens: 10, OutputTokens: 10}, defaultMaxTokens: 5000}
	model := meter(1, false).Wrap(p, "")
	_, err := model.Complete(ctx, hi, nil, 0)
	var be *BudgetExceeded
	if !errors.As(err, &be) || p.nCalls() != 0 || !strings.Contains(be.Decision.Reason, "worst-case") {
		t.Fatal(err)
	}
	// Worst case: ceil(2/2)+8 = 9 input tokens + 5000 output = $5.009.
	want := "budget governor refused model call: worst-case cost of this call exceeds the budget " +
		"(spent $0.0000, projected $5.0090, ceiling $1.0000)"
	if err.Error() != want {
		t.Fatalf("%q", err.Error())
	}
	if _, err := model.Complete(ctx, hi, nil, 50); err != nil || p.nCalls() != 1 {
		t.Fatal(err, p.nCalls())
	}
}

func TestHardStopAccountsForSpendSoFar(t *testing.T) {
	ctx := context.Background()
	p := &priced{usage: contracts.Usage{InputTokens: 450, OutputTokens: 450}, defaultMaxTokens: 100}
	model := meter(1, false).Wrap(p, "")
	if _, err := model.Complete(ctx, hi, nil, 0); err != nil { // $0.90 spent
		t.Fatal(err)
	}
	var be *BudgetExceeded
	if _, err := model.Complete(ctx, hi, nil, 0); !errors.As(err, &be) || p.nCalls() != 1 {
		t.Fatal(err)
	}
}

func TestParallelCallsReserveTheirWorstCase(t *testing.T) {
	ctx := context.Background()
	p := &priced{usage: contracts.Usage{InputTokens: 1, OutputTokens: 1}, defaultMaxTokens: 600,
		gate: make(chan struct{}), started: make(chan struct{}, 1)}
	m := meter(1, false)
	model := m.Wrap(p, "")
	done := make(chan error, 1)
	go func() {
		_, err := model.Complete(ctx, hi, nil, 0)
		done <- err
	}()
	select {
	case <-p.started: // first call is now in flight with ~$0.6 reserved
	case <-time.After(5 * time.Second):
		t.Fatal("first call never started")
	}
	if r := m.ReservedUSD(); !approx(r, 0.609) {
		t.Fatal(r)
	}
	var be *BudgetExceeded
	if _, err := model.Complete(ctx, hi, nil, 0); !errors.As(err, &be) {
		t.Fatal(err)
	}
	close(p.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.ReservedUSD() != 0 {
		t.Fatal(m.ReservedUSD())
	}
}

func TestUnknownCostIsRefusedByDefault(t *testing.T) {
	p := &priced{usage: contracts.Usage{InputTokens: 1, OutputTokens: 1}, unpriced: true}
	_, err := meter(100, false).Wrap(p, "").Complete(context.Background(), hi, nil, 0)
	var be *BudgetExceeded
	if !errors.As(err, &be) || !strings.Contains(be.Decision.Reason, "price") || p.nCalls() != 0 {
		t.Fatal(err)
	}
}

func TestUnknownCostRecordedWhenExplicitlyAllowed(t *testing.T) {
	p := &priced{usage: contracts.Usage{InputTokens: 7, OutputTokens: 3, Model: "m"}, unpriced: true}
	m := meter(100, true)
	if _, err := m.Wrap(p, "").Complete(context.Background(), hi, nil, 0); err != nil {
		t.Fatal(err)
	}
	e := m.Ledger.Entries()[0]
	if e.CostKnown || e.InputTokens != 7 || m.Ledger.UnknownCostEntries() != 1 {
		t.Fatalf("%+v", e)
	}
}

func TestProviderErrorsReleaseReservationAndPropagate(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	m := meter(100, false)
	p := &priced{defaultMaxTokens: 10, completeErr: boom}
	if _, err := m.Wrap(p, "").Complete(ctx, hi, nil, 0); !errors.Is(err, boom) || m.ReservedUSD() != 0 {
		t.Fatal(err, m.ReservedUSD())
	}
	if len(m.Ledger.Entries()) != 0 {
		t.Fatal("failed call recorded")
	}
	// A pricing failure other than an unknown price is an error, not "unpriced".
	bad := &priced{defaultMaxTokens: 10, priceErr: boom}
	if _, err := m.Wrap(bad, "").Complete(ctx, hi, nil, 0); !errors.Is(err, boom) || bad.nCalls() != 0 {
		t.Fatal(err)
	}
}

func TestWorstCaseUsesAssumedMaxOutput(t *testing.T) {
	m := meter(100, false)
	m.AssumedMaxOutputTokens = 1000
	p := &priced{} // no default max tokens
	worst, err := m.Wrap(p, "").WorstCaseUSD(hi, nil, 0)
	if err != nil || worst == nil || !approx(*worst, 1.009) {
		t.Fatal(worst, err)
	}
	unpriced, err := m.Wrap(&priced{unpriced: true}, "").WorstCaseUSD(hi, nil, 0)
	if err != nil || unpriced != nil {
		t.Fatal(unpriced, err)
	}
}

func TestEstimateInputTokens(t *testing.T) {
	msgs := []contracts.ModelMessage{
		{Role: "user", Content: "héllo"}, // 5 characters
		{Role: "assistant", ToolCalls: []contracts.ToolCall{
			{ID: "1", Name: "read_file", Arguments: map[string]any{"path": "a.py"}}, // {"path": "a.py"} = 16
			{ID: "2", Name: "x"}, // {} = 2
		}},
	}
	tools := []map[string]any{{"name": "t"}} // [{"name": "t"}] = 15
	// chars = 5 + (16 + 9) + (2 + 1) + 15 = 48 -> ceil(48/2) = 24, + 8*2 = 40.
	if got := EstimateInputTokens(msgs, tools); got != 40 {
		t.Fatal(got)
	}
	if got := EstimateInputTokens(nil, nil); got != 0 {
		t.Fatal(got)
	}
}

func TestPyJSONDumpsMatchesPython(t *testing.T) {
	v := map[string]any{
		"a": []any{json.Number("1"), json.Number("2.5"), nil, true, "x\"\\\n\u00e9\U0001F600\x7f\x01"},
		"b": map[string]any{"c": json.Number("1e16"), "d": json.Number("1e-5"), "e": math.Inf(1), "f": json.Number("1e400")},
		"g": json.Number("-0.0"),
		"h": json.Number("1e-400"),
	}
	// Output of Python's json.dumps for the same value.
	want := `{"a": [1, 2.5, null, true, "x\"\\\n\u00e9\ud83d\ude00\u007f\u0001"], "b": {"c": 1e+16, "d": 1e-05, "e": Infinity, "f": Infinity}, "g": -0.0, "h": 0.0}`
	if got := pyJSONDumps(v); got != want {
		t.Fatalf("\n got  %s\n want %s", got, want)
	}
	cases := map[string]any{
		"1":                     float64(1),
		"0":                     math.Copysign(0, -1),
		"1.5":                   1.5,
		"1234567.0":             json.Number("1234567.0"),
		"1e+22":                 1e22,
		"NaN":                   math.NaN(),
		"-Infinity":             math.Inf(-1),
		"false":                 false,
		"3":                     3,
		"4":                     int64(4),
		"5":                     int32(5),
		"6":                     uint64(6),
		"2.5":                   float32(2.5),
		`["a", "b"]`:            []string{"a", "b"},
		"null":                  []string(nil),
		`{"k": 1}`:              map[string]int{"k": 1},
		"123456789012345678901": json.Number("123456789012345678901"),
		"bad":                   json.Number("bad"),
	}
	for want, in := range cases {
		if got := pyJSONDumps(in); got != want {
			t.Errorf("pyJSONDumps(%#v) = %s, want %s", in, got, want)
		}
	}
	if got := pyJSONDumps(func() {}); got != "null" {
		t.Error(got)
	}
}

func TestWaveShare(t *testing.T) {
	for _, c := range []struct {
		ceiling, prior float64
		n              int
		want           float64
	}{{10, 4, 1, 10}, {10, 4, 3, 6}, {10, 12, 2, 12}, {10, 0, 4, 2.5}} {
		if got := WaveShare(c.ceiling, c.prior, c.n); got != c.want {
			t.Errorf("WaveShare(%v, %v, %d) = %v, want %v", c.ceiling, c.prior, c.n, got, c.want)
		}
	}
}

// cappedProvider is a provider whose calls carry their own spend cap, like claude -p.
type cappedProvider struct {
	cap  float64
	seen *[]float64 // the cap each call ran with
}

func (p cappedProvider) Name() string { return "capped" }

func (p cappedProvider) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	*p.seen = append(*p.seen, p.cap)
	cost := 0.25
	return contracts.TurnResult{Text: "ok", Usage: contracts.Usage{ReportedCostUSD: &cost}}, nil
}

func (p cappedProvider) EstimateCostUSD(usage contracts.Usage) (float64, error) {
	if usage.ReportedCostUSD != nil {
		return *usage.ReportedCostUSD, nil
	}
	return p.cap, nil
}

func (p cappedProvider) BudgetCapped(remaining float64) contracts.ModelProvider {
	if remaining >= 0.01 && remaining < p.cap {
		p.cap = remaining
	}
	return p
}

func TestAMeteredCallReservesAndRunsWithTheSameLoweredCap(t *testing.T) {
	m := NewCostMeter(NewCostLedger(), NewBudgetGovernor(3.0, 9, false))
	seen := []float64{}
	model := m.Wrap(cappedProvider{cap: 5, seen: &seen}, "lead")
	if m.RemainingUSD() != 3 || model.RemainingUSD() != 3 {
		t.Fatal(m.RemainingUSD())
	}
	msgs := []contracts.ModelMessage{{Role: "user", Content: "x"}}
	if _, err := model.Complete(context.Background(), msgs, nil, 0); err != nil { // allowed: capped at $3
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != 3 {
		t.Fatalf("caps %v", seen)
	}
	if !approx(m.RemainingUSD(), 2.75) { // the reported cost was charged
		t.Fatal(m.RemainingUSD())
	}
}
