package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// The Voyage embedder over a mocked Voyage API (no network): probe, request bodies and batching,
// input_type for documents vs queries, retries on 429/5xx, 401/403 without retry, key redaction,
// egress (https, public addresses, pinned dialling), pgvector width handling and degradation.

var voyageKey = "pa-" + strings.Repeat("k3y", 12)

const voyagePublic = "93.184.216.34"

func publicResolver(context.Context, string, int) ([]string, error) {
	return []string{voyagePublic}, nil
}

type voyageBody struct {
	Input     []string `json:"input"`
	Model     string   `json:"model"`
	InputType string   `json:"input_type"`
}

// fakeVoyage serves POST /v1/embeddings like the Voyage API, with scripted failures.
type fakeVoyage struct {
	mu       sync.Mutex
	dim      int
	bodies   []voyageBody
	headers  []http.Header
	urls     []string
	statuses []int // served (then dropped) before answering 200
	always   int   // a status every request gets
	down     bool
	reverse  bool
}

func newFakeVoyage(dim int) *fakeVoyage { return &fakeVoyage{dim: dim} }

func (f *fakeVoyage) vector(text string) []float64 {
	base := semanticVector(text)
	out := make([]float64, f.dim)
	copy(out, base)
	return out
}

func (f *fakeVoyage) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("connection refused")
	}
	var body voyageBody
	_ = json.NewDecoder(req.Body).Decode(&body)
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, req.Header.Clone())
	f.urls = append(f.urls, req.URL.String())
	status := f.always
	if status == 0 {
		status = 200
		if len(f.statuses) > 0 {
			status, f.statuses = f.statuses[0], f.statuses[1:]
		}
	}
	respond := func(status int, v any) (*http.Response, error) {
		data, _ := json.Marshal(v)
		return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{"Retry-After": {"7"}}, Request: req}, nil
	}
	if status != 200 {
		return respond(status, map[string]string{"detail": "nope"})
	}
	data := []map[string]any{}
	for i, text := range body.Input {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": f.vector(text)})
	}
	if f.reverse {
		for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
			data[i], data[j] = data[j], data[i]
		}
	}
	return respond(200, map[string]any{"object": "list", "data": data, "model": body.Model})
}

func (f *fakeVoyage) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// noSleep replaces the retry sleeps for one test and records them.
func noSleep(t *testing.T) *[]float64 {
	t.Helper()
	slept := &[]float64{}
	var mu sync.Mutex
	saved := VoyageRetry
	VoyageRetry.Sleep = func(_ context.Context, s float64) error {
		mu.Lock()
		*slept = append(*slept, s)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { VoyageRetry = saved })
	return slept
}

func connectVoyage(t *testing.T, fake *fakeVoyage, edit func(*VoyageOptions)) (*VoyageEmbedder, error) {
	t.Helper()
	o := VoyageOptions{APIKey: voyageKey, Resolver: publicResolver, Transport: fake}
	if edit != nil {
		edit(&o)
	}
	return ConnectVoyage(bg, o)
}

func TestConnectVoyageProbesTheKeyAndDimension(t *testing.T) {
	noSleep(t)
	fake := newFakeVoyage(8)
	e, err := connectVoyage(t, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name() != "voyage:voyage-4" || e.Version() != "voyage-4" || e.Dim() != 8 {
		t.Fatal(e.Name(), e.Version(), e.Dim())
	}
	want := voyageBody{Input: []string{"dimension probe"}, Model: "voyage-4", InputType: "document"}
	if len(fake.bodies) != 1 || fmt.Sprint(fake.bodies[0]) != fmt.Sprint(want) || fake.urls[0] != VoyageEndpoint {
		t.Fatal(fake.bodies, fake.urls)
	}
	if fake.headers[0].Get("Authorization") != "Bearer "+voyageKey || fake.headers[0].Get("Content-Type") != "application/json" {
		t.Fatal(fake.headers[0])
	}
	if s := e.String(); strings.Contains(s, voyageKey) || !strings.Contains(s, "voyage-4") {
		t.Fatal(s)
	}
	_ = e.Close()
}

func TestVoyageDocumentsAndQueriesUseTheirInputTypeAndBatches(t *testing.T) {
	noSleep(t)
	fake := newFakeVoyage(8)
	fake.reverse = true // vectors are put back in input order by data[].index
	e, err := connectVoyage(t, fake, func(o *VoyageOptions) { o.Model = "voyage-3.5-lite" })
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{}
	for i := 0; i < VoyageBatchTexts+2; i++ {
		if i%2 == 1 {
			texts = append(texts, fmt.Sprintf("port %d", i))
		} else {
			texts = append(texts, fmt.Sprintf("sql %d", i))
		}
	}
	vectors := must(e.Embed(bg, texts))
	if len(fake.bodies) != 3 || len(fake.bodies[1].Input) != VoyageBatchTexts || len(fake.bodies[2].Input) != 2 ||
		fake.bodies[1].InputType != "document" || fake.bodies[2].InputType != "document" {
		t.Fatal(len(fake.bodies))
	}
	if fmt.Sprint(vectors[1]) != fmt.Sprint(fake.vector("port 1")) || fmt.Sprint(vectors[0]) != fmt.Sprint(fake.vector("sql 0")) {
		t.Fatal("vectors out of order")
	}
	query := must(e.EmbedQuery(bg, []string{"listen socket"}))
	last := fake.bodies[len(fake.bodies)-1]
	if last.InputType != "query" || last.Model != "voyage-3.5-lite" || last.Input[0] != "listen socket" {
		t.Fatal(last)
	}
	if c, _ := Cosine(query[0], vectors[1]); c < 0.99 {
		t.Fatal(c)
	}
	if got := must(e.Embed(bg, nil)); len(got) != 0 || len(fake.bodies) != 4 {
		t.Fatal("an empty embed must send nothing")
	}
	_ = e.Close()
}

func TestEmbedQueriesFallsBackAndPaddingKeepsTheInputType(t *testing.T) {
	noSleep(t)
	h := NewHashEmbedder(4)
	if a, b := must(EmbedQueries(bg, h, []string{"a"})), must(h.Embed(bg, []string{"a"})); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatal(a, b)
	}
	fake := newFakeVoyage(8)
	inner, err := connectVoyage(t, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	padded, _ := NewPaddedEmbedder(inner, 16)
	if v := must(padded.EmbedQuery(bg, []string{"listen port"})); len(v[0]) != 16 || fake.bodies[len(fake.bodies)-1].InputType != "query" {
		t.Fatal(v)
	}
	if v := must(padded.Embed(bg, []string{"listen port"})); len(v[0]) != 16 || fake.bodies[len(fake.bodies)-1].InputType != "document" {
		t.Fatal(v)
	}
	if _, err := NewPaddedEmbedder(inner, 16); err != nil {
		t.Fatal(err)
	}
	_ = padded.Close()
}

func TestVoyageRetriesTransientErrorsWithBackoff(t *testing.T) {
	slept := noSleep(t)
	fake := newFakeVoyage(8)
	fake.statuses = []int{429, 503}
	e, err := connectVoyage(t, fake, nil)
	if err != nil || e.Dim() != 8 || len(fake.bodies) != 3 {
		t.Fatal(err, len(fake.bodies))
	}
	if fmt.Sprint(*slept) != "[7 7]" { // the server's Retry-After, not blind doubling
		t.Fatal(*slept)
	}
}

func TestConnectReportsWhyVoyageIsUnusable(t *testing.T) {
	for _, c := range []struct {
		setup    func(*fakeVoyage)
		message  string
		requests int
	}{
		{func(f *fakeVoyage) { f.always = 401 }, "rejected the API key for 'voyage-4' (HTTP 401)", 1},
		{func(f *fakeVoyage) { f.always = 403 }, "rejected the API key for 'voyage-4' (HTTP 403)", 1},
		{func(f *fakeVoyage) { f.always = 400 }, "could not embed with 'voyage-4' (HTTP 400)", 1},
		{func(f *fakeVoyage) { f.always = 429 }, "could not embed with 'voyage-4' (HTTP 429)", 4},
		{func(f *fakeVoyage) { f.always = 500 }, "could not embed with 'voyage-4' (HTTP 500)", 4},
		{func(f *fakeVoyage) { f.down = true }, "voyage unreachable at " + VoyageEndpoint, 0},
	} {
		noSleep(t)
		fake := newFakeVoyage(8)
		c.setup(fake)
		_, err := connectVoyage(t, fake, nil)
		var unavailable *VoyageUnavailableError
		if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), c.message) || strings.Contains(err.Error(), voyageKey) {
			t.Errorf("%s: %v", c.message, err)
		}
		if fake.count() != c.requests {
			t.Errorf("%s: %d requests, want %d", c.message, fake.count(), c.requests)
		}
	}
}

type staticVoyage string

func (s staticVoyage) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(s))), Header: http.Header{}, Request: req}, nil
}

func TestAMalformedVoyageAnswerMakesItUnusable(t *testing.T) {
	noSleep(t)
	_, err := ConnectVoyage(bg, VoyageOptions{APIKey: voyageKey, Resolver: publicResolver, Transport: staticVoyage(`{"data": []}`)})
	if err == nil || !strings.Contains(err.Error(), "0 vectors for 1 texts") {
		t.Fatal(err)
	}
	_, err = ConnectVoyage(bg, VoyageOptions{APIKey: voyageKey, Resolver: publicResolver, Transport: staticVoyage(`not json`)})
	if err == nil || !strings.Contains(err.Error(), "could not embed with 'voyage-4' (JSONDecodeError") {
		t.Fatal(err)
	}
	fake := newFakeVoyage(8)
	e, err := connectVoyage(t, fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake.dim = 4 // the API changed width mid-run: never mix widths in one index
	if _, err := e.Embed(bg, []string{"port"}); err == nil || !strings.Contains(err.Error(), "dim is not 8") {
		t.Fatal(err)
	}
}

func TestAMissingKeyOrARefusedEndpointNeverCallsOut(t *testing.T) {
	noSleep(t)
	fake := newFakeVoyage(8)
	private := func(context.Context, string, int) ([]string, error) { return []string{"10.0.0.5"}, nil }
	unresolvable := func(context.Context, string, int) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "api.voyageai.com", IsNotFound: true}
	}
	for message, edit := range map[string]func(*VoyageOptions){
		"LHA_VOYAGE_API_KEY is not set": func(o *VoyageOptions) { o.APIKey = "  " },
		"must use https":                func(o *VoyageOptions) { o.Endpoint = "http://api.voyageai.com/v1/embeddings" },
		"endpoint refused":              func(o *VoyageOptions) { o.Endpoint = "https://user:pw@api.voyageai.com/v1/embeddings" },
		"non-public address":            func(o *VoyageOptions) { o.Resolver = private },
		"cannot resolve":                func(o *VoyageOptions) { o.Resolver = unresolvable },
	} {
		_, err := connectVoyage(t, fake, edit)
		var unavailable *VoyageUnavailableError
		if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v", message, err)
		}
	}
	if fake.count() != 0 {
		t.Fatal("a refused endpoint was called", fake.bodies)
	}
}

func TestVoyageConnectionsDialOnlyTheVettedAddresses(t *testing.T) {
	slept := noSleep(t)
	var mu sync.Mutex
	targets := []string{}
	dial := func(_ context.Context, _, address string) (net.Conn, error) {
		mu.Lock()
		targets = append(targets, address)
		mu.Unlock()
		return nil, errors.New("refused by the test")
	}
	_, err := ConnectVoyage(bg, VoyageOptions{APIKey: voyageKey, Endpoint: "https://voyage.example:8443/v1/embeddings",
		Resolver: publicResolver, Dial: dial})
	if err == nil || !strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), voyageKey) {
		t.Fatal(err)
	}
	// The name was resolved by the egress check; the socket went to that address only (retried as
	// a transient connection error).
	want := voyagePublic + ":8443"
	if len(targets) != 4 || targets[0] != want || targets[3] != want || len(*slept) != 3 {
		t.Fatal(targets, *slept)
	}
}

func TestTheVoyageKeyIsASecretEverywhere(t *testing.T) {
	s := settingsFrom(t, "LHA_MEMORY_EMBEDDER=voyage", "LHA_VOYAGE_API_KEY="+voyageKey)
	found := false
	for _, kv := range s.Redacted() {
		if kv.Key == "voyage_api_key" {
			found = true
			if fmt.Sprint(kv.Value) != "***" {
				t.Fatal(kv)
			}
		}
	}
	if !found || s.VoyageAPIKey.Value() != voyageKey {
		t.Fatal("voyage_api_key not a setting")
	}
	if got := obs.RedactText("sent " + voyageKey + " to voyage"); got != "sent *** to voyage" {
		t.Fatal(got)
	}
	if EmbeddingModel(s) != "voyage-4" {
		t.Fatal(EmbeddingModel(s))
	}
	if code := EmbeddingModel(settingsFrom(t, "LHA_MEMORY_EMBEDDER=voyage", "LHA_MEMORY_EMBEDDING_MODEL=voyage-code-3")); code != "voyage-code-3" {
		t.Fatal(code)
	}
}

// --- through the memory service ---------------------------------------------------------------

func voyageSettings(t *testing.T, extra ...string) *config.Settings {
	return settingsFrom(t, append([]string{"LHA_MEMORY_EMBEDDER=voyage", "LHA_VOYAGE_API_KEY=" + voyageKey}, extra...)...)
}

func TestVoyageMemoryRecallsByMeaningAndGatesByModelVersion(t *testing.T) {
	noSleep(t)
	fake := newFakeVoyage(8)
	store := openStore(t)
	mem := OpenMissionMemory(bg, voyageSettings(t, "LHA_VOYAGE_ENDPOINT=https://voyage.example/v1/embeddings"), store,
		ollamaRepo(t), "m1", OpenOptions{EmbedderTransport: fake, EmbedderResolver: publicResolver})
	if mem.Mode().Label() != "hybrid" || mem.Embedder().Name() != "voyage:voyage-4" {
		t.Fatalf("%+v", mem.Mode())
	}
	block := mem.Recall(bg, "m1", "c1", item("01", "make the listen port configurable"), contracts.SituationSnapshot{})
	if !strings.Contains(block, "net.py") {
		t.Fatal(block)
	}
	queries := [][]string{}
	for i, b := range fake.bodies {
		if fake.urls[i] != "https://voyage.example/v1/embeddings" {
			t.Fatal(fake.urls[i])
		}
		if b.InputType == "query" {
			queries = append(queries, b.Input)
		}
	}
	if fmt.Sprint(queries) != "[[make the listen port configurable]]" { // the item as a query, chunks as documents
		t.Fatal(queries)
	}
	mem.ObserveCycle(bg, CycleObservation{MissionID: "m1", CycleID: "c1", ItemID: "01", ItemDescription: "listen port",
		Verdict: "passed", Verified: true, Status: "done", Attempts: 1, DoneSummary: "moved the socket number to settings"})
	rows := must(store.MemoryVectors(bg, "m1", "voyage:voyage-4", "voyage-4", 0))
	if len(rows) == 0 || len(rows[0].Vector) != 8 {
		t.Fatal(rows)
	}
	if other := must(store.MemoryVectors(bg, "m1", "voyage:voyage-3.5", "voyage-3.5", 0)); len(other) != 0 {
		t.Fatal("another model compared against these vectors")
	}
	_ = mem.Close()
}

func TestUnusableVoyageMeansLexicalMemoryNotAFailure(t *testing.T) {
	for _, c := range []struct {
		env    []string
		setup  func(*fakeVoyage)
		reason string
	}{
		{[]string{"LHA_MEMORY_EMBEDDER=voyage"}, nil, "LHA_VOYAGE_API_KEY is not set"},
		{nil, func(f *fakeVoyage) { f.always = 401 }, "rejected the API key"},
		{nil, func(f *fakeVoyage) { f.down = true }, "voyage unreachable"},
	} {
		noSleep(t)
		fake := newFakeVoyage(8)
		if c.setup != nil {
			c.setup(fake)
		}
		s := voyageSettings(t)
		if c.env != nil {
			s = settingsFrom(t, c.env...)
		}
		recorder := obs.NewTraceRecorder(nil)
		mem := OpenMissionMemory(bg, s, openStore(t), ollamaRepo(t), "m1",
			OpenOptions{EmbedderTransport: fake, EmbedderResolver: publicResolver, Recorder: recorder})
		if mem.Embedder() != nil || mem.Mode().Label() != "lexical" {
			t.Fatalf("%s: %+v", c.reason, mem.Mode())
		}
		reasons := eventData(recorder, "memory_degraded", "reason")
		if len(reasons) != 1 || !strings.Contains(reasons[0], c.reason) || strings.Contains(reasons[0], voyageKey) {
			t.Fatal(reasons)
		}
	}
}

func TestVoyageFailingMidRunDegradesToLexical(t *testing.T) {
	noSleep(t)
	fake := newFakeVoyage(8)
	recorder := obs.NewTraceRecorder(nil)
	mem := OpenMissionMemory(bg, voyageSettings(t), openStore(t), ollamaRepo(t), "m1",
		OpenOptions{EmbedderTransport: fake, EmbedderResolver: publicResolver, Recorder: recorder})
	if !mem.Mode().Dense {
		t.Fatal("not hybrid")
	}
	fake.mu.Lock()
	fake.always = 503 // the API stops serving after the run started (retried, then dropped)
	fake.mu.Unlock()
	block := mem.Recall(bg, "m1", "c1", item("01", "the sql database"), contracts.SituationSnapshot{})
	if !strings.Contains(block, "db.py") || mem.Embedder() != nil || mem.Mode().Label() != "lexical" {
		t.Fatal(block)
	}
	if fake.count() != 1+4 { // probe, then one call and its three retries
		t.Fatal(fake.count())
	}
	if len(eventData(recorder, "memory_degraded", "reason")) == 0 {
		t.Fatal("no memory_degraded")
	}
}

func TestVoyageWidthAgainstThePgvectorColumn(t *testing.T) {
	for _, c := range []struct {
		dim   int
		label string
	}{{1024, "hybrid"}, {512, "hybrid"}, {2048, "lexical"}} {
		noSleep(t)
		recorder := obs.NewTraceRecorder(nil)
		mem := OpenMissionMemory(bg, voyageSettings(t), fakePG{openStore(t)}, t.TempDir(), "m",
			OpenOptions{EmbedderTransport: newFakeVoyage(c.dim), EmbedderResolver: publicResolver, Recorder: recorder})
		if mem.Mode().Label() != c.label {
			t.Fatalf("dim %d: %+v", c.dim, mem.Mode())
		}
		switch {
		case c.dim < PGEmbeddingDim:
			padded, ok := mem.Embedder().(*PaddedEmbedder)
			if !ok || padded.Dim() != PGEmbeddingDim || padded.Version() != "voyage-4" {
				t.Fatalf("dim %d not padded", c.dim)
			}
		case c.dim > PGEmbeddingDim: // never truncated: a wider model runs lexical-only
			reasons := eventData(recorder, "memory_degraded", "reason")
			if len(reasons) != 1 || !strings.Contains(reasons[0], "embedder dim 2048 > vector(1024) column") {
				t.Fatal(reasons)
			}
		}
		_ = mem.Close()
	}
}
