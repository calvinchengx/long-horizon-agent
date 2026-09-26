package memory

import (
	"context"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// The procedural skill library (python: lha.memory.skills). A skill is admitted ONLY when it is
// verified (it passed the deterministic test gate), carries provenance + an expiry so a wrong
// lesson can't fossilize, and is retrieved by description similarity within its namespace (the
// repo) or "global".

// InMemorySkillStore stores verified skills and retrieves them by description similarity.
type InMemorySkillStore struct {
	mu    sync.Mutex
	index *InMemorySemanticIndex
	byID  map[string]contracts.Skill
}

// NewInMemorySkillStore is an empty store over embedder.
func NewInMemorySkillStore(embedder contracts.Embedder) *InMemorySkillStore {
	index, _ := NewInMemorySemanticIndex(embedder, 0)
	return &InMemorySkillStore{index: index, byID: map[string]contracts.Skill{}}
}

// Add admits a verified skill (an unverified one is a *contracts.SkillNotVerifiedError).
func (s *InMemorySkillStore) Add(ctx context.Context, skill contracts.Skill) error {
	if !skill.Verified {
		return contracts.UnverifiedSkill("admit", skill.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[skill.ID] = skill
	return s.index.Add(ctx, []contracts.MemoryRecord{contracts.NewMemoryRecord(skill.ID, "procedural",
		skill.Description, map[string]string{"namespace": skill.Namespace, "name": skill.Name})})
}

// Find is the k skills most similar to query ("" namespace = any; else namespace or global).
func (s *InMemorySkillStore) Find(ctx context.Context, query string, k int, namespace string) ([]contracts.Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := k
	if namespace != "" {
		want = 2 * k
	}
	hits, err := s.index.Query(ctx, query, want)
	if err != nil {
		return nil, err
	}
	out := []contracts.Skill{}
	for _, h := range hits {
		sk, ok := s.byID[h.Record.ID]
		if !ok {
			continue
		}
		if namespace != "" && sk.Namespace != namespace && sk.Namespace != "global" {
			continue
		}
		out = append(out, sk)
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

// Get is the skill by id.
func (s *InMemorySkillStore) Get(id string) (contracts.Skill, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk, ok := s.byID[id]
	return sk, ok
}

// InMemoryEpisodicLog is a fast in-process episodic view for one session (python:
// lha.memory.episodic): append + tail.
type InMemoryEpisodicLog struct {
	mu     sync.Mutex
	events []contracts.EventRecord
}

// Append adds an event.
func (l *InMemoryEpisodicLog) Append(event contracts.EventRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// Tail is the last n events.
func (l *InMemoryEpisodicLog) Tail(n int) []contracts.EventRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.events) {
		n = len(l.events)
	}
	return append([]contracts.EventRecord{}, l.events[len(l.events)-n:]...)
}

// Len is the number of events.
func (l *InMemoryEpisodicLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}
