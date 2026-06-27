package verify

import (
	"fmt"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// FlakeEvidenceError is returned when quarantining a check that has no recorded flake history.
type FlakeEvidenceError struct{ Message string }

func (e *FlakeEvidenceError) Error() string { return e.Message }

// FlakyQuarantine keeps flaky checks from gating progress. Quarantined checks still run (for
// visibility / flake-rate tracking) but as NON-gating.
//
// Quarantine requires EVIDENCE: a check can only be marked flaky after it has been observed both
// passing and failing on the SAME revision, at least minFlips times per outcome over at least
// minRuns runs. Without that rule, MarkFlaky("pytest") would silently turn the suite advisory.
type FlakyQuarantine struct {
	minFlips int
	minRuns  int

	mu      sync.Mutex
	history map[string]map[string]*[2]int // name -> revision -> [passes, fails]
	flaky   map[string]bool
}

// NewFlakyQuarantine returns a quarantine (Python defaults: minFlips=1, minRuns=3). minFlips is
// clamped to >= 1 and minRuns to >= 2.
func NewFlakyQuarantine(minFlips, minRuns int) *FlakyQuarantine {
	return &FlakyQuarantine{
		minFlips: max(1, minFlips),
		minRuns:  max(2, minRuns),
		history:  map[string]map[string]*[2]int{},
		flaky:    map[string]bool{},
	}
}

// NewDefaultFlakyQuarantine is NewFlakyQuarantine(1, 3).
func NewDefaultFlakyQuarantine() *FlakyQuarantine { return NewFlakyQuarantine(1, 3) }

// Record records one observed outcome of check name on code revision (e.g. a git sha).
func (q *FlakyQuarantine) Record(name, revision string, passed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	byRev := q.history[name]
	if byRev == nil {
		byRev = map[string]*[2]int{}
		q.history[name] = byRev
	}
	counts := byRev[revision]
	if counts == nil {
		counts = &[2]int{}
		byRev[revision] = counts
	}
	if passed {
		counts[0]++
	} else {
		counts[1]++
	}
}

// RecordResults records every result on revision.
func (q *FlakyQuarantine) RecordResults(results []contracts.CheckResult, revision string) {
	for _, r := range results {
		q.Record(r.Name, revision, r.Passed)
	}
}

// HasFlakeEvidence reports whether name passed and failed on one revision often enough.
func (q *FlakyQuarantine) HasFlakeEvidence(name string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.hasEvidence(name)
}

func (q *FlakyQuarantine) hasEvidence(name string) bool {
	for _, c := range q.history[name] {
		passes, fails := c[0], c[1]
		if passes >= q.minFlips && fails >= q.minFlips && passes+fails >= q.minRuns {
			return true
		}
	}
	return false
}

// MarkFlaky quarantines name; it returns a *FlakeEvidenceError without recorded flake history.
func (q *FlakyQuarantine) MarkFlaky(name string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.hasEvidence(name) {
		return &FlakeEvidenceError{fmt.Sprintf(
			"refusing to quarantine %s: no recorded pass+fail on the same revision "+
				"(need >= %d of each over >= %d runs)", contracts.PyRepr(name), q.minFlips, q.minRuns)}
	}
	q.flaky[name] = true
	return nil
}

// IsFlaky reports whether name is quarantined.
func (q *FlakyQuarantine) IsFlaky(name string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.flaky[name]
}

// Partition splits checks into (gating, quarantined); quarantined checks are forced non-gating.
func (q *FlakyQuarantine) Partition(checks []contracts.Check) (gating, quarantined []contracts.Check) {
	q.mu.Lock()
	defer q.mu.Unlock()
	gating, quarantined = []contracts.Check{}, []contracts.Check{}
	for _, c := range checks {
		if q.flaky[c.Name] {
			c.Gating = false
			quarantined = append(quarantined, c)
		} else {
			gating = append(gating, c)
		}
	}
	return gating, quarantined
}
