package serve

// Paths no black-box case can reach (python/tests/serve plays spec/serve/ against the binary):
// a failing store, a value JSON cannot encode, a stream whose client is gone or falls behind, and
// Temporal answers the fake workflow never gives (python: tests/unit/test_serve_internals.py).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

var errBoom = errors.New("database is locked")

// fakeStore answers what the server reads; a method named in fail returns errBoom.
type fakeStore struct {
	persistence.Store
	fail    map[string]bool
	mission persistence.MissionRow
	events  []persistence.EventRow
	gates   []persistence.GateRow
}

func (f *fakeStore) err(name string) error {
	if f.fail[name] {
		return errBoom
	}
	return nil
}

func (f *fakeStore) GetMission(_ context.Context, id string) (*persistence.MissionRow, error) {
	if err := f.err("GetMission"); err != nil {
		return nil, err
	}
	if id != f.mission.MissionID {
		return nil, nil
	}
	row := f.mission
	return &row, nil
}

func (f *fakeStore) ListMissions(context.Context, int) ([]persistence.MissionRow, error) {
	return []persistence.MissionRow{f.mission}, f.err("ListMissions")
}

func (f *fakeStore) CostSummary(context.Context, string) (persistence.CostSummary, error) {
	return persistence.CostSummary{}, f.err("CostSummary")
}

func (f *fakeStore) LastMissionEvent(context.Context, string) (*persistence.EventRow, error) {
	return nil, f.err("LastMissionEvent")
}

func (f *fakeStore) ReadMissionEvents(_ context.Context, _ string, after int64, _ int) ([]persistence.EventRow, error) {
	var out []persistence.EventRow
	for _, e := range f.events {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out, f.err("ReadMissionEvents")
}

func (f *fakeStore) ListCosts(context.Context, string, int) ([]persistence.CostRow, error) {
	return nil, f.err("ListCosts")
}

func (f *fakeStore) ListGates(context.Context, string, int) ([]persistence.GateRow, error) {
	return f.gates, f.err("ListGates")
}

func newServer(t *testing.T, store *fakeStore) *Server {
	t.Helper()
	if store.mission.MissionID == "" {
		dir := t.TempDir()
		items := contracts.Checklist{Items: []contracts.ChecklistItem{{ID: "01", Description: "x", Status: "todo"}}}
		if _, err := state.NewGitMissionAnchor(dir).Initialize(context.Background(), "t", "d", items); err != nil {
			t.Fatal(err)
		}
		store.mission = persistence.MissionRow{
			MissionID: "m", Title: "t", Status: "RUNNING", WorkflowID: "mission:m", Workdir: dir,
		}
	}
	s := &Server{Store: store, Token: "tok", Port: 1, keepalive: time.Hour}
	s.hub = newHub(s)
	return s
}

func call(s *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:1"
	req.Header.Set("X-LHA-Token", "tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

// A store error is a 500 internal error on every read that meets it.
func TestStoreErrorsAre500(t *testing.T) {
	for _, tc := range []struct{ fail, target string }{
		{"GetMission", "/api/v1/missions/m"},
		{"CostSummary", "/api/v1/missions/m"},
		{"LastMissionEvent", "/api/v1/missions/m"},
		{"ListMissions", "/api/v1/missions"},
		{"CostSummary", "/api/v1/missions"},
		{"ReadMissionEvents", "/api/v1/missions/m/events"},
		{"ReadMissionEvents", "/api/v1/missions/m/items/01"},
		{"ListCosts", "/api/v1/missions/m/items/01"},
		{"CostSummary", "/api/v1/missions/m/costs"},
		{"ListCosts", "/api/v1/missions/m/costs"},
		{"ListGates", "/api/v1/missions/m/gates"},
	} {
		s := newServer(t, &fakeStore{fail: map[string]bool{tc.fail: true}})
		rec := call(s, "GET", tc.target, nil)
		if rec.Code != 500 || code(t, rec) != "internal" {
			t.Errorf("%s failing, GET %s: %d %s", tc.fail, tc.target, rec.Code, rec.Body)
		}
	}
	if (&apiError{message: "m"}).Error() != "m" {
		t.Fatal("apiError.Error")
	}
}

// A value JSON cannot encode is a plain 500, and an MCP call meeting it is a tool error.
func TestAnUnencodableValueIs500(t *testing.T) {
	store := &fakeStore{events: []persistence.EventRow{{ID: 1, Kind: "x", Payload: map[string]any{"v": math.NaN()}}}}
	s := newServer(t, store)
	if rec := call(s, "GET", "/api/v1/missions/m/events", nil); rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_events","arguments":{"mission_id":"m"}}}`
	result := s.handleMCP(context.Background(), []byte(raw))["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("%v", result)
	}
}

// Rows without a payload or options are sent as {} and [].
func TestMissingPayloadAndOptionsAreEmpty(t *testing.T) {
	store := &fakeStore{
		events: []persistence.EventRow{{ID: 1, Kind: "x"}},
		gates:  []persistence.GateRow{{GateID: "g"}},
	}
	s := newServer(t, store)
	if body := call(s, "GET", "/api/v1/missions/m/events", nil).Body.String(); !strings.Contains(body, `"payload":{}`) {
		t.Fatal(body)
	}
	if body := call(s, "GET", "/api/v1/missions/m/gates", nil).Body.String(); !strings.Contains(body, `"options":[]`) {
		t.Fatal(body)
	}
}

func TestStartNeedsAKeepaliveAndAReadableStore(t *testing.T) {
	t.Setenv("LHA_SERVE_KEEPALIVE_S", "soon")
	if err := newServer(t, &fakeStore{}).Start(context.Background()); err == nil {
		t.Fatal("an invalid LHA_SERVE_KEEPALIVE_S was accepted")
	}
	t.Setenv("LHA_SERVE_KEEPALIVE_S", "1")
	for _, failing := range []string{"ReadMissionEvents", "ListMissions"} {
		if err := newServer(t, &fakeStore{fail: map[string]bool{failing: true}}).Start(context.Background()); !errors.Is(err, errBoom) {
			t.Errorf("%s failing: %v", failing, err)
		}
	}
	h := newServer(t, &fakeStore{fail: map[string]bool{"ListMissions": true}}).hub
	h.subscribe("")
	h.missions(context.Background()) // a store hiccup: nothing is published, nothing breaks
}

// streamWriter is a stream's client: it can refuse writes (gone) and not flush (no streaming).
type streamWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    strings.Builder
	refuse  bool
	flushes int
}

func (w *streamWriter) Header() http.Header { return w.header }
func (w *streamWriter) WriteHeader(int)     {}
func (w *streamWriter) Flush()              { w.mu.Lock(); w.flushes++; w.mu.Unlock() }
func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.refuse {
		return 0, errors.New("broken pipe")
	}
	return w.body.Write(p)
}
func (w *streamWriter) text() string { w.mu.Lock(); defer w.mu.Unlock(); return w.body.String() }

type noFlush struct{ http.ResponseWriter }

func streamRequest(target string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Host = "127.0.0.1:1"
	return req
}

func TestAStreamNeedsAFlushingWriter(t *testing.T) {
	s := newServer(t, &fakeStore{})
	if err := s.stream(noFlush{httptest.NewRecorder()}, streamRequest("/api/v1/stream")); err == nil {
		t.Fatal("streamed without a flusher")
	}
}

// A replay stops when the store fails or the client is gone.
func TestAReplayStopsOnAStoreErrorOrAGoneClient(t *testing.T) {
	failing := newServer(t, &fakeStore{fail: map[string]bool{"ReadMissionEvents": true}})
	if err := failing.stream(&streamWriter{header: http.Header{}}, streamRequest("/api/v1/stream?after=0")); err != nil {
		t.Fatal(err)
	}
	gone := newServer(t, &fakeStore{events: []persistence.EventRow{{ID: 1, Kind: "x", Payload: map[string]any{}}}})
	if err := gone.stream(&streamWriter{header: http.Header{}, refuse: true}, streamRequest("/api/v1/stream?after=0")); err != nil {
		t.Fatal(err)
	}
	nan := newServer(t, &fakeStore{events: []persistence.EventRow{{ID: 1, Kind: "x", Payload: map[string]any{"v": math.NaN()}}}})
	if err := nan.stream(&streamWriter{header: http.Header{}}, streamRequest("/api/v1/stream?after=0")); err != nil {
		t.Fatal(err)
	}
}

// following runs s.stream until it returns, handing its subscriber to drive once registered.
func following(t *testing.T, s *Server, w *streamWriter, target string, drive func(*subscriber)) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.stream(w, streamRequest(target)) }()
	var sub *subscriber
	for deadline := time.Now().Add(5 * time.Second); sub == nil; {
		s.hub.mu.Lock()
		for candidate := range s.hub.subs {
			sub = candidate
		}
		s.hub.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("the stream never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
	drive(sub)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
}

func drop(h *hub, sub *subscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	close(sub.queue)
	h.mu.Unlock()
}

// The live part skips events a replay already sent, sends the rest and mission rows, and ends
// when the hub drops it.
func TestAStreamsLivePart(t *testing.T) {
	s := newServer(t, &fakeStore{})
	w := &streamWriter{header: http.Header{}}
	following(t, s, w, "/api/v1/stream?after=5", func(sub *subscriber) {
		sub.queue <- message{"mission_event", map[string]any{"id": int64(5)}}
		sub.queue <- message{"mission_event", map[string]any{"id": int64(6)}}
		sub.queue <- message{"mission", map[string]any{"mission_id": "m"}}
		for !strings.Contains(w.text(), "event: mission\n") {
			time.Sleep(time.Millisecond)
		}
		drop(s.hub, sub)
	})
	if got := w.text(); strings.Count(got, "event: mission_event") != 1 || !strings.Contains(got, "id: 6\n") {
		t.Fatal(got)
	}
}

// A client gone mid-stream ends it, whatever was being sent.
func TestAStreamEndsWhenItsClientIsGone(t *testing.T) {
	for _, m := range []message{
		{"mission_event", map[string]any{"id": int64(9)}},
		{"mission", map[string]any{"mission_id": "m"}},
	} {
		s := newServer(t, &fakeStore{})
		following(t, s, &streamWriter{header: http.Header{}, refuse: true}, "/api/v1/stream", func(sub *subscriber) {
			sub.queue <- m
		})
	}
	s := newServer(t, &fakeStore{})
	s.keepalive = time.Millisecond // the keepalive is what finds the client gone
	if err := s.stream(&streamWriter{header: http.Header{}, refuse: true}, streamRequest("/api/v1/stream")); err != nil {
		t.Fatal(err)
	}
}

func TestASlowStreamIsDropped(t *testing.T) {
	h := newServer(t, &fakeStore{}).hub
	slow, other := h.subscribe("m"), h.subscribe("other")
	for i := 0; i <= streamQueue; i++ {
		h.publish("m", message{"mission_event", map[string]any{"id": int64(i)}})
	}
	if h.subs[slow] || !h.subs[other] || len(other.queue) != 0 {
		t.Fatal("the slow stream was not dropped alone")
	}
}

// fakeTemporal answers queries from answers (an error, or a value) and fails signals.
type fakeTemporal struct {
	client.Client
	answers map[string]any
}

type value struct{ v any }

func (v value) HasValue() bool { return v.v != nil }
func (v value) Get(out any) error {
	raw, _ := json.Marshal(v.v)
	return json.Unmarshal(raw, out)
}

func (f *fakeTemporal) QueryWorkflow(_ context.Context, _, _, name string, _ ...any) (converter.EncodedValue, error) {
	if err, ok := f.answers[name].(error); ok {
		return nil, err
	}
	return value{f.answers[name]}, nil
}

func (f *fakeTemporal) SignalWorkflow(context.Context, string, string, string, any) error {
	return errors.New("connection refused")
}

func TestTemporalAnswersTheFakeWorkflowNeverGives(t *testing.T) {
	ctx := context.Background()
	answers := map[string]any{
		durable.QueryStatus: "RUNNING", durable.QueryCycles: 2,
		durable.QueryGate: map[string]any{"gate_id": "g"}, // no options
	}
	state, err := queryLive(ctx, &fakeTemporal{answers: answers}, "w")
	if err != nil || state["gate"].(map[string]any)["options"] == nil || state["steer_notes"] == nil {
		t.Fatalf("%v %v", state, err)
	}
	for _, failing := range []string{durable.QueryCycles, durable.QueryOpenQuestion} {
		broken := map[string]any{durable.QueryStatus: "RUNNING", durable.QueryCycles: 1, failing: errBoom}
		if _, err := queryLive(ctx, &fakeTemporal{answers: broken}, "w"); !errors.Is(err, errBoom) {
			t.Errorf("%s failing: %v", failing, err)
		}
	}
	s := newServer(t, &fakeStore{})
	s.temporal.client = &fakeTemporal{}
	rec := call(s, "POST", "/api/v1/missions/m/steer", strings.NewReader(`{"note":"x"}`))
	if rec.Code != 503 || code(t, rec) != "temporal_unavailable" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestIsoformatOmitsZeroMicroseconds(t *testing.T) {
	at := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
	if got := isoformat(at); got != "2026-10-04T00:05:00+00:00" {
		t.Fatal(got)
	}
}

type unreadable struct{}

func (unreadable) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestAnUnreadableBody(t *testing.T) {
	s := newServer(t, &fakeStore{})
	if rec := call(s, "POST", "/api/v1/missions/m/steer", unreadable{}); rec.Code != 422 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := call(s, "POST", "/mcp", unreadable{}); rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

type closedPipe struct{}

func (closedPipe) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestMCPStdioStopsWhenItCannotAnswer(t *testing.T) {
	s := newServer(t, &fakeStore{})
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	if err := s.ServeMCPStdio(context.Background(), in, closedPipe{}); err == nil {
		t.Fatal("a failed write was ignored")
	}
}

// TestWitnessResultsAndNumOr covers the defensive branches the black-box cases cannot: a check
// that is not a map, a check naming another item's check, a non-list checks value, missing fields,
// duplicate witnesses, and a number that is not a json.Number.
func TestWitnessResultsAndNumOr(t *testing.T) {
	about := []persistence.EventRow{
		{Kind: "tool_call", Payload: map[string]any{"tool": "x"}},
		{Kind: "verify", Payload: map[string]any{"checks": "not a list"}},
		{Kind: "verify", Payload: map[string]any{"checks": []any{
			map[string]any{"name": "cmd:true", "passed": false, "exit_code": json.Number("1"),
				"gating": true, "timed_out": false, "duration_s": json.Number("0.2")},
			map[string]any{"name": "other", "passed": true},
			"nope",
		}}},
		{Kind: "verify", Payload: map[string]any{"checks": []any{
			map[string]any{"name": "cmd:true", "passed": true},
		}}},
	}
	item := map[string]any{"witnesses": []any{"cmd:true", "cmd:true", "go:T", 7}}
	want := []map[string]any{
		{"witness": "cmd:true", "latest": map[string]any{
			"passed": true, "exit_code": 0, "gating": true, "timed_out": false, "duration_s": float64(0)}},
		{"witness": "go:T", "latest": nil},
	}
	if got := witnessResults(item, about); !reflect.DeepEqual(got, want) {
		t.Fatalf("witnessResults = %#v", got)
	}
	if out := witnessResults(map[string]any{}, nil); len(out) != 0 {
		t.Fatalf("no witnesses should mean no results: %#v", out)
	}
	if numOr("x", 3) != 3 || numOr(json.Number("bad"), 4) != 4 || numOr(json.Number("2.5"), 0) != 2.5 {
		t.Fatal("numOr")
	}
}
