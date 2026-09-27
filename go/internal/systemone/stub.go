package systemone

import (
	"context"
	"strconv"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Responder answers a request's questions by id (missing ids get a neutral answer).
type Responder func(state any, questions []Named) map[string]Answer

// Stub is an offline System One model for tests (python: StubSystemOne): scripted answers, or a
// failure, for every call. The default answer is uniform (zero confidence).
type Stub struct {
	Respond Responder
	Err     string

	mu    sync.Mutex
	calls int
}

// Name is "systemone:stub".
func (*Stub) Name() string { return "systemone:stub" }

// Calls is the number of evaluations so far.
func (s *Stub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Evaluate answers with Respond.
func (s *Stub) Evaluate(_ context.Context, state any, questions []Named) (Result, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.Err != "" {
		return Result{}, &Error{Message: s.Err}
	}
	var scripted map[string]Answer
	if s.Respond != nil {
		scripted = s.Respond(state, questions)
	}
	out := Result{Model: "stub-1", Answers: map[string]Answer{}}
	for _, n := range questions {
		a, ok := scripted[n.ID]
		if !ok {
			a = neutral(n.Question)
		}
		out.IDs = append(out.IDs, n.ID)
		out.Answers[n.ID] = a
	}
	return out, nil
}

// ChoiceAnswer is the answer for a distribution over options, in order (the most likely option,
// standard confidence).
func ChoiceAnswer(options []string, probs []float64) Answer {
	m := contracts.NewOrderedMap()
	best := 0
	for i, o := range options {
		m.Set(o, probs[i])
		if probs[i] > probs[best] {
			best = i
		}
	}
	return Answer{Type: "choice", Choice: options[best], Probabilities: m, Confidence: ChoiceConfidence(probs)}
}

func neutral(q Question) Answer {
	switch q.Type {
	case "noul":
		return Answer{Type: "noul", Noul: 0.5}
	case "choice":
		opts := q.options()
		probs := make([]float64, len(opts))
		for i := range probs {
			probs[i] = 1 / float64(len(opts))
		}
		return ChoiceAnswer(opts, probs)
	}
	n := q.levels()
	probs := contracts.NewOrderedMap()
	flat := make([]float64, n)
	score := 0.0
	for i := range n {
		flat[i] = 1 / float64(n)
		probs.Set(strconv.Itoa(i), flat[i])
		score += float64(i) * flat[i]
	}
	return Answer{Type: "score", Score: score, Probabilities: probs, Confidence: ScoreConfidence(flat)}
}
