// Package ops is dependency-degradation -> safe-park decisions (python: lha.ops.degradation).
//
// When a dependency fails, the agent shouldn't blindly retry forever. Critical deps (git, the
// model, the sandbox) being DOWN means no verified progress is possible -> park. Optional deps
// (Postgres, pgvector, embeddings, Langfuse, egress proxy) DOWN means degrade gracefully and keep
// going.
//
// Memory degradation (DecideMemoryMode, used by the memory service): the dense channel of hybrid
// retrieval needs its embedder and, on Postgres, pgvector. If any of postgres / pgvector /
// embeddings is not OK, retrieval drops to LEXICAL only — BM25 over the stored memory + repo
// chunks, plus `git grep` over the checkout — and the mission continues.
package ops

import (
	"fmt"
	"sort"
	"strings"
)

// Health of a dependency.
type Health string

// Health values.
const (
	OK       Health = "ok"
	Degraded Health = "degraded"
	Down     Health = "down"
)

var (
	critical        = map[string]bool{"git": true, "model": true, "sandbox": true}
	optional        = map[string]bool{"postgres": true, "pgvector": true, "embeddings": true, "langfuse": true, "egress_proxy": true}
	memoryDenseDeps = map[string]bool{"postgres": true, "pgvector": true, "embeddings": true}
)

// DependencyStatus is one dependency's health.
type DependencyStatus struct {
	Name   string
	Health Health
	Detail string
}

// SafeParkDecision says whether to park.
type SafeParkDecision struct {
	Park     bool
	Reason   string
	Degraded []string
}

// DecideSafePark parks iff a CRITICAL dependency is DOWN (noting degraded optionals).
func DecideSafePark(statuses []DependencyStatus) SafeParkDecision {
	downCritical, degraded := []string{}, []string{}
	for _, s := range statuses {
		if s.Health == Down && critical[s.Name] {
			downCritical = append(downCritical, s.Name)
		}
		if s.Health != OK && optional[s.Name] {
			degraded = append(degraded, s.Name)
		}
	}
	if len(downCritical) > 0 {
		sort.Strings(downCritical)
		quoted := make([]string, len(downCritical))
		for i, n := range downCritical {
			quoted[i] = "'" + n + "'"
		}
		return SafeParkDecision{Park: true, Reason: fmt.Sprintf("critical dependency down: [%s]", strings.Join(quoted, ", ")), Degraded: degraded}
	}
	return SafeParkDecision{Park: false, Reason: "operational (some optionals degraded)", Degraded: degraded}
}

// MemoryMode is how memory retrieval runs given the optional deps' health.
type MemoryMode struct {
	Dense    bool // use the embedder / vector index channel
	GitGrep  bool // add `git grep` over the checkout as a lexical channel
	Reason   string
	Degraded []string
}

// Label is "hybrid" or "lexical".
func (m MemoryMode) Label() string {
	if m.Dense {
		return "hybrid"
	}
	return "lexical"
}

// DecideMemoryMode is hybrid (lexical + dense) iff every dense-channel dep is OK; else lexical +
// git grep. Never parks: memory is optional.
func DecideMemoryMode(statuses []DependencyStatus) MemoryMode {
	degradedSet := map[string]bool{}
	for _, s := range statuses {
		if memoryDenseDeps[s.Name] && s.Health != OK {
			degradedSet[s.Name] = true
		}
	}
	if len(degradedSet) == 0 {
		return MemoryMode{Dense: true, GitGrep: false, Reason: "hybrid retrieval (BM25 + dense)", Degraded: []string{}}
	}
	degraded := make([]string, 0, len(degradedSet))
	for name := range degradedSet {
		degraded = append(degraded, name)
	}
	sort.Strings(degraded)
	sorted := append([]DependencyStatus{}, statuses...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	details := []string{}
	for _, s := range sorted {
		if !degradedSet[s.Name] {
			continue
		}
		detail := s.Detail
		if detail == "" {
			detail = string(s.Health)
		}
		details = append(details, s.Name+": "+detail)
	}
	return MemoryMode{
		Dense: false, GitGrep: true,
		Reason:   "lexical-only retrieval (BM25 + git grep): " + strings.Join(details, "; "),
		Degraded: degraded,
	}
}
