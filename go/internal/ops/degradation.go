package ops

import (
	"fmt"
	"sort"
	"strings"
)

// Dependency degradation -> safe-park decisions (python/src/lha/ops/degradation.py).
//
// Critical dependencies (git, the model, the sandbox) being DOWN means no verified progress is
// possible, so a durable mission parks (status DEGRADED_PARK) and re-probes health with backoff.
// Optional dependencies (Postgres, pgvector, embeddings, Langfuse, the egress proxy) degrade
// gracefully. DecideMemoryMode narrows memory retrieval to the lexical channels when the dense
// channel's dependencies are not OK; it never parks.

// Health is one dependency's health (values match the Python enum).
type Health string

// The health values.
const (
	HealthOK       Health = "ok"
	HealthDegraded Health = "degraded"
	HealthDown     Health = "down"
)

// Critical dependencies: without these no verified progress is possible, so the mission parks.
var Critical = map[string]bool{"git": true, "model": true, "sandbox": true}

// Optional dependencies degrade gracefully.
var Optional = map[string]bool{
	"postgres": true, "pgvector": true, "embeddings": true, "langfuse": true, "egress_proxy": true,
}

// MemoryDenseDeps are the optional dependencies the dense (vector) memory channel needs.
var MemoryDenseDeps = map[string]bool{"postgres": true, "pgvector": true, "embeddings": true}

// DependencyStatus is one probed dependency.
type DependencyStatus struct {
	Name   string
	Health Health
	Detail string
}

// SafeParkDecision says whether to park, why, and which optionals are degraded.
type SafeParkDecision struct {
	Park     bool
	Reason   string
	Degraded []string
}

// pyList renders names like Python's repr of a list of str: ['git', 'model'].
func pyList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// DecideSafePark parks iff a CRITICAL dependency is DOWN; otherwise continues, noting degraded
// optionals (python: decide_safe_park).
func DecideSafePark(statuses []DependencyStatus) SafeParkDecision {
	var downCritical []string
	degraded := []string{}
	for _, s := range statuses {
		if s.Health == HealthDown && Critical[s.Name] {
			downCritical = append(downCritical, s.Name)
		}
		if s.Health != HealthOK && Optional[s.Name] {
			degraded = append(degraded, s.Name)
		}
	}
	if len(downCritical) > 0 {
		sort.Strings(downCritical)
		return SafeParkDecision{
			Park:     true,
			Reason:   fmt.Sprintf("critical dependency down: %s", pyList(downCritical)),
			Degraded: degraded,
		}
	}
	return SafeParkDecision{Park: false, Reason: "operational (some optionals degraded)", Degraded: degraded}
}

// MemoryMode is how memory retrieval runs given the optional dependencies' health.
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

// DecideMemoryMode is hybrid (lexical + dense) iff every dense-channel dependency is OK; else
// lexical + git grep (python: decide_memory_mode). Never parks.
func DecideMemoryMode(statuses []DependencyStatus) MemoryMode {
	var degraded []string
	for _, s := range statuses {
		if MemoryDenseDeps[s.Name] && s.Health != HealthOK {
			degraded = append(degraded, s.Name)
		}
	}
	if len(degraded) == 0 {
		return MemoryMode{Dense: true, Reason: "hybrid retrieval (BM25 + dense)", Degraded: []string{}}
	}
	sort.Strings(degraded)
	bad := map[string]bool{}
	for _, n := range degraded {
		bad[n] = true
	}
	sorted := append([]DependencyStatus{}, statuses...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var details []string
	for _, s := range sorted {
		if bad[s.Name] {
			detail := s.Detail
			if detail == "" {
				detail = string(s.Health)
			}
			details = append(details, s.Name+": "+detail)
		}
	}
	return MemoryMode{
		Dense:    false,
		GitGrep:  true,
		Reason:   "lexical-only retrieval (BM25 + git grep): " + strings.Join(details, "; "),
		Degraded: degraded,
	}
}
