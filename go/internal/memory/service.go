package memory

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/ops"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// Tiered memory wired into the agent loop (python: lha.memory.service.MissionMemory).
//
// Per cycle, BEFORE the lead's first turn, Recall builds one bounded memory block for the prompt
// (RenderMemoryBlock) from three tiers:
//
//   - episodic: past attempts/outcomes of THIS item plus the mission's most recent other
//     outcomes, from episodic_events;
//   - procedural: verified skills relevant to this item;
//   - semantic: hybrid retrieval over distilled facts + progress notes (persisted), the anchor's
//     recent decisions, and chunks of the checkout's tracked text files. BM25 and the dense
//     channel (embedder cosine; pgvector on Postgres, stored vectors on SQLite) are fused with
//     RRF, then reranked (fusion order, or memory_rerank=system_one).
//
// AFTER the checkpoint, ObserveCycle appends the outcome as an episodic event, stores a progress
// note, admits a skill when the item was VERIFIED, and every ConsolidateEvery outcomes
// consolidates the new episodes into facts (extractive, or "model" with the metered model),
// soft-invalidating the progress notes they summarize.
//
// Degradation (ops.DecideMemoryMode): when Postgres, pgvector or the embedder is unavailable — at
// open or at any later call — the dense channel is dropped and retrieval runs on BM25 + git grep;
// a memory_degraded event says why. Memory never fails a cycle: errors are recorded
// (memory_error) and the cycle proceeds with less memory.

// Event kinds in episodic_events.
const (
	EpisodeKind       = "cycle_outcome"
	ConsolidationKind = "memory_consolidation"
)

// PGEmbeddingDim is the width of pgvector's semantic_memory.embedding column.
const PGEmbeddingDim = 1024

// OllamaTimeout is the HTTP timeout of the Ollama embedder.
const OllamaTimeout = 30 * time.Second

// SkillTTLDays is how long an admitted skill stays valid.
const SkillTTLDays = 90

const (
	chunkLines        = 40
	maxFileBytes      = 100_000
	gitTimeout        = 20 * time.Second
	grepTerms         = 6
	grepFiles         = 10
	grepLinesPerFile  = 5
	episodeScan       = 500
	memoryScan        = 500
	lineCap           = 700
	sectionEpisodic   = "Earlier attempts and outcomes"
	sectionSkills     = "Skills that worked before"
	sectionSemantic   = "Related facts, progress, decisions and code"
	extraEmbedderHelp = "sentence-transformers unavailable (not supported in the Go implementation); " +
		"install the `embeddings` extra and use the Python lha, or set LHA_MEMORY_EMBEDDER=ollama"
)

var (
	stopwords = func() map[string]bool {
		m := map[string]bool{}
		for _, w := range strings.Fields("the and for with that this from into when then than must should make add use are was " +
			"not all any can its has have will each item test tests file files") {
			m[w] = true
		}
		return m
	}()
	termRE = regexp.MustCompile(`[a-z0-9_]{3,}`)
	// Weights are the episodic, skills and semantic shares of the prompt budget.
	Weights = []float64{0.4, 0.2, 0.4}
)

// Config tunes MissionMemory (python: MemoryConfig).
type Config struct {
	PromptBudgetChars int
	EpisodicK         int
	SemanticK         int
	SkillsK           int
	ConsolidateEvery  int
	Consolidation     string // "extractive" | "model"
	IndexRepoFiles    bool
	MaxRepoFiles      int
}

// DefaultConfig is Python's MemoryConfig() defaults.
func DefaultConfig() Config {
	return Config{PromptBudgetChars: 4000, EpisodicK: 4, SemanticK: 4, SkillsK: 2, ConsolidateEvery: 5,
		Consolidation: "extractive", IndexRepoFiles: true, MaxRepoFiles: 400}
}

// ConfigFromSettings reads the LHA_MEMORY_* settings.
func ConfigFromSettings(s *config.Settings) Config {
	return Config{
		PromptBudgetChars: s.MemoryPromptBudgetChars, EpisodicK: s.MemoryEpisodicK, SemanticK: s.MemorySemanticK,
		SkillsK: s.MemorySkillsK, ConsolidateEvery: s.MemoryConsolidateEvery, Consolidation: s.MemoryConsolidation,
		IndexRepoFiles: s.MemoryIndexRepoFiles, MaxRepoFiles: s.MemoryMaxRepoFiles,
	}
}

// CycleObservation is what one committed cycle did (ObserveCycle's input).
type CycleObservation struct {
	MissionID       string
	CycleID         string
	ItemID          string
	ItemDescription string
	Verdict         string
	Verified        bool
	Status          string
	Attempts        int
	HeadSHA         string
	BeforeHead      string
	Failure         string
	DoneSummary     string
	Tools           []string
}

// CycleMemory is what the agent loop needs from a memory plane.
type CycleMemory interface {
	// Recall is the bounded memory block for this cycle's prompt ("" if nothing relevant).
	Recall(ctx context.Context, missionID, cycleID string, item contracts.ChecklistItem, snapshot contracts.SituationSnapshot) string
	// ObserveCycle records a committed cycle into every tier (never fails).
	ObserveCycle(ctx context.Context, o CycleObservation)
}

// vectorStore is the SQLite store's dense-channel read.
type vectorStore interface {
	MemoryVectors(ctx context.Context, missionID, model, version string, limit int) ([]persistence.StoredVector, error)
}

// SQLiteSemanticIndex is a SemanticIndex over the SQLite store: vectors persisted as JSON, exact
// cosine at query time, gated by embedder model+version.
type SQLiteSemanticIndex struct {
	store     persistence.Store
	embedder  contracts.Embedder
	missionID string
}

// Add embeds and stores records with their vectors.
func (x *SQLiteSemanticIndex) Add(ctx context.Context, records []contracts.MemoryRecord) error {
	if len(records) == 0 {
		return nil
	}
	texts := make([]string, len(records))
	for i, r := range records {
		texts[i] = r.Text
	}
	vectors, err := x.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	for _, v := range vectors {
		if len(v) != x.embedder.Dim() {
			return fmt.Errorf("embedder %s returned dim=%d; declared %d", contracts.PyRepr(x.embedder.Name()), len(v), x.embedder.Dim())
		}
	}
	return x.store.PutMemory(ctx, x.missionID, records, &persistence.Embedding{
		Vectors: vectors, Model: x.embedder.Name(), Version: x.embedder.Version()})
}

// Query is the k most similar stored records with a positive score.
func (x *SQLiteSemanticIndex) Query(ctx context.Context, text string, k int) ([]contracts.RetrievalHit, error) {
	vs, ok := x.store.(vectorStore)
	if !ok {
		return nil, errors.New("store has no stored vectors")
	}
	rows, err := vs.MemoryVectors(ctx, x.missionID, x.embedder.Name(), x.embedder.Version(), 0)
	if err != nil || len(rows) == 0 {
		return []contracts.RetrievalHit{}, err
	}
	qv, err := EmbedQueries(ctx, x.embedder, []string{text})
	if err != nil {
		return nil, err
	}
	hits := []contracts.RetrievalHit{}
	for _, row := range rows {
		score, err := Cosine(qv[0], row.Vector)
		if err != nil {
			return nil, err
		}
		hits = append(hits, contracts.RetrievalHit{Record: row.Record, Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	out := []contracts.RetrievalHit{}
	for _, h := range hits {
		if h.Score > 0 && len(out) < k {
			out = append(out, h)
		}
	}
	return out, nil
}

// --- helpers ---------------------------------------------------------------------------------------

// Terms are the query's distinct search terms (3+ chars, no stopwords), in order.
func Terms(text string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range termRE.FindAllString(strings.ToLower(text), -1) {
		if !stopwords[t] && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

type gitResult struct {
	code   int
	stdout []byte
	stderr []byte
}

// runGit is the hardened harness git (state.RunGitBytes); a refusal or timeout reads as exit
// code 128 (python: memory.service._git).
func runGit(ctx context.Context, workdir string, args ...string) (gitResult, error) {
	res, err := state.RunGitBytes(ctx, workdir, gitTimeout, args...)
	var gitErr *state.GitError
	switch {
	case errors.As(err, &gitErr):
		return gitResult{code: 128, stderr: []byte(gitErr.Error())}, nil
	case err != nil:
		return gitResult{}, err
	}
	return gitResult{code: res.ReturnCode, stdout: res.Stdout, stderr: res.Stderr}, nil
}

func changedFiles(ctx context.Context, workdir, before, after string) ([]string, error) {
	if before == "" || after == "" || before == after {
		return []string{}, nil
	}
	res, err := runGit(ctx, workdir, "diff", "--name-only", before, after)
	if err != nil {
		return nil, err
	}
	if res.code != 0 {
		return []string{}, nil
	}
	out := []string{}
	for _, n := range splitLines(decodeReplace(res.stdout)) {
		if n != "" && !strings.HasPrefix(n, ".lha/") {
			out = append(out, n)
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}

// RepoChunks is the tracked text files (git ls-files, excluding .lha/) split into line chunks.
func RepoChunks(ctx context.Context, workdir string, maxFiles int) ([]contracts.MemoryRecord, error) {
	res, err := runGit(ctx, workdir, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	records := []contracts.MemoryRecord{}
	if res.code != 0 {
		return records, nil
	}
	paths := []string{}
	for _, p := range strings.Split(decodeReplace(res.stdout), "\x00") {
		if p != "" && !strings.HasPrefix(p, ".lha/") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	if len(paths) > maxFiles {
		paths = paths[:max(0, maxFiles)]
	}
	for _, rel := range paths {
		path := filepath.Join(workdir, rel)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		lines := splitLines(decodeReplace(data))
		for start := 0; start < len(lines); start += chunkLines {
			end := min(len(lines), start+chunkLines)
			body := pyfmt.PyStrip(strings.Join(lines[start:end], "\n"))
			if body == "" {
				continue
			}
			records = append(records, contracts.NewMemoryRecord(
				fmt.Sprintf("file:%s:%d", rel, start+1), "file",
				fmt.Sprintf("%s:%d-%d\n%s", rel, start+1, end, body), map[string]string{"path": rel}))
		}
	}
	return records, nil
}

// GitGrep is `git grep` for the query terms: one record per matching file, most hits first.
func GitGrep(ctx context.Context, workdir string, terms []string) ([]contracts.MemoryRecord, error) {
	if len(terms) == 0 {
		return []contracts.MemoryRecord{}, nil
	}
	args := []string{"grep", "-n", "-I", "-i", "-F", "--no-color"}
	for _, t := range terms[:min(len(terms), grepTerms)] {
		args = append(args, "-e", t)
	}
	args = append(args, "--", ".", ":(exclude).lha")
	res, err := runGit(ctx, workdir, args...)
	if err != nil {
		return nil, err
	}
	if res.code != 0 && res.code != 1 {
		return nil, fmt.Errorf("git grep failed: %s", pyfmt.Head(decodeReplace(res.stderr), 200))
	}
	order := []string{}
	hits := map[string][]string{}
	for _, line := range splitLines(decodeReplace(res.stdout)) {
		path, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if _, seen := hits[path]; !seen {
			order = append(order, path)
		}
		hits[path] = append(hits[path], rest)
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if len(hits[a]) != len(hits[b]) {
			return len(hits[a]) > len(hits[b])
		}
		return a < b
	})
	if len(order) > grepFiles {
		order = order[:grepFiles]
	}
	out := []contracts.MemoryRecord{}
	for _, path := range order {
		lines := hits[path]
		shown := []string{}
		for _, ln := range lines[:min(len(lines), grepLinesPerFile)] {
			shown = append(shown, oneLine(ln, 160))
		}
		out = append(out, contracts.NewMemoryRecord("grep:"+path, "grep",
			fmt.Sprintf("%s (%d matching lines)\n%s", path, len(lines), strings.Join(shown, "\n")),
			map[string]string{"path": path}))
	}
	return out, nil
}

// RenderEpisode is one episodic line ("- c1 [01] failed (attempt 1, status in_progress); ...").
func RenderEpisode(payload map[string]any, cycleID string) string {
	verdict := "no verdict"
	if v, _ := get(payload, "verdict"); truthy(v) {
		verdict = pyStr(v)
	}
	parts := []string{fmt.Sprintf("%s [%s] %s (attempt %s, status %s)", cycleID, getStr(payload, "item_id", "?"),
		verdict, getStr(payload, "attempts", "?"), getStr(payload, "status", "?"))}
	if tools := list(payload, "tools"); len(tools) > 0 {
		parts = append(parts, "tools: "+strings.Join(dedupe(tools), ", "))
	}
	if files := list(payload, "files"); len(files) > 0 {
		parts = append(parts, "files: "+strings.Join(files[:min(len(files), 8)], ", "))
	}
	summary, _ := get(payload, "summary")
	if v, _ := get(payload, "verified"); truthy(v) {
		if truthy(summary) {
			parts = append(parts, "what worked: "+oneLine(pyStr(summary), 300))
		}
	} else {
		if truthy(summary) {
			parts = append(parts, "claimed: "+oneLine(pyStr(summary), 160))
		}
		if failure, _ := get(payload, "failure"); truthy(failure) {
			parts = append(parts, "failure: "+oneLine(pyStr(failure), 300))
		}
	}
	return "- " + strings.Join(parts, "; ")
}

// --- the service -----------------------------------------------------------------------------------

// MissionMemory is episodic + semantic + procedural memory for one run.
type MissionMemory struct {
	Store     persistence.Store
	Workdir   string
	Config    Config
	Reranker  Reranker
	Model     contracts.ModelProvider // for "model" consolidation (metered by the caller)
	Recorder  *obs.TraceRecorder
	Namespace string // the skill namespace (default: the resolved workdir)
	// Today is the date skills expire against (nil => the local date).
	Today func() time.Time

	mu            sync.Mutex
	mode          ops.MemoryMode
	embedder      contracts.Embedder
	embedderOwned contracts.Embedder
	errors        int
	dense         map[string]contracts.SemanticIndex
	vectors       map[string][]float64
}

var _ CycleMemory = (*MissionMemory)(nil)

// Options build a MissionMemory.
type Options struct {
	Store     persistence.Store
	Workdir   string
	Config    Config
	Mode      ops.MemoryMode
	Embedder  contracts.Embedder // dropped unless Mode.Dense
	Reranker  Reranker           // nil => NoopReranker
	Model     contracts.ModelProvider
	Recorder  *obs.TraceRecorder
	Namespace string
}

// New is a MissionMemory (see Options).
func New(o Options) *MissionMemory {
	workdir := config.ResolvePath(config.ExpandUser(o.Workdir))
	m := &MissionMemory{
		Store: o.Store, Workdir: workdir, Config: o.Config, Reranker: o.Reranker, Model: o.Model,
		Recorder: o.Recorder, Namespace: o.Namespace, mode: o.Mode, embedderOwned: o.Embedder,
		dense: map[string]contracts.SemanticIndex{}, vectors: map[string][]float64{},
	}
	if o.Mode.Dense {
		m.embedder = o.Embedder
	}
	if m.Reranker == nil {
		m.Reranker = NoopReranker{}
	}
	if m.Namespace == "" {
		m.Namespace = workdir
	}
	return m
}

var log = func() *slog.Logger { return slog.Default().With("logger", "lha.memory") }

// Mode is the current retrieval mode.
func (m *MissionMemory) Mode() ops.MemoryMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

// Embedder is the dense channel's embedder (nil when lexical-only).
func (m *MissionMemory) Embedder() contracts.Embedder {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.embedder
}

// SetEmbedder replaces the dense embedder (tests).
func (m *MissionMemory) SetEmbedder(e contracts.Embedder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embedder = e
}

// Errors is the number of memory errors recorded so far.
func (m *MissionMemory) Errors() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errors
}

func (m *MissionMemory) emit(kind, missionID, cycleID string, data ...obs.Field) {
	if m.Recorder != nil {
		m.Recorder.Record(kind, missionID, cycleID, data...)
		return
	}
	attrs := []any{"mission_id", missionID, "cycle_id", cycleID}
	for _, f := range data {
		attrs = append(attrs, f.Key, f.Value)
	}
	log().Info(kind, attrs...)
}

// errText is python's f"{type(exc).__name__}: {exc}" (pyfmt.ExcTypeName; an unsupported feature is
// NotImplementedError).
func errText(err error) string {
	var unsupported interface{ Unsupported() bool }
	if errors.As(err, &unsupported) {
		return "NotImplementedError: " + err.Error()
	}
	return pyfmt.ExcText(err)
}

func (m *MissionMemory) recordError(where, missionID, cycleID string, err error) {
	m.mu.Lock()
	m.errors++
	m.mu.Unlock()
	log().Warn("memory_error", "where", where, "error", errText(err))
	m.emit("memory_error", missionID, cycleID, obs.F("where", where), obs.F("error", errText(err)))
}

// degrade drops the dense channel for the rest of the run (lexical + git grep).
func (m *MissionMemory) degrade(missionID, cycleID, dep string, err error) {
	m.mu.Lock()
	m.mode = ops.DecideMemoryMode([]ops.DependencyStatus{{Name: dep, Health: ops.HealthDown, Detail: errText(err)}})
	m.embedder = nil
	m.dense = map[string]contracts.SemanticIndex{}
	reason := m.mode.Reason
	m.mu.Unlock()
	log().Warn("memory_degraded", "reason", reason)
	m.emit("memory_degraded", missionID, cycleID, obs.F("reason", reason))
}

func (m *MissionMemory) denseDep() string {
	if m.Store.Backend() == persistence.BackendPostgres {
		return "pgvector"
	}
	return "embeddings"
}

func (m *MissionMemory) denseIndex(missionID string) (contracts.SemanticIndex, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.embedder == nil {
		return nil, nil
	}
	if index, ok := m.dense[missionID]; ok {
		return index, nil
	}
	var index contracts.SemanticIndex
	if m.Store.Backend() == persistence.BackendPostgres {
		pg, ok := m.Store.(*persistence.PostgresStore)
		if !ok {
			return nil, errors.New("the Postgres dense channel needs a *persistence.PostgresStore")
		}
		pgIndex, err := NewPgSemanticIndex(pg.Pool, m.embedder, PGEmbeddingDim, missionID)
		if err != nil {
			return nil, err
		}
		index = pgIndex
	} else {
		index = &SQLiteSemanticIndex{store: m.Store, embedder: m.embedder, missionID: missionID}
	}
	m.dense[missionID] = index
	return index, nil
}

// embed is the cached vectors of texts: as search text (query) or as documents (an embedder such
// as Voyage embeds the two differently, so they are cached apart).
func (m *MissionMemory) embed(ctx context.Context, texts []string, query bool) ([][]float64, error) {
	prefix := ""
	if query {
		prefix = "q:"
	}
	m.mu.Lock()
	embedder := m.embedder
	keys := make([]string, len(texts))
	missing := []int{}
	for i, t := range texts {
		sum := sha1.Sum([]byte(t))
		keys[i] = prefix + hex.EncodeToString(sum[:])
		if _, ok := m.vectors[keys[i]]; !ok {
			missing = append(missing, i)
		}
	}
	m.mu.Unlock()
	if embedder == nil {
		return nil, errors.New("no embedder")
	}
	if len(missing) > 0 {
		batch := make([]string, len(missing))
		for j, i := range missing {
			batch[j] = texts[i]
		}
		var vectors [][]float64
		var err error
		if query {
			vectors, err = EmbedQueries(ctx, embedder, batch)
		} else {
			vectors, err = embedder.Embed(ctx, batch)
		}
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(batch) {
			return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(vectors), len(batch))
		}
		m.mu.Lock()
		for j, i := range missing {
			m.vectors[keys[i]] = vectors[j]
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]float64, len(texts))
	for i, k := range keys {
		out[i] = m.vectors[k]
	}
	return out, nil
}

func (m *MissionMemory) denseRank(ctx context.Context, query string, records []contracts.MemoryRecord) ([]string, error) {
	if m.Embedder() == nil || len(records) == 0 {
		return []string{}, nil
	}
	queryVec, err := m.embed(ctx, []string{query}, true)
	if err != nil {
		return nil, err
	}
	texts := []string{}
	for _, r := range records {
		texts = append(texts, r.Text)
	}
	vectors, err := m.embed(ctx, texts, false)
	if err != nil {
		return nil, err
	}
	scored := make([]Scored, len(records))
	for i, r := range records {
		s, err := Cosine(queryVec[0], vectors[i])
		if err != nil {
			return nil, err
		}
		scored[i] = Scored{r.ID, s}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	out := []string{}
	for _, s := range scored {
		if s.Score > 0 {
			out = append(out, s.ID)
		}
	}
	return out, nil
}

func (m *MissionMemory) storeRecords(ctx context.Context, missionID string, records []contracts.MemoryRecord) error {
	index, err := m.denseIndex(missionID)
	if err != nil {
		m.degrade(missionID, "", m.denseDep(), err)
	} else if index != nil {
		if err := index.Add(ctx, records); err == nil {
			return nil
		} else {
			m.degrade(missionID, "", m.denseDep(), err)
		}
	}
	return m.Store.PutMemory(ctx, missionID, records, nil)
}

// --- recall ----------------------------------------------------------------------------------------

// Recall is the bounded memory block for this cycle's prompt ("" if nothing relevant).
func (m *MissionMemory) Recall(ctx context.Context, missionID, cycleID string, item contracts.ChecklistItem, snapshot contracts.SituationSnapshot) string {
	type fetch struct {
		title string
		fn    func() ([]string, error)
	}
	fetches := []fetch{
		{sectionEpisodic, func() ([]string, error) { return m.episodicLines(ctx, missionID, item) }},
		{sectionSkills, func() ([]string, error) { return m.skillLines(ctx, missionID, item) }},
		{sectionSemantic, func() ([]string, error) { return m.semanticLines(ctx, missionID, cycleID, item, snapshot) }},
	}
	sections := []MemorySection{}
	counts := []int{}
	for _, f := range fetches {
		lines, err := f.fn()
		if err != nil {
			m.recordError("recall:"+f.title, missionID, cycleID, err)
			lines = nil
		}
		sections = append(sections, MemorySection{Title: f.title, Lines: lines})
		counts = append(counts, len(lines))
	}
	block, err := RenderMemoryBlock(sections, m.Config.PromptBudgetChars, Weights)
	if err != nil {
		m.recordError("recall:render", missionID, cycleID, err)
		return ""
	}
	log().Debug("memory_recall", "mission_id", missionID, "cycle_id", cycleID, "mode", m.Mode().Label(),
		"chars", pyfmt.RuneLen(block), "counts", counts)
	return block
}

func (m *MissionMemory) episodicLines(ctx context.Context, missionID string, item contracts.ChecklistItem) ([]string, error) {
	k := m.Config.EpisodicK
	if k <= 0 {
		return nil, nil
	}
	events, err := m.Store.ListEvents(ctx, missionID, persistence.EventQuery{Kinds: []string{EpisodeKind}, Limit: episodeScan})
	if err != nil {
		return nil, err
	}
	same, others := []persistence.EventRow{}, []persistence.EventRow{}
	for _, e := range events {
		if id, ok := e.Payload["item_id"].(string); ok && id == item.ID {
			same = append(same, e)
		} else {
			others = append(others, e)
		}
	}
	same = lastN(same, k)
	others = lastN(others, max(1, k/2))
	// This item's history first (newest last), then the mission's latest other outcomes.
	out := []string{}
	for _, e := range append(same, others...) {
		out = append(out, RenderEpisode(e.Payload, e.CycleID))
	}
	return out, nil
}

func lastN[T any](items []T, n int) []T {
	if len(items) > n {
		return items[len(items)-n:]
	}
	return items
}

func (m *MissionMemory) skillLines(ctx context.Context, missionID string, item contracts.ChecklistItem) ([]string, error) {
	k := m.Config.SkillsK
	if k <= 0 {
		return nil, nil
	}
	skills, err := m.FindSkills(ctx, item.Description, k, missionID)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, s := range skills {
		out = append(out, fmt.Sprintf("- %s: %s\n  what worked: %s", s.Name, oneLine(s.Description, 240), oneLine(s.Code, 400)))
	}
	return out, nil
}

func (m *MissionMemory) today() string {
	now := time.Now()
	if m.Today != nil {
		now = m.Today()
	}
	return now.Format("2006-01-02")
}

// FindSkills is the verified, unexpired skills in this namespace (or global), most relevant first.
func (m *MissionMemory) FindSkills(ctx context.Context, query string, k int, missionID string) ([]contracts.Skill, error) {
	today := m.today()
	all, err := m.Store.ListSkills(ctx, m.Namespace, 0)
	if err != nil {
		return nil, err
	}
	skills := []contracts.Skill{}
	for _, s := range all {
		if s.Verified && (s.ExpiresAt == "" || s.ExpiresAt >= today) {
			skills = append(skills, s)
		}
	}
	if len(skills) == 0 {
		return []contracts.Skill{}, nil
	}
	records := make([]contracts.MemoryRecord, len(skills))
	byID := map[string]contracts.Skill{}
	for i, s := range skills {
		records[i] = contracts.NewMemoryRecord(s.ID, "procedural", s.Name+"\n"+s.Description, nil)
		byID[s.ID] = s
	}
	bm25 := NewBM25Index()
	bm25.Add(records)
	lexical := []string{}
	for _, s := range bm25.Query(query, len(records)) {
		lexical = append(lexical, s.ID)
	}
	rankings := [][]string{lexical}
	if m.Mode().Dense {
		if dense, err := m.denseRank(ctx, query, records); err != nil {
			m.degrade(missionID, "", m.denseDep(), err)
		} else {
			rankings = append(rankings, dense)
		}
	}
	out := []contracts.Skill{}
	for _, f := range ReciprocalRankFusion(nonEmpty(rankings), 60) {
		if s, ok := byID[f.ID]; ok && len(out) < k {
			out = append(out, s)
		}
	}
	return out, nil
}

func nonEmpty(rankings [][]string) [][]string {
	out := [][]string{}
	for _, r := range rankings {
		if len(r) > 0 {
			out = append(out, r)
		}
	}
	return out
}

func (m *MissionMemory) semanticLines(ctx context.Context, missionID, cycleID string, item contracts.ChecklistItem, snapshot contracts.SituationSnapshot) ([]string, error) {
	k := m.Config.SemanticK
	if k <= 0 {
		return nil, nil
	}
	query := item.Description
	stored, err := m.Store.ListMemory(ctx, missionID, memoryScan)
	if err != nil {
		return nil, err
	}
	local := []contracts.MemoryRecord{}
	for i, d := range snapshot.LastDecisions {
		local = append(local, contracts.NewMemoryRecord(fmt.Sprintf("decision:%d", i), "decision",
			fmt.Sprintf("%s (why: %s)", d.Decision, d.Rationale), nil))
	}
	if m.Config.IndexRepoFiles {
		chunks, err := RepoChunks(ctx, m.Workdir, m.Config.MaxRepoFiles)
		if err != nil {
			return nil, err
		}
		local = append(local, chunks...)
	}
	grep := []contracts.MemoryRecord{}
	if m.Mode().GitGrep {
		if grep, err = GitGrep(ctx, m.Workdir, Terms(query)); err != nil {
			return nil, err
		}
	}
	// This item's own progress notes are already in the episodic section.
	kept := []contracts.MemoryRecord{}
	for _, r := range stored {
		if !(r.Kind == "progress" && r.Metadata["item_id"] == item.ID) {
			kept = append(kept, r)
		}
	}
	stored = kept
	order := []string{}
	candidates := map[string]contracts.MemoryRecord{}
	add := func(records []contracts.MemoryRecord) {
		for _, r := range records {
			if _, ok := candidates[r.ID]; !ok {
				order = append(order, r.ID)
			}
			candidates[r.ID] = r
		}
	}
	add(stored)
	add(local)
	add(grep)
	if len(order) == 0 {
		return nil, nil
	}
	depth := max(3*k, 12)
	bm25 := NewBM25Index()
	all := make([]contracts.MemoryRecord, len(order))
	for i, id := range order {
		all[i] = candidates[id]
	}
	bm25.Add(all)
	lexical := []string{}
	for _, s := range bm25.Query(query, depth) {
		lexical = append(lexical, s.ID)
	}
	rankings := [][]string{lexical}
	if len(grep) > 0 {
		rankings = append(rankings, ids(grep))
	}
	if m.Mode().Dense {
		if dense, err := m.denseRankings(ctx, missionID, query, local, stored, candidates, depth); err != nil {
			m.degrade(missionID, cycleID, m.denseDep(), err)
		} else {
			rankings = append(rankings, dense...)
		}
	}
	if m.Mode().GitGrep && len(grep) == 0 { // degraded just now: add the git-grep channel
		if grep, err = GitGrep(ctx, m.Workdir, Terms(query)); err != nil {
			return nil, err
		}
		add(grep)
		if len(grep) > 0 {
			rankings = append(rankings, ids(grep))
		}
	}
	fused := ReciprocalRankFusion(nonEmpty(rankings), 60)
	if len(fused) > depth {
		fused = fused[:depth]
	}
	hits := make([]contracts.RetrievalHit, len(fused))
	for i, f := range fused {
		hits[i] = contracts.RetrievalHit{Record: candidates[f.ID], Score: f.Score}
	}
	hits, err = m.Reranker.Rerank(ctx, query, hits, k)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, h := range hits {
		out = append(out, fmt.Sprintf("- (%s) %s", h.Record.Kind, pyfmt.Head(h.Record.Text, lineCap)))
	}
	return out, nil
}

func (m *MissionMemory) denseRankings(ctx context.Context, missionID, query string, local, stored []contracts.MemoryRecord, candidates map[string]contracts.MemoryRecord, depth int) ([][]string, error) {
	ranked, err := m.denseRank(ctx, query, local)
	if err != nil {
		return nil, err
	}
	dense := [][]string{ranked[:min(len(ranked), depth)]}
	index, err := m.denseIndex(missionID)
	if err != nil {
		return nil, err
	}
	if index != nil && len(stored) > 0 {
		found, err := index.Query(ctx, query, depth)
		if err != nil {
			return nil, err
		}
		kept := []string{}
		for _, h := range found {
			if _, ok := candidates[h.Record.ID]; ok {
				kept = append(kept, h.Record.ID)
			}
		}
		dense = append(dense, kept)
	}
	return dense, nil
}

func ids(records []contracts.MemoryRecord) []string {
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.ID
	}
	return out
}

// --- observe ---------------------------------------------------------------------------------------

// ObserveCycle records a committed cycle into every tier (never fails: errors are recorded).
func (m *MissionMemory) ObserveCycle(ctx context.Context, o CycleObservation) {
	if err := m.observe(ctx, o); err != nil {
		m.recordError("observe", o.MissionID, o.CycleID, err)
	}
	if _, err := m.MaybeConsolidate(ctx, o.MissionID, o.CycleID); err != nil {
		m.recordError("consolidate", o.MissionID, o.CycleID, err)
	}
}

func (m *MissionMemory) observe(ctx context.Context, o CycleObservation) error {
	files, err := changedFiles(ctx, m.Workdir, o.BeforeHead, o.HeadSHA)
	if err != nil {
		return err
	}
	tools := o.Tools
	if tools == nil {
		tools = []string{}
	}
	payload := map[string]any{
		"item_id":     o.ItemID,
		"description": o.ItemDescription,
		"verdict":     o.Verdict,
		"verified":    o.Verified,
		"status":      o.Status,
		"attempts":    o.Attempts,
		"tools":       tools[:min(len(tools), 30)],
		"files":       files,
		"summary":     pyfmt.Head(o.DoneSummary, 1000),
		"failure":     pyfmt.Tail(o.Failure, 1500),
		"head_sha":    o.HeadSHA,
	}
	if _, err := m.Store.AppendEvent(ctx, o.MissionID, o.CycleID, EpisodeKind, payload); err != nil {
		return err
	}
	note := strings.TrimPrefix(RenderEpisode(payload, o.CycleID), "- ")
	if err := m.storeRecords(ctx, o.MissionID, []contracts.MemoryRecord{contracts.NewMemoryRecord(
		fmt.Sprintf("progress:%s:%s:%s", o.MissionID, o.CycleID, o.ItemID), "progress",
		o.ItemDescription+": "+note, map[string]string{"item_id": o.ItemID, "cycle_id": o.CycleID})}); err != nil {
		return err
	}
	if o.Verified {
		if _, err := m.StoreSkill(ctx, o, files); err != nil {
			return err
		}
	}
	return nil
}

// StoreSkill admits what worked on a VERIFIED item as a reusable skill (namespaced to the repo).
func (m *MissionMemory) StoreSkill(ctx context.Context, o CycleObservation, files []string) (contracts.Skill, error) {
	sum := sha1.Sum([]byte(m.Namespace + "\x1f" + o.ItemDescription))
	digest := hex.EncodeToString(sum[:])
	tools := strings.Join(dedupe(o.Tools), ", ")
	if tools == "" {
		tools = "(none)"
	}
	summary := o.DoneSummary
	if summary == "" {
		summary = "(none)"
	}
	fileList := strings.Join(files, ", ")
	if fileList == "" {
		fileList = "(none)"
	}
	now := time.Now()
	if m.Today != nil {
		now = m.Today()
	}
	skill := contracts.Skill{
		ID:            "skill_" + digest[:20],
		Name:          oneLine(o.ItemDescription, 80),
		Description:   o.ItemDescription,
		Code:          fmt.Sprintf("summary: %s\ntools: %s\nfiles: %s", summary, tools, fileList),
		Preconditions: []string{},
		Namespace:     m.Namespace,
		Provenance:    fmt.Sprintf("%s:%s:%s", o.MissionID, o.CycleID, o.HeadSHA),
		ExpiresAt:     now.AddDate(0, 0, SkillTTLDays).Format("2006-01-02"),
		Verified:      true,
	}
	if err := m.Store.PutSkill(ctx, skill); err != nil {
		return skill, err
	}
	m.emit("skill_stored", o.MissionID, o.CycleID, obs.F("skill_id", skill.ID))
	return skill, nil
}

// --- consolidation ---------------------------------------------------------------------------------

func payloadInt(payload map[string]any, key string) int64 {
	switch v := payload[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case interface{ Int64() (int64, error) }:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// MaybeConsolidate consolidates when ConsolidateEvery new episodes exist; returns facts stored.
func (m *MissionMemory) MaybeConsolidate(ctx context.Context, missionID, cycleID string) (int, error) {
	every := m.Config.ConsolidateEvery
	if every <= 0 {
		return 0, nil
	}
	marks, err := m.Store.ListEvents(ctx, missionID, persistence.EventQuery{Kinds: []string{ConsolidationKind}, Limit: 1})
	if err != nil {
		return 0, err
	}
	var watermark int64
	if len(marks) > 0 {
		watermark = payloadInt(marks[len(marks)-1].Payload, "upto_id")
	}
	episodes, err := m.Store.ListEvents(ctx, missionID, persistence.EventQuery{Kinds: []string{EpisodeKind}, AfterID: watermark, Limit: episodeScan})
	if err != nil {
		return 0, err
	}
	if len(episodes) < every {
		return 0, nil
	}
	return m.ConsolidateNow(ctx, missionID, cycleID, episodes)
}

// ConsolidateNow turns episodes into facts (see the package doc) and records a watermark.
func (m *MissionMemory) ConsolidateNow(ctx context.Context, missionID, cycleID string, episodes []persistence.EventRow) (int, error) {
	var upto int64
	for _, e := range episodes {
		upto = max(upto, e.ID)
	}
	failedBatches := 0
	var facts []contracts.MemoryRecord
	mode := "extractive"
	if m.Config.Consolidation == "model" && m.Model != nil {
		lines := make([]string, len(episodes))
		for i, e := range episodes {
			lines[i] = strings.TrimPrefix(RenderEpisode(e.Payload, e.CycleID), "- ")
		}
		result, err := Consolidate(ctx, m.Model, lines, missionID, DefaultBatchSize)
		if err != nil {
			return 0, err
		}
		failedBatches = result.FailedBatches
		mode = "model"
		for i, f := range result.Facts {
			facts = append(facts, contracts.NewMemoryRecord(fmt.Sprintf("fact:%s:%d:%d", missionID, upto, i), "fact",
				f.Text, map[string]string{"source": "consolidation:model"}))
		}
	} else {
		// One fact per item, rebuilt from ALL of that item's episodes so far and upserted under
		// a stable id: a newer consolidation replaces the older fact for that item.
		items := map[string]bool{}
		for _, e := range episodes {
			items[getStr(e.Payload, "item_id", "?")] = true
		}
		history, err := m.Store.ListEvents(ctx, missionID, persistence.EventQuery{Kinds: []string{EpisodeKind}, Limit: episodeScan})
		if err != nil {
			return 0, err
		}
		relevant := []persistence.EventRow{}
		for _, e := range history {
			if e.ID <= upto && items[getStr(e.Payload, "item_id", "None")] {
				relevant = append(relevant, e)
			}
		}
		for _, f := range extractiveFacts(relevant) {
			facts = append(facts, contracts.NewMemoryRecord(fmt.Sprintf("fact:%s:item:%s", missionID, f[0]), "fact",
				f[1], map[string]string{"source": "consolidation:extractive", "item_id": f[0]}))
		}
	}
	if len(facts) > 0 {
		if err := m.storeRecords(ctx, missionID, facts); err != nil {
			return 0, err
		}
	}
	// The progress notes these facts summarize no longer need to compete for retrieval.
	progress := []string{}
	for _, e := range episodes {
		if v, _ := get(e.Payload, "item_id"); truthy(v) {
			progress = append(progress, fmt.Sprintf("progress:%s:%s:%s", missionID, e.CycleID, pyStr(v)))
		}
	}
	if _, err := m.Store.InvalidateMemory(ctx, progress); err != nil {
		return 0, err
	}
	if _, err := m.Store.AppendEvent(ctx, missionID, cycleID, ConsolidationKind, map[string]any{
		"upto_id": upto, "episodes": len(episodes), "facts": len(facts), "mode": mode,
		"failed_batches": failedBatches, "at": persistence.ISOAuto(time.Now()),
	}); err != nil {
		return 0, err
	}
	m.emit("memory_consolidated", missionID, cycleID, obs.F("episodes", len(episodes)),
		obs.F("facts", len(facts)), obs.F("mode", mode))
	return len(facts), nil
}

// extractiveFacts is deterministic consolidation: (item_id, fact) per item (no model).
func extractiveFacts(episodes []persistence.EventRow) [][2]string {
	order := []string{}
	byItem := map[string][]persistence.EventRow{}
	for _, e := range episodes {
		id := getStr(e.Payload, "item_id", "?")
		if _, ok := byItem[id]; !ok {
			order = append(order, id)
		}
		byItem[id] = append(byItem[id], e)
	}
	out := [][2]string{}
	for _, itemID := range order {
		group := byItem[itemID]
		last := group[len(group)-1].Payload
		desc := oneLine(getStr(last, "description", ""), 160)
		var wins, fails []persistence.EventRow
		fileSet := map[string]bool{}
		for _, e := range group {
			if v, _ := get(e.Payload, "verified"); truthy(v) {
				wins = append(wins, e)
			} else {
				fails = append(fails, e)
			}
			for _, f := range list(e.Payload, "files") {
				fileSet[f] = true
			}
		}
		files := make([]string, 0, len(fileSet))
		for f := range fileSet {
			files = append(files, f)
		}
		sort.Strings(files)
		files = files[:min(len(files), 8)]
		var fact string
		if len(wins) > 0 {
			win := wins[len(wins)-1]
			fact = fmt.Sprintf("[%s] %s: verified in %s after %s attempt(s)", itemID, desc, win.CycleID,
				getStr(win.Payload, "attempts", strconv.Itoa(len(group))))
			if s, _ := get(win.Payload, "summary"); truthy(s) {
				fact += "; what worked: " + oneLine(pyStr(s), 240)
			}
		} else {
			fact = fmt.Sprintf("[%s] %s: %d failed attempt(s) so far", itemID, desc, len(fails))
		}
		if len(fails) > 0 {
			if failure := oneLine(getStr(fails[len(fails)-1].Payload, "failure", ""), 200); failure != "" {
				fact += "; last failure: " + failure
			}
		}
		if len(files) > 0 {
			fact += "; files: " + strings.Join(files, ", ")
		}
		out = append(out, [2]string{itemID, fact})
	}
	return out
}

// Close closes the dense indexes and the embedder it owns (an Ollama HTTP client).
func (m *MissionMemory) Close() error {
	m.mu.Lock()
	m.dense = map[string]contracts.SemanticIndex{}
	owned := m.embedderOwned
	m.embedderOwned = nil
	m.mu.Unlock()
	if c, ok := owned.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// --- construction ----------------------------------------------------------------------------------

// DefaultEmbeddingModels is LHA_MEMORY_EMBEDDING_MODEL when empty, per embedder.
var DefaultEmbeddingModels = map[string]string{"ollama": "nomic-embed-text", "voyage": VoyageDefaultModel,
	"sentence_transformers": "BAAI/bge-m3"}

// EmbeddingModel is the configured embedding model, or the embedder's default.
func EmbeddingModel(s *config.Settings) string {
	if configured := strings.TrimSpace(s.MemoryEmbeddingModel); configured != "" {
		return configured
	}
	return DefaultEmbeddingModels[s.MemoryEmbedder]
}

func buildEmbedder(ctx context.Context, s *config.Settings, backend string, transport OpenOptions) (contracts.Embedder, ops.DependencyStatus) {
	switch s.MemoryEmbedder {
	case "none":
		return nil, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthDegraded, Detail: "disabled (LHA_MEMORY_EMBEDDER=none)"}
	case "ollama":
		emb, err := ConnectOllama(ctx, OllamaOptions{Model: EmbeddingModel(s), BaseURL: s.OllamaBaseURL,
			Timeout: OllamaTimeout, Transport: transport.EmbedderTransport})
		if err != nil {
			return nil, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthDown, Detail: err.Error()}
		}
		return emb, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthOK}
	case "voyage":
		key, endpoint := "", ""
		if s.VoyageAPIKey != nil {
			key = s.VoyageAPIKey.Value()
		}
		if s.VoyageEndpoint != nil {
			endpoint = *s.VoyageEndpoint
		}
		emb, err := ConnectVoyage(ctx, VoyageOptions{APIKey: key, Model: EmbeddingModel(s), Endpoint: endpoint,
			Timeout: VoyageTimeout, Resolver: transport.EmbedderResolver, Transport: transport.EmbedderTransport})
		if err != nil {
			return nil, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthDown, Detail: err.Error()}
		}
		return emb, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthOK}
	case "sentence_transformers":
		newST := NewSentenceTransformerEmbedder
		if transport.SentenceTransformer != nil {
			newST = transport.SentenceTransformer
		}
		emb, err := newST(EmbeddingModel(s))
		if err != nil {
			detail := extraEmbedderHelp
			if !errors.Is(err, ErrSentenceTransformersUnsupported) {
				detail = "sentence-transformers unavailable (" + errText(err) + ")"
			}
			return nil, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthDown, Detail: detail}
		}
		return emb, ops.DependencyStatus{Name: "embeddings", Health: ops.HealthOK}
	}
	dim := 256
	if backend == persistence.BackendPostgres {
		dim = PGEmbeddingDim
	}
	return NewHashEmbedder(dim), ops.DependencyStatus{Name: "embeddings", Health: ops.HealthOK}
}

// OpenOptions are OpenMissionMemory's optional inputs.
type OpenOptions struct {
	Model    contracts.ModelProvider // the metered librarian model ("model" consolidation)
	Recorder *obs.TraceRecorder
	// EmbedderTransport is a test seam for the Ollama / Voyage embedder's HTTP client.
	EmbedderTransport http.RoundTripper
	// EmbedderResolver is a test seam for the Voyage endpoint's DNS resolution (egress check).
	EmbedderResolver safety.Resolver
	// SentenceTransformer is a test seam standing in for the (Python-only) sentence_transformers
	// embedder.
	SentenceTransformer func(model string) (contracts.Embedder, error)
	// SystemOne serves memory_rerank=system_one (nil: fusion order is kept).
	SystemOne systemone.Model
}

// OpenMissionMemory is the run's memory plane per settings (nil when memory_enabled is false).
func OpenMissionMemory(ctx context.Context, s *config.Settings, store persistence.Store, workdir, missionID string, o OpenOptions) *MissionMemory {
	if !s.MemoryEnabled {
		return nil
	}
	statuses := []ops.DependencyStatus{}
	if reason := store.DegradedReason(); reason != "" {
		statuses = append(statuses, ops.DependencyStatus{Name: "postgres", Health: ops.HealthDown, Detail: reason})
	}
	embedder, status := buildEmbedder(ctx, s, store.Backend(), o)
	statuses = append(statuses, status)
	if store.Backend() == persistence.BackendPostgres && embedder != nil {
		if embedder.Dim() < PGEmbeddingDim {
			// e.g. nomic-embed-text (768), or a 512-wide Voyage model: zero-padding keeps cosine
			// similarity unchanged. Wider (a 2048-wide Voyage output) runs lexical-only.
			embedder, _ = NewPaddedEmbedder(embedder, PGEmbeddingDim)
		}
		if embedder.Dim() != PGEmbeddingDim {
			statuses = append(statuses, ops.DependencyStatus{Name: "pgvector", Health: ops.HealthDown,
				Detail: fmt.Sprintf("embedder dim %d > vector(%d) column", embedder.Dim(), PGEmbeddingDim)})
		} else {
			// Go sends vectors as pgvector text literals: no client adapter to be missing.
			statuses = append(statuses, ops.DependencyStatus{Name: "pgvector", Health: ops.HealthOK})
		}
	}
	mode := ops.DecideMemoryMode(statuses)
	var reranker Reranker = NoopReranker{}
	switch s.MemoryRerank {
	case "cross_encoder":
		if r, err := NewCrossEncoderReranker(""); err != nil {
			log().Warn("memory_rerank_unavailable", "error", errText(err))
		} else {
			reranker = r
		}
	case "system_one":
		if o.SystemOne == nil {
			log().Warn("memory_rerank_unavailable", "error", "memory_rerank=system_one needs LHA_SYSTEM_ONE_BACKEND")
		} else {
			reranker = &SystemOneReranker{Model: o.SystemOne, MinP: s.SystemOneRerankMin}
		}
	}
	memory := New(Options{Store: store, Workdir: workdir, Config: ConfigFromSettings(s), Mode: mode,
		Embedder: embedder, Reranker: reranker, Model: o.Model, Recorder: o.Recorder})
	if !mode.Dense {
		log().Warn("memory_degraded", "reason", mode.Reason)
		memory.emit("memory_degraded", missionID, "", obs.F("reason", mode.Reason))
	}
	return memory
}
