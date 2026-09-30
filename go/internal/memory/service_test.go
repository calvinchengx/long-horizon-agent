package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/ops"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from python/tests/unit/test_memory_service.py and test_memory_ollama.py (the run-path
// tests live in internal/agent).

func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	if err := state.InitRepo(bg, ws); err != nil {
		t.Fatal(err)
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := state.CommitAll(bg, ws, "init"); err != nil {
		t.Fatal(err)
	}
	return ws
}

func openStore(t *testing.T) *persistence.SQLiteStore {
	t.Helper()
	store, err := persistence.OpenSQLite(bg, filepath.Join(t.TempDir(), "mem.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newMemory(t *testing.T, ws string, recorder *obs.TraceRecorder, tweak func(*Config)) *MissionMemory {
	t.Helper()
	cfg := DefaultConfig()
	if tweak != nil {
		tweak(&cfg)
	}
	return New(Options{Store: openStore(t), Workdir: ws, Config: cfg, Mode: ops.DecideMemoryMode(nil),
		Embedder: NewHashEmbedder(0), Recorder: recorder})
}

func observation(cycle string, verified bool, tweak func(*CycleObservation)) CycleObservation {
	o := CycleObservation{MissionID: "m1", CycleID: cycle, ItemID: "01", ItemDescription: "fix the config parser",
		Verdict: "failed", Verified: verified, Status: "in_progress", Attempts: 1,
		Failure: "- pytest FAILED (exit 1): KeyError 'port'", DoneSummary: "handled missing port key",
		Tools: []string{"read_file", "write_file"}}
	if verified {
		o.Verdict, o.Status, o.Failure = "passed", "done", ""
	}
	if tweak != nil {
		tweak(&o)
	}
	return o
}

func item(id, description string) contracts.ChecklistItem {
	return contracts.NewChecklistItem(id, description)
}

func kinds(recorder *obs.TraceRecorder) []string {
	out := []string{}
	for _, e := range recorder.Events() {
		out = append(out, e.Kind)
	}
	return out
}

func eventData(recorder *obs.TraceRecorder, kind, key string) []string {
	out := []string{}
	for _, e := range recorder.Events() {
		if e.Kind == kind {
			v, _ := e.Data.Get(key)
			s, _ := v.(string)
			out = append(out, s)
		}
	}
	return out
}

// --- degradation rules -----------------------------------------------------------------------------

func TestDecideMemoryModeDropsDenseWhenAMemoryDepIsDown(t *testing.T) {
	if m := ops.DecideMemoryMode(nil); !m.Dense || m.GitGrep || m.Label() != "hybrid" {
		t.Fatalf("%+v", m)
	}
	for _, dep := range []string{"postgres", "pgvector", "embeddings"} {
		m := ops.DecideMemoryMode([]ops.DependencyStatus{{Name: dep, Health: ops.HealthDown, Detail: "gone"}})
		if m.Dense || !m.GitGrep || m.Label() != "lexical" || len(m.Degraded) != 1 || m.Degraded[0] != dep ||
			!strings.Contains(m.Reason, "gone") || !strings.Contains(m.Reason, "git grep") {
			t.Fatalf("%s: %+v", dep, m)
		}
	}
	if !ops.DecideMemoryMode([]ops.DependencyStatus{{Name: "langfuse", Health: ops.HealthDown}}).Dense {
		t.Fatal("non-memory dep degraded memory")
	}
	if park := ops.DecideSafePark([]ops.DependencyStatus{{Name: "pgvector", Health: ops.HealthDown}}); park.Park || park.Degraded[0] != "pgvector" {
		t.Fatalf("%+v", park)
	}
	if park := ops.DecideSafePark([]ops.DependencyStatus{{Name: "git", Health: ops.HealthDown}}); !park.Park || park.Reason != "critical dependency down: ['git']" {
		t.Fatalf("%+v", park)
	}
}

func TestRenderMemoryBlockRespectsTheBudget(t *testing.T) {
	lines := func(prefix string) []string {
		out := []string{}
		for i := 0; i < 10; i++ {
			out = append(out, "- "+prefix+" "+strings.Repeat("x", 300))
		}
		return out
	}
	sections := []MemorySection{{"Earlier attempts", lines("attempt")}, {"Skills", nil}, {"Related", lines("fact")}}
	for _, budget := range []int{200, 800, 2000, 4000} {
		block, err := RenderMemoryBlock(sections, budget, Weights)
		if err != nil || len([]rune(block)) > budget || !strings.HasPrefix(block, MemoryHeader) || strings.Contains(block, "Skills:") {
			t.Fatalf("%d: %q %v", budget, block, err)
		}
	}
	if block, _ := RenderMemoryBlock(sections, 2000, Weights); !strings.Contains(block, "Earlier attempts:") || !strings.Contains(block, "Related:") {
		t.Fatal(block)
	}
	if block, _ := RenderMemoryBlock([]MemorySection{{"A", nil}}, 4000, nil); block != "" {
		t.Fatal(block)
	}
	if block, _ := RenderMemoryBlock([]MemorySection{{"A", []string{"- a"}}}, 10, nil); block != "" {
		t.Fatal(block)
	}
	if _, err := RenderMemoryBlock([]MemorySection{{"A", []string{"- a"}}}, 500, []float64{1, 2}); err == nil {
		t.Fatal("weights mismatch accepted")
	}
}

// --- skills ----------------------------------------------------------------------------------------

func TestVerifiedSuccessStoresASkillRecalledForARelatedItem(t *testing.T) {
	ws := repo(t, map[string]string{"greet.py": "def greet(name):\n    return name\n"})
	recorder := obs.NewTraceRecorder(nil)
	mem := newMemory(t, ws, recorder, nil)
	mem.ObserveCycle(bg, observation("c1", true, func(o *CycleObservation) {
		o.ItemDescription = "add a greeting helper"
		o.DoneSummary = "wrote greet() in greet.py returning 'Hello, <name>'"
	}))
	if k := kinds(recorder); len(k) != 1 || k[0] != "skill_stored" {
		t.Fatal(k)
	}
	skills, _ := mem.Store.ListSkills(bg, mem.Namespace, 0)
	if len(skills) != 1 || !skills[0].Verified || !strings.Contains(skills[0].Code, "wrote greet()") || !strings.Contains(skills[0].Code, "write_file") {
		t.Fatalf("%+v", skills)
	}
	if !strings.HasPrefix(skills[0].ID, "skill_") || len(skills[0].ID) != 26 || skills[0].ExpiresAt != time.Now().AddDate(0, 0, 90).Format("2006-01-02") {
		t.Fatalf("%+v", skills[0])
	}
	block := mem.Recall(bg, "m2", "c1", item("07", "add a greeting helper for admins"), contracts.SituationSnapshot{})
	if !strings.Contains(block, "Skills that worked before:") || !strings.Contains(block, "wrote greet()") {
		t.Fatal(block)
	}
	mem.ObserveCycle(bg, observation("c2", false, func(o *CycleObservation) { o.ItemDescription = "break things" }))
	if skills, _ := mem.Store.ListSkills(bg, mem.Namespace, 0); len(skills) != 1 {
		t.Fatal("a failure became a skill")
	}
	if skills, _ := mem.Store.ListSkills(bg, "/some/other/repo", 0); len(skills) != 0 {
		t.Fatal("namespace leak")
	}
}

func TestExpiredSkillsAreNotRecalled(t *testing.T) {
	mem := newMemory(t, repo(t, map[string]string{"a.py": "x = 1\n"}), nil, nil)
	skill, err := mem.StoreSkill(bg, observation("c1", true, func(o *CycleObservation) { o.ItemDescription = "add a greeting helper" }), nil)
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := mem.FindSkills(bg, "greeting helper", 3, ""); len(found) != 1 || found[0].ID != skill.ID {
		t.Fatal(found)
	}
	skill.ExpiresAt = "2000-01-01"
	_ = mem.Store.PutSkill(bg, skill)
	if found, _ := mem.FindSkills(bg, "greeting helper", 3, ""); len(found) != 0 {
		t.Fatal(found)
	}
}

// --- consolidation ---------------------------------------------------------------------------------

func TestExtractiveConsolidationCompactsEpisodesIntoFacts(t *testing.T) {
	recorder := obs.NewTraceRecorder(nil)
	mem := newMemory(t, repo(t, map[string]string{"a.py": "x = 1\n"}), recorder, func(c *Config) { c.ConsolidateEvery = 2 })
	store := mem.Store
	mem.ObserveCycle(bg, observation("c1", false, nil))
	if marks, _ := store.ListEvents(bg, "m1", persistence.EventQuery{Kinds: []string{ConsolidationKind}}); len(marks) != 0 {
		t.Fatal(marks)
	}
	mem.ObserveCycle(bg, observation("c2", true, func(o *CycleObservation) { o.Attempts = 2 }))
	marks, _ := store.ListEvents(bg, "m1", persistence.EventQuery{Kinds: []string{ConsolidationKind}})
	if len(marks) != 1 || marks[0].Payload["episodes"].(json.Number).String() != "2" || marks[0].Payload["mode"] != "extractive" {
		t.Fatalf("%+v", marks)
	}
	records, _ := store.ListMemory(bg, "m1", 0)
	facts := []contracts.MemoryRecord{}
	for _, r := range records {
		switch r.Kind {
		case "fact":
			facts = append(facts, r)
		case "progress":
			t.Fatal("progress note not invalidated")
		}
	}
	if len(facts) != 1 || !strings.Contains(facts[0].Text, "verified in c2 after 2 attempt(s)") ||
		!strings.Contains(facts[0].Text, "handled missing port key") || !strings.Contains(facts[0].Text, "KeyError") {
		t.Fatalf("%+v", facts)
	}
	if !strings.Contains(strings.Join(kinds(recorder), ","), "memory_consolidated") {
		t.Fatal(kinds(recorder))
	}
	mem.ObserveCycle(bg, observation("c3", false, func(o *CycleObservation) { o.ItemID = "02" }))
	mem.ObserveCycle(bg, observation("c4", false, func(o *CycleObservation) { o.ItemID = "02" }))
	if marks, _ := store.ListEvents(bg, "m1", persistence.EventQuery{Kinds: []string{ConsolidationKind}}); len(marks) != 2 {
		t.Fatal(marks)
	}
	ids := []string{}
	for _, r := range must(store.ListMemory(bg, "m1", 0)) {
		if r.Kind == "fact" {
			ids = append(ids, r.ID)
		}
	}
	if strings.Join(ids, ",") != "fact:m1:item:01,fact:m1:item:02" {
		t.Fatal(ids)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func factTexts(t *testing.T, store persistence.Store) []string {
	out := []string{}
	for _, r := range must(store.ListMemory(bg, "m1", 0)) {
		if r.Kind == "fact" {
			out = append(out, r.Text)
		}
	}
	return out
}

func TestModelConsolidationUsesTheModelAndSurvivesBadOutput(t *testing.T) {
	mem := newMemory(t, repo(t, map[string]string{"a.py": "x = 1\n"}), nil, func(c *Config) {
		c.ConsolidateEvery, c.Consolidation = 1, "model"
	})
	mem.Model = model.NewStub([]contracts.TurnResult{{Text: `["the parser needs a port default"]`}})
	mem.ObserveCycle(bg, observation("c1", false, nil))
	if facts := factTexts(t, mem.Store); len(facts) != 1 || facts[0] != "the parser needs a port default" {
		t.Fatal(facts)
	}
	mem.Model = model.NewStub([]contracts.TurnResult{{Text: "not json"}})
	mem.ObserveCycle(bg, observation("c2", false, nil))
	marks, _ := mem.Store.ListEvents(bg, "m1", persistence.EventQuery{Kinds: []string{ConsolidationKind}})
	if marks[len(marks)-1].Payload["failed_batches"].(json.Number).String() != "1" || len(factTexts(t, mem.Store)) != 2 {
		t.Fatalf("%+v", marks)
	}
}

type boomModel struct{ *model.StubModel }

func (boomModel) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, errors.New("budget refused")
}

func TestConsolidationFailureIsRecordedNotRaised(t *testing.T) {
	recorder := obs.NewTraceRecorder(nil)
	mem := newMemory(t, repo(t, map[string]string{"a.py": "x = 1\n"}), recorder, func(c *Config) {
		c.ConsolidateEvery, c.Consolidation = 1, "model"
	})
	mem.Model = boomModel{model.NewStub(nil)}
	mem.ObserveCycle(bg, observation("c1", false, nil))
	if !strings.Contains(strings.Join(kinds(recorder), ","), "memory_error") {
		t.Fatal(kinds(recorder))
	}
	if marks, _ := mem.Store.ListEvents(bg, "m1", persistence.EventQuery{Kinds: []string{ConsolidationKind}}); len(marks) != 0 {
		t.Fatal(marks)
	}
	if eps, _ := mem.Store.ListEvents(bg, "m1", persistence.EventQuery{Kinds: []string{EpisodeKind}}); len(eps) != 1 {
		t.Fatal(eps)
	}
}

// --- degradation -----------------------------------------------------------------------------------

func settingsFrom(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	s, err := config.LoadFrom(append([]string{"LHA_MODEL_BACKEND=stub"}, env...), "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMissingEmbedderExtraFallsBackToLexicalAndGitGrep(t *testing.T) {
	ws := repo(t, map[string]string{"server.py": "PORT = 8080\n\ndef listen(port=PORT):\n    return port\n"})
	recorder := obs.NewTraceRecorder(nil)
	mem := OpenMissionMemory(bg, settingsFrom(t, "LHA_MEMORY_EMBEDDER=sentence_transformers", "LHA_MEMORY_INDEX_REPO_FILES=false"),
		openStore(t), ws, "m1", OpenOptions{Recorder: recorder})
	if mem == nil || mem.Embedder() != nil || mem.Mode().Label() != "lexical" || !mem.Mode().GitGrep {
		t.Fatalf("%+v", mem.Mode())
	}
	if reasons := eventData(recorder, "memory_degraded", "reason"); len(reasons) != 1 || !strings.Contains(reasons[0], "sentence-transformers unavailable") {
		t.Fatal(reasons)
	}
	block := mem.Recall(bg, "m1", "c1", item("01", "make the listen port configurable"), contracts.SituationSnapshot{})
	if !strings.Contains(block, "(grep) server.py") {
		t.Fatal(block)
	}
}

func TestPostgresFallbackStoreMeansLexicalMemory(t *testing.T) {
	store := openStore(t)
	store.SetDegradedReason("postgres unavailable (OperationalError: refused)")
	mem := OpenMissionMemory(bg, settingsFrom(t), store, t.TempDir(), "m1", OpenOptions{})
	if mem.Mode().Label() != "lexical" || strings.Join(mem.Mode().Degraded, ",") != "postgres" || !strings.Contains(mem.Mode().Reason, "refused") {
		t.Fatalf("%+v", mem.Mode())
	}
}

// fakePG claims to be Postgres (for the open-time pgvector checks only).
type fakePG struct{ *persistence.SQLiteStore }

func (fakePG) Backend() string { return persistence.BackendPostgres }

func sized(dim int) func(string) (contracts.Embedder, error) {
	return func(string) (contracts.Embedder, error) { return NewHashEmbedder(dim), nil }
}

func TestPostgresMemoryRejectsAnEmbedderWiderThanTheColumn(t *testing.T) {
	mem := OpenMissionMemory(bg, settingsFrom(t, "LHA_MEMORY_EMBEDDER=sentence_transformers"), fakePG{openStore(t)},
		t.TempDir(), "m", OpenOptions{SentenceTransformer: sized(2048)})
	if mem.Mode().Label() != "lexical" || !strings.Contains(mem.Mode().Reason, "2048") || !strings.Contains(mem.Mode().Reason, "vector(1024)") {
		t.Fatalf("%+v", mem.Mode())
	}
}

func TestPostgresMemoryPadsANarrowerEmbedderToTheColumn(t *testing.T) {
	mem := OpenMissionMemory(bg, settingsFrom(t, "LHA_MEMORY_EMBEDDER=sentence_transformers"), fakePG{openStore(t)},
		t.TempDir(), "m", OpenOptions{SentenceTransformer: sized(384)})
	padded, isPadded := mem.Embedder().(*PaddedEmbedder)
	if mem.Mode().Label() != "hybrid" || !isPadded || padded.Dim() != 1024 || padded.Name() != "hash" || padded.Inner.Dim() != 384 {
		t.Fatalf("%+v", mem.Mode())
	}
	vectors, _ := padded.Embed(bg, []string{"listen port"})
	for _, v := range vectors[0][384:] {
		if v != 0 {
			t.Fatal("not zero-padded")
		}
	}
	if len(vectors[0]) != 1024 {
		t.Fatal(len(vectors[0]))
	}
	_ = mem.Close()
}

func TestPostgresMemoryIsHybridWithA1024WideEmbedder(t *testing.T) {
	mem := OpenMissionMemory(bg, settingsFrom(t), fakePG{openStore(t)}, t.TempDir(), "m", OpenOptions{})
	if mem.Mode().Label() != "hybrid" || mem.Embedder().Dim() != 1024 {
		t.Fatalf("%+v", mem.Mode())
	}
}

func TestUnavailableCrossEncoderKeepsFusionOrder(t *testing.T) {
	store := openStore(t)
	mem := OpenMissionMemory(bg, settingsFrom(t, "LHA_MEMORY_RERANK=cross_encoder"), store, t.TempDir(), "m", OpenOptions{})
	if _, isNoop := mem.Reranker.(NoopReranker); !isNoop {
		t.Fatalf("%T", mem.Reranker)
	}
	if OpenMissionMemory(bg, settingsFrom(t, "LHA_MEMORY_ENABLED=false"), store, t.TempDir(), "m", OpenOptions{}) != nil {
		t.Fatal("memory opened while disabled")
	}
}

type flaky struct {
	*HashEmbedder
	mu     sync.Mutex
	broken bool
}

func (f *flaky) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	f.mu.Lock()
	broken := f.broken
	f.mu.Unlock()
	if broken {
		return nil, errors.New("vector backend went away")
	}
	return f.HashEmbedder.Embed(ctx, texts)
}

func TestDenseFailureMidRunDegradesAndTheMissionContinues(t *testing.T) {
	ws := repo(t, map[string]string{"config_parser.py": "def parse_config(text):\n    return {}\n"})
	recorder := obs.NewTraceRecorder(nil)
	mem := newMemory(t, ws, recorder, nil)
	embedder := &flaky{HashEmbedder: NewHashEmbedder(0)}
	mem.SetEmbedder(embedder)
	mem.ObserveCycle(bg, observation("c1", false, nil))
	embedder.mu.Lock()
	embedder.broken = true
	embedder.mu.Unlock()
	block := mem.Recall(bg, "m1", "c2", item("02", "fix the config parser"), contracts.SituationSnapshot{})
	reasons := eventData(recorder, "memory_degraded", "reason")
	if len(reasons) != 1 || !strings.Contains(reasons[0], "vector backend went away") {
		t.Fatal(reasons)
	}
	if mem.Mode().Label() != "lexical" || mem.Errors() != 0 || !strings.Contains(block, "c1 [01] failed") {
		t.Fatalf("%d %s", mem.Errors(), block)
	}
	block = mem.Recall(bg, "m1", "c3", item("02", "fix the config parser"), contracts.SituationSnapshot{})
	if mem.Mode().Label() != "lexical" || !strings.Contains(block, "config_parser.py") || !strings.Contains(block, "c1 [01] failed") {
		t.Fatal(block)
	}
}

type brokenEvents struct{ *persistence.SQLiteStore }

func (brokenEvents) ListEvents(context.Context, string, persistence.EventQuery) ([]persistence.EventRow, error) {
	return nil, errors.New("disk gone")
}
func (brokenEvents) AppendEvent(context.Context, string, string, string, map[string]any) (int64, error) {
	return 0, errors.New("disk gone")
}

func TestMemoryErrorsNeverFailACycle(t *testing.T) {
	mem := New(Options{Store: brokenEvents{openStore(t)}, Workdir: t.TempDir(), Config: DefaultConfig(),
		Mode: ops.DecideMemoryMode(nil), Embedder: NewHashEmbedder(0)})
	mem.ObserveCycle(bg, observation("c1", false, nil))
	block := mem.Recall(bg, "m1", "c2", item("01", "fix the config parser"), contracts.SituationSnapshot{})
	if mem.Errors() < 2 {
		t.Fatal(mem.Errors(), block)
	}
}

// --- Ollama ----------------------------------------------------------------------------------------

const digest = "sha256:0a109f422b47e3a30ba2b10eca18548e944e8a23073ee3f3e947efcf3c45e59f"

var axes = map[string]int{"port": 0, "socket": 0, "listen": 0, "config": 1, "settings": 1, "parser": 1, "database": 2, "sql": 2}

// semanticVector is a tiny "semantic" space: synonyms share an axis.
func semanticVector(text string) []float64 {
	vec := make([]float64, 8)
	for _, word := range strings.Fields(strings.NewReplacer(".", " ", "_", " ").Replace(strings.ToLower(text))) {
		if i, ok := axes[word]; ok {
			vec[i]++
		}
	}
	vec[7] = 0.01
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

// fakeOllama serves GET /api/tags + POST /api/embed like a local Ollama with pulled models.
type fakeOllama struct {
	mu          sync.Mutex
	models      []string
	down        bool
	embedStatus int
	embedCalls  [][]string
}

func newFakeOllama(models ...string) *fakeOllama {
	if len(models) == 0 {
		models = []string{"nomic-embed-text:latest"}
	}
	return &fakeOllama{models: models, embedStatus: 200}
}

func (f *fakeOllama) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("connection refused")
	}
	respond := func(status int, body any) (*http.Response, error) {
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}, Request: req}, nil
	}
	switch req.URL.Path {
	case "/api/tags":
		models := []map[string]string{}
		for _, m := range f.models {
			models = append(models, map[string]string{"name": m, "model": m, "digest": digest})
		}
		return respond(200, map[string]any{"models": models})
	case "/api/embed":
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		f.embedCalls = append(f.embedCalls, body.Input)
		if f.embedStatus != 200 {
			return respond(f.embedStatus, map[string]string{"error": "boom"})
		}
		vectors := [][]float64{}
		for _, text := range body.Input {
			vectors = append(vectors, semanticVector(text))
		}
		return respond(200, map[string]any{"model": body.Model, "embeddings": vectors})
	}
	return respond(404, map[string]string{})
}

func TestConnectProbesTheModelDigestAndDimension(t *testing.T) {
	fake := newFakeOllama()
	e, err := ConnectOllama(bg, OllamaOptions{Model: "nomic-embed-text", BaseURL: "http://ollama.test/", Transport: fake})
	if err != nil {
		t.Fatal(err)
	}
	if e.Name() != "ollama:nomic-embed-text" || e.Version() != "nomic-embed-text@"+digest[:12] || e.Dim() != 8 ||
		len(fake.embedCalls) != 1 || fake.embedCalls[0][0] != "dimension probe" {
		t.Fatal(e.Name(), e.Version(), e.Dim(), fake.embedCalls)
	}
	saved := OllamaBatch
	OllamaBatch = 2
	defer func() { OllamaBatch = saved }()
	vectors, err := e.Embed(bg, []string{"port", "socket", "sql"})
	if err != nil || len(fake.embedCalls) != 3 || len(fake.embedCalls[1]) != 2 || len(fake.embedCalls[2]) != 1 {
		t.Fatal(fake.embedCalls, err)
	}
	near, _ := Cosine(vectors[0], vectors[1])
	far, _ := Cosine(vectors[0], vectors[2])
	if near < 0.99 || far > 0.1 {
		t.Fatal(near, far)
	}
	_ = e.Close()
}

func TestConnectAcceptsAnExplicitTag(t *testing.T) {
	fake := newFakeOllama("mxbai-embed-large:335m")
	if _, err := ConnectOllama(bg, OllamaOptions{Model: "mxbai-embed-large:335m", Transport: fake}); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOllama(bg, OllamaOptions{Model: "mxbai-embed-large", Transport: fake}); err == nil || !strings.Contains(err.Error(), "not pulled") {
		t.Fatal(err)
	}
}

func TestConnectReportsWhyOllamaIsUnusable(t *testing.T) {
	for message, setup := range map[string]func(*fakeOllama){
		"unreachable":                  func(f *fakeOllama) { f.down = true },
		"ollama pull nomic-embed-text": func(f *fakeOllama) { f.models = []string{"llama3:8b"} },
		"could not embed":              func(f *fakeOllama) { f.embedStatus = 500 },
	} {
		fake := newFakeOllama()
		setup(fake)
		_, err := ConnectOllama(bg, OllamaOptions{Model: "nomic-embed-text", Transport: fake})
		var unavailable *OllamaUnavailableError
		if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v", message, err)
		}
	}
}

type shortOllama struct{}

func (shortOllama) RoundTrip(req *http.Request) (*http.Response, error) {
	body := `{"embeddings": [[]]}`
	if req.URL.Path == "/api/tags" {
		body = `{"models": [{"name": "e:latest", "digest": ""}]}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: req}, nil
}

func TestEmbedRejectsAShortAnswerAndAnEmptyProbeVector(t *testing.T) {
	if _, err := ConnectOllama(bg, OllamaOptions{Model: "e", Transport: shortOllama{}}); err == nil || !strings.Contains(err.Error(), "empty vector") {
		t.Fatal(err)
	}
	e := &OllamaEmbedder{model: "e", version: ollamaVersion("e", ""), dim: 3, base: "http://x", client: &http.Client{Transport: shortOllama{}}}
	if e.Version() != "e" {
		t.Fatal(e.Version())
	}
	if _, err := e.Embed(bg, []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "1 vectors for 2 texts") {
		t.Fatal(err)
	}
}

func TestEmbeddingModelDefaultsPerEmbedder(t *testing.T) {
	for _, c := range []struct {
		env  []string
		want string
	}{
		{[]string{"LHA_MEMORY_EMBEDDER=ollama"}, "nomic-embed-text"},
		{[]string{"LHA_MEMORY_EMBEDDER=sentence_transformers"}, "BAAI/bge-m3"},
		{[]string{"LHA_MEMORY_EMBEDDER=ollama", "LHA_MEMORY_EMBEDDING_MODEL= bge-m3 "}, "bge-m3"},
		{[]string{"LHA_MEMORY_EMBEDDER=hash"}, ""},
	} {
		if got := EmbeddingModel(settingsFrom(t, c.env...)); got != c.want {
			t.Errorf("%v: %q", c.env, got)
		}
	}
	if settingsFrom(t).MemoryEmbedder != "hash" {
		t.Fatal("default embedder")
	}
}

func TestPaddedEmbedderKeepsCosineAndRefusesToShrink(t *testing.T) {
	inner, err := ConnectOllama(bg, OllamaOptions{Model: "nomic-embed-text", Transport: newFakeOllama()})
	if err != nil {
		t.Fatal(err)
	}
	padded, _ := NewPaddedEmbedder(inner, 16)
	if padded.Name() != inner.Name() || padded.Version() != inner.Version() || padded.Dim() != 16 {
		t.Fatal(padded.Name(), padded.Version())
	}
	a := must(padded.Embed(bg, []string{"listen port", "socket settings"}))
	raw := must(inner.Embed(bg, []string{"listen port", "socket settings"}))
	pc, _ := Cosine(a[0], a[1])
	rc, _ := Cosine(raw[0], raw[1])
	if len(a[0]) != 16 || math.Abs(pc-rc) > 1e-12 {
		t.Fatal(pc, rc)
	}
	if _, err := NewPaddedEmbedder(inner, 4); err == nil || !strings.Contains(err.Error(), "cannot pad") {
		t.Fatal(err)
	}
	_ = padded.Close()
}

func ollamaRepo(t *testing.T) string {
	return repo(t, map[string]string{
		"net.py": "SOCKET = 8080  # the socket number\n",
		"db.py":  "DATABASE = 'sqlite'  # sql storage\n",
	})
}

func ollamaSettings(t *testing.T) *config.Settings {
	return settingsFrom(t, "LHA_MEMORY_EMBEDDER=ollama", "LHA_OLLAMA_BASE_URL=http://ollama.test:11434/")
}

func TestOllamaMemoryRecallsByMeaningAndGatesByModelVersion(t *testing.T) {
	fake := newFakeOllama()
	store := openStore(t)
	mem := OpenMissionMemory(bg, ollamaSettings(t), store, ollamaRepo(t), "m1", OpenOptions{EmbedderTransport: fake})
	if mem.Mode().Label() != "hybrid" || mem.Embedder().Name() != "ollama:nomic-embed-text" {
		t.Fatalf("%+v", mem.Mode())
	}
	block := mem.Recall(bg, "m1", "c1", item("01", "make the listen port configurable"), contracts.SituationSnapshot{})
	// "listen port" shares no word with net.py but means the same thing: the dense channel ranks it.
	if !strings.Contains(block, "net.py") || (strings.Contains(block, "db.py") && strings.Index(block, "db.py") < strings.Index(block, "net.py")) {
		t.Fatal(block)
	}
	mem.ObserveCycle(bg, CycleObservation{MissionID: "m1", CycleID: "c1", ItemID: "01", ItemDescription: "listen port",
		Verdict: "passed", Verified: true, Status: "done", Attempts: 1, DoneSummary: "moved the socket number to settings"})
	rows := must(store.MemoryVectors(bg, "m1", "ollama:nomic-embed-text", "nomic-embed-text@"+digest[:12], 0))
	if len(rows) == 0 || len(rows[0].Vector) != 8 {
		t.Fatal(rows)
	}
	if other := must(store.MemoryVectors(bg, "m1", "ollama:nomic-embed-text", "nomic-embed-text@x", 0)); len(other) != 0 {
		t.Fatal("a re-pulled model compared against old vectors")
	}
	_ = mem.Close()
}

func TestAbsentOllamaMeansLexicalMemoryNotAFailure(t *testing.T) {
	fake := newFakeOllama()
	fake.down = true
	recorder := obs.NewTraceRecorder(nil)
	mem := OpenMissionMemory(bg, ollamaSettings(t), openStore(t), ollamaRepo(t), "m1", OpenOptions{EmbedderTransport: fake, Recorder: recorder})
	if mem.Embedder() != nil || mem.Mode().Label() != "lexical" {
		t.Fatalf("%+v", mem.Mode())
	}
	if reasons := eventData(recorder, "memory_degraded", "reason"); len(reasons) != 1 || !strings.Contains(reasons[0], "ollama unreachable") {
		t.Fatal(reasons)
	}
}

func TestOllamaFailingMidRunDegradesToLexical(t *testing.T) {
	fake := newFakeOllama()
	recorder := obs.NewTraceRecorder(nil)
	mem := OpenMissionMemory(bg, ollamaSettings(t), openStore(t), ollamaRepo(t), "m1", OpenOptions{EmbedderTransport: fake, Recorder: recorder})
	if !mem.Mode().Dense {
		t.Fatal("not hybrid")
	}
	fake.mu.Lock()
	fake.embedStatus = 503
	fake.mu.Unlock()
	block := mem.Recall(bg, "m1", "c1", item("01", "the sql database"), contracts.SituationSnapshot{})
	if !strings.Contains(block, "db.py") || mem.Embedder() != nil || mem.Mode().Label() != "lexical" {
		t.Fatal(block)
	}
	if len(eventData(recorder, "memory_degraded", "reason")) == 0 {
		t.Fatal("no memory_degraded")
	}
}

func TestOllamaVectorsArePaddedForPgvector(t *testing.T) {
	mem := OpenMissionMemory(bg, ollamaSettings(t), fakePG{openStore(t)}, t.TempDir(), "m", OpenOptions{EmbedderTransport: newFakeOllama()})
	padded, isPadded := mem.Embedder().(*PaddedEmbedder)
	if mem.Mode().Label() != "hybrid" || !isPadded || padded.Dim() != 1024 || padded.Version() != "nomic-embed-text@"+digest[:12] {
		t.Fatalf("%+v", mem.Mode())
	}
	_ = mem.Close()
}

// Memory's git is the hardened harness git: an operator GIT_DIR does not redirect it to another
// repository, and a work tree whose config the harness refuses reads as no repository evidence.
func TestMemoryGitIsHardened(t *testing.T) {
	ws := repo(t, map[string]string{"parser.py": "def parse_config(): pass\n"})
	other := repo(t, map[string]string{"decoy.py": "parse_config = 'decoy'\n"})
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	chunks, err := RepoChunks(bg, ws, 10)
	if err != nil || len(chunks) != 1 || chunks[0].Metadata["path"] != "parser.py" {
		t.Fatalf("ls-files followed the operator's GIT_DIR: %+v %v", chunks, err)
	}
	hits, err := GitGrep(bg, ws, []string{"parse_config"})
	if err != nil || len(hits) != 1 || hits[0].Metadata["path"] != "parser.py" {
		t.Fatalf("git grep followed the operator's GIT_DIR: %+v %v", hits, err)
	}
	if none, err := GitGrep(bg, ws, []string{"no_such_term_anywhere"}); err != nil || len(none) != 0 {
		t.Fatalf("git grep exit 1 must be an empty result: %+v %v", none, err)
	}
	before, _ := state.HeadSHA(bg, ws)
	if err := os.WriteFile(filepath.Join(ws, "new.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := state.CommitAll(bg, ws, "add new.py")
	if err != nil {
		t.Fatal(err)
	}
	if files, err := changedFiles(bg, ws, before, after); err != nil || len(files) != 1 || files[0] != "new.py" {
		t.Fatalf("diff followed the operator's GIT_DIR: %v %v", files, err)
	}

	// A config include from the (agent-writable) work tree: every driver-capable command is
	// refused, i.e. exit 128.
	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_WORK_TREE")
	if err := os.WriteFile(filepath.Join(ws, "agent.cfg"), []byte("[user]\n\tname = agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "config", "include.path", "../agent.cfg")
	cmd.Dir = ws
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v %s", err, out)
	}
	if files, err := changedFiles(bg, ws, before, after); err != nil || len(files) != 0 {
		t.Fatalf("a refused diff must read as no changed files: %v %v", files, err)
	}
	if _, err := GitGrep(bg, ws, []string{"parse_config"}); err == nil || !strings.Contains(err.Error(), "refusing to run git") {
		t.Fatalf("a refused git grep must fail with the refusal, got %v", err)
	}
}

// --- re-embedding ----------------------------------------------------------------------------------

// versioned is an embedder under a new model version (e.g. after an `ollama pull`).
type versioned struct {
	contracts.Embedder
	version string
}

func (v versioned) Version() string { return v.version }

func facts(texts ...string) []contracts.MemoryRecord {
	out := []contracts.MemoryRecord{}
	for _, t := range texts {
		out = append(out, contracts.NewMemoryRecord(t, "fact", t, nil))
	}
	return out
}

func reembedEvents(recorder *obs.TraceRecorder) []string {
	out := []string{}
	for _, e := range recorder.Events() {
		if e.Kind == "memory_reembedded" {
			b, _ := json.Marshal(e.Data)
			out = append(out, e.MissionID+"@"+e.CycleID+" "+string(b))
		}
	}
	return out
}

func TestReembedRestoresDenseRecallAfterAnEmbedderChange(t *testing.T) {
	recorder := obs.NewTraceRecorder(nil)
	mem := newMemory(t, t.TempDir(), recorder, nil)
	if err := mem.storeRecords(bg, "m1", facts("config parser keys", "port defaults")); err != nil {
		t.Fatal(err)
	}
	if err := mem.Store.PutMemory(bg, "m2", facts("stored while degraded"), nil); err != nil {
		t.Fatal(err)
	}
	query := func() []contracts.RetrievalHit {
		index, err := mem.denseIndex("m1")
		if err != nil {
			t.Fatal(err)
		}
		hits, err := index.Query(bg, "config parser", 1)
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}
	if len(query()) != 1 {
		t.Fatal("no dense hit before the change")
	}
	mem.SetEmbedder(versioned{NewHashEmbedder(0), "2"})
	if hits := query(); len(hits) != 0 {
		t.Fatalf("another version's vectors were compared: %+v", hits)
	}
	done, err := mem.Reembed(bg, "", 0, "")
	if err != nil || !reflect.DeepEqual(done, map[string]int{"m1": 2, "m2": 1}) {
		t.Fatal(done, err)
	}
	if hits := query(); len(hits) != 1 || hits[0].Record.ID != "config parser keys" {
		t.Fatalf("%+v", hits)
	}
	if done, err := mem.Reembed(bg, "", 0, ""); err != nil || len(done) != 0 {
		t.Fatal(done, err)
	}
	want := []string{
		`m1@ {"count":2,"embedding_model":"hash","embedding_version":"2"}`,
		`m2@ {"count":1,"embedding_model":"hash","embedding_version":"2"}`,
	}
	if got := reembedEvents(recorder); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestReembedHonoursTheLimitAndTheMission(t *testing.T) {
	mem := newMemory(t, t.TempDir(), nil, nil)
	if err := mem.Store.PutMemory(bg, "m1", facts("a", "b", "c"), nil); err != nil {
		t.Fatal(err)
	}
	if err := mem.Store.PutMemory(bg, "m2", facts("d"), nil); err != nil {
		t.Fatal(err)
	}
	if done, err := mem.Reembed(bg, "m1", 2, ""); err != nil || !reflect.DeepEqual(done, map[string]int{"m1": 2}) {
		t.Fatal(done, err)
	}
	if done, err := mem.Reembed(bg, "m1", 0, ""); err != nil || !reflect.DeepEqual(done, map[string]int{"m1": 1}) {
		t.Fatal(done, err)
	}
	counts, err := mem.Store.CountStaleMemory(bg, "", "hash", "1")
	if err != nil || !reflect.DeepEqual(counts, map[string]int{"m2": 1}) {
		t.Fatal(counts, err)
	}
}

func TestRecallReEmbedsABoundedBatchEachCycle(t *testing.T) {
	defer func(n int) { ReembedPerRecall = n }(ReembedPerRecall)
	ReembedPerRecall = 2
	recorder := obs.NewTraceRecorder(nil)
	mem := newMemory(t, repo(t, map[string]string{"README.md": "x\n"}), recorder, nil)
	if err := mem.Store.PutMemory(bg, "m1", facts("parser one", "parser two", "parser three"), nil); err != nil {
		t.Fatal(err)
	}
	for _, cycle := range []string{"c1", "c2"} {
		mem.Recall(bg, "m1", cycle, item("01", "fix the config parser"), contracts.SituationSnapshot{})
	}
	want := []string{
		`m1@c1 {"count":2,"embedding_model":"hash","embedding_version":"1"}`,
		`m1@c2 {"count":1,"embedding_model":"hash","embedding_version":"1"}`,
	}
	if got := reembedEvents(recorder); !reflect.DeepEqual(got, want) || mem.Errors() != 0 {
		t.Fatal(got, mem.Errors())
	}
}

func TestReembedWithoutAnEmbedderDoesNothing(t *testing.T) {
	mem := newMemory(t, t.TempDir(), nil, nil)
	if err := mem.Store.PutMemory(bg, "m1", facts("a"), nil); err != nil {
		t.Fatal(err)
	}
	mem.SetEmbedder(nil)
	if done, err := mem.Reembed(bg, "", 0, ""); err != nil || len(done) != 0 {
		t.Fatal(done, err)
	}
}

type deafIndex struct{ contracts.SemanticIndex }

func (deafIndex) Add(context.Context, []contracts.MemoryRecord) error { return nil }

func TestReembedStopsWhenTheStoreDoesNotRestamp(t *testing.T) {
	defer func(n int) { reembedBatch = n }(reembedBatch)
	reembedBatch = 2
	mem := newMemory(t, t.TempDir(), nil, nil)
	if err := mem.Store.PutMemory(bg, "m1", facts("a", "b", "c"), nil); err != nil {
		t.Fatal(err)
	}
	mem.SetDenseIndex("m1", deafIndex{})
	if _, err := mem.Reembed(bg, "m1", 0, ""); err == nil || err.Error() != "re-embedded rows are still stale: a" {
		t.Fatal(err)
	}
}

// Ported from python/tests/unit/test_bounded_memory.py: the embedding cache keeps only what the
// latest Recall used, so edits replace cached chunks instead of adding to them.
func TestTheEmbeddingCacheKeepsOnlyWhatTheLatestRecallUsed(t *testing.T) {
	files := map[string][]string{}
	initial := map[string]string{}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("mod%d.py", i)
		for j := 0; j < 120; j++ {
			files[name] = append(files[name], fmt.Sprintf("def f%d_%d(): return %d\n", i, j, j))
		}
		initial[name] = strings.Join(files[name], "")
	}
	ws := repo(t, initial)
	mem := newMemory(t, ws, nil, nil)
	sizes := []int{}
	for cycle := 0; cycle < 6; cycle++ {
		name := fmt.Sprintf("mod%d.py", cycle%5)
		files[name] = append([]string{fmt.Sprintf("# edit %d\n", cycle)}, files[name]...)
		if err := os.WriteFile(filepath.Join(ws, name), []byte(strings.Join(files[name], "")), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := state.CommitAll(bg, ws, fmt.Sprintf("e%d", cycle)); err != nil {
			t.Fatal(err)
		}
		mem.Recall(bg, "m1", fmt.Sprintf("c%d", cycle), item("01", "edit the modules"), contracts.SituationSnapshot{})
		mem.mu.Lock()
		for key := range mem.vectors {
			if !mem.touched[key] {
				t.Fatalf("cycle %d: cached a vector the latest recall did not use", cycle)
			}
		}
		sizes = append(sizes, len(mem.vectors))
		mem.mu.Unlock()
	}
	if sizes[4] != sizes[5] { // each file grows from 3 chunks to 4 once; later edits replace chunks
		t.Fatal(sizes)
	}
}
