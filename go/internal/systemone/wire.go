// Package systemone is the System One decision-model layer (python: lha.systemone): typed
// questions in, calibrated typed answers out, no generated text. A System One model is
// TypeSafe's hosted Jev, or a self-hosted server speaking the same POST /v1/systemone API (Kev).
//
// LHA uses the answers only to make itself more cautious or cheaper (split or block an item
// early, reorder recalled memory), never to allow an action or mark work done. A failed call
// changes nothing.
//
// JSON values (instructions, criteria, state) are ordered JSON values as contracts.DecodeOrdered
// produces them (*contracts.OrderedMap objects): option order reaches the model as written, as
// it does from Python.
package systemone

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// API limits (TypeSafe's published limits, checked client-side).
const (
	MaxChoiceOptions = 255
	MinScoreLevels   = 2
	MaxScoreLevels   = 10
	// SumTolerance is how far a distribution's sum may be from 1 (servers round probabilities).
	SumTolerance = 0.05
)

// Error is a failed System One call: network, HTTP status, budget or a malformed answer
// (python: SystemOneError).
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func errorf(format string, args ...any) *Error { return &Error{Message: fmt.Sprintf(format, args...)} }

// Question is one typed question. Criteria is nil or *contracts.OrderedMap for a noul
// ("true"/"false" descriptions), a *contracts.OrderedMap of option -> description for a choice,
// and a []any of level descriptions (lowest first) for a score.
type Question struct {
	Type         string
	Instructions any
	Criteria     any
}

// Named is a question under the id its answer comes back under.
type Named struct {
	ID       string
	Question Question
}

// Noul is a yes/no question (criteria may be nil).
func Noul(instructions any, criteria *contracts.OrderedMap) Question {
	q := Question{Type: "noul", Instructions: instructions}
	if criteria != nil {
		q.Criteria = criteria
	}
	return q
}

// Choice picks one of the options of criteria (option -> description or nil).
func Choice(instructions any, criteria *contracts.OrderedMap) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: criteria}
}

// Score rates against ordered levels.
func Score(instructions any, levels []any) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// QuestionFromWire is a question from its wire form (an ordered JSON object).
func QuestionFromWire(raw any) (Question, error) {
	m, ok := raw.(*contracts.OrderedMap)
	if !ok {
		return Question{}, errorf("a question is not an object")
	}
	kind, _ := m.String("type")
	criteria, hasCriteria := m.Get("criteria")
	switch kind {
	case "noul":
		if !hasCriteria || criteria == nil {
			return Question{Type: kind, Instructions: m.Value("instructions")}, nil
		}
		if _, ok := criteria.(*contracts.OrderedMap); !ok {
			return Question{}, errorf("noul criteria is not an object")
		}
	case "choice":
		if _, ok := criteria.(*contracts.OrderedMap); !ok {
			return Question{}, errorf("choice criteria is not an object")
		}
	case "score":
		if _, ok := criteria.([]any); !ok {
			return Question{}, errorf("score criteria is not a list")
		}
	default:
		return Question{}, errorf("unknown question type %s", contracts.PyRepr(kind))
	}
	return Question{Type: kind, Instructions: m.Value("instructions"), Criteria: criteria}, nil
}

// Body is the question as it goes on the wire (a noul without criteria omits the field).
func (q Question) Body() *contracts.OrderedMap {
	body := contracts.NewOrderedMap("type", q.Type, "instructions", q.Instructions)
	if q.Criteria != nil {
		body.Set("criteria", q.Criteria)
	}
	return body
}

// options are a choice's option names in order.
func (q Question) options() []string {
	if m, ok := q.Criteria.(*contracts.OrderedMap); ok {
		return append([]string{}, m.Keys...)
	}
	return nil
}

// levels are a score's level count.
func (q Question) levels() int {
	l, _ := q.Criteria.([]any)
	return len(l)
}

func checkQuestion(id string, q Question) error {
	if q.Type == "choice" && (len(q.options()) < 1 || len(q.options()) > MaxChoiceOptions) {
		return errorf("question %s: a choice needs 1-%d options", contracts.PyRepr(id), MaxChoiceOptions)
	}
	if q.Type == "score" && (q.levels() < MinScoreLevels || q.levels() > MaxScoreLevels) {
		return errorf("question %s: a score needs %d-%d levels", contracts.PyRepr(id), MinScoreLevels, MaxScoreLevels)
	}
	return nil
}

// RequestBody is the JSON body of one evaluation request.
func RequestBody(model string, state any, questions []Named) (*contracts.OrderedMap, error) {
	if len(questions) == 0 {
		return nil, errorf("an evaluation needs at least one question")
	}
	qs := contracts.NewOrderedMap()
	for _, n := range questions {
		if err := checkQuestion(n.ID, n.Question); err != nil {
			return nil, err
		}
		qs.Set(n.ID, n.Question.Body())
	}
	return contracts.NewOrderedMap("model", model, "state", state, "questions", qs), nil
}

// ChoiceConfidence is (p_max - 1/K) / (1 - 1/K): 1 when one option has everything, 0 when uniform.
func ChoiceConfidence(p []float64) float64 {
	k := len(p)
	if k <= 1 {
		return 1.0
	}
	top := p[0]
	for _, v := range p[1:] {
		top = math.Max(top, v)
	}
	kf := float64(k)
	return math.Max(0.0, math.Min(1.0, (top-1/kf)/(1-1/kf)))
}

// ScoreConfidence is max(0, 1 - E|level - mode| / D), D being the mean distance of a uniform
// distribution over the levels from its middle (2/3 for three levels).
func ScoreConfidence(p []float64) float64 {
	n := len(p)
	if n <= 1 {
		return 1.0
	}
	mode := 0
	for i, v := range p {
		if v > p[mode] {
			mode = i
		}
	}
	spread := 0.0
	for i, v := range p {
		spread += v * math.Abs(float64(i-mode))
	}
	middle := float64(n-1) / 2
	uniform := 0.0
	for i := range n {
		uniform += math.Abs(float64(i) - middle)
	}
	uniform /= float64(n)
	return math.Max(0.0, 1-spread/uniform)
}

// Answer is one typed answer. Probabilities maps each option (choice) or level index "0", "1",
// ... (score) to its probability, in the question's order.
type Answer struct {
	Type          string
	Noul          float64
	Choice        string
	Score         float64
	Probabilities *contracts.OrderedMap
	Confidence    float64
}

// Prob is the probability of option or level key.
func (a Answer) Prob(key string) float64 {
	f, _ := contracts.AsFloat(a.Probabilities.Value(key))
	return f
}

// JSON is the answer as python's model_dump writes it.
func (a Answer) JSON() *contracts.OrderedMap {
	switch a.Type {
	case "noul":
		return contracts.NewOrderedMap("type", "noul", "noul", a.Noul)
	case "choice":
		return contracts.NewOrderedMap("type", "choice", "choice", a.Choice, "probabilities", a.Probabilities, "confidence", a.Confidence)
	}
	return contracts.NewOrderedMap("type", "score", "score", a.Score, "probabilities", a.Probabilities, "confidence", a.Confidence)
}

// Result is one evaluation: the versioned model that answered, one answer per question (in the
// order asked), token usage.
type Result struct {
	Model        string
	IDs          []string
	Answers      map[string]Answer
	InputTokens  int
	OutputTokens int
}

// JSON is the result as python's model_dump writes it.
func (r Result) JSON() *contracts.OrderedMap {
	answers := contracts.NewOrderedMap()
	for _, id := range r.IDs {
		answers.Set(id, r.Answers[id].JSON())
	}
	return contracts.NewOrderedMap("model", r.Model, "answers", answers,
		"input_tokens", r.InputTokens, "output_tokens", r.OutputTokens)
}

func number(v any, what string, low, high float64) (float64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, errorf("%s is not a number: %s", what, pyfmt.PyReprValue(v))
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if (err != nil && !isRange(err)) || math.IsNaN(f) || math.IsInf(f, 0) || f < low || f > high {
		return 0, errorf("%s is outside [%g, %g]: %s", what, low, high, string(n))
	}
	return f, nil
}

func isRange(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

func distribution(v any, keys []string, what string) (*contracts.OrderedMap, error) {
	m, ok := v.(*contracts.OrderedMap)
	if !ok {
		return nil, errorf("%s probabilities are not an object", what)
	}
	got := append([]string{}, m.Keys...)
	want := append([]string{}, keys...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") || len(got) != len(want) {
		return nil, errorf("%s probabilities name %v, expected %v", what, got, want)
	}
	probs := contracts.NewOrderedMap()
	sum := 0.0
	for _, k := range keys {
		f, err := number(m.Value(k), fmt.Sprintf("%s probability of %s", what, contracts.PyRepr(k)), 0, 1)
		if err != nil {
			return nil, err
		}
		probs.Set(k, f)
		sum += f
	}
	if math.Abs(sum-1.0) > SumTolerance {
		return nil, errorf("%s probabilities sum to %.4f, not 1", what, sum)
	}
	return probs, nil
}

// ParseAnswer is one answer, checked against the question that was asked.
func ParseAnswer(id string, q Question, raw any) (Answer, error) {
	what := "answer " + contracts.PyRepr(id)
	m, ok := raw.(*contracts.OrderedMap)
	if !ok {
		return Answer{}, errorf("%s is not an object", what)
	}
	if kind, _ := m.String("type"); kind != q.Type {
		return Answer{}, errorf("%s has type %s, expected %s", what, pyfmt.PyReprValue(m.Value("type")), contracts.PyRepr(q.Type))
	}
	switch q.Type {
	case "noul":
		f, err := number(m.Value("noul"), what+" noul", 0, 1)
		return Answer{Type: "noul", Noul: f}, err
	case "choice":
		probs, err := distribution(m.Value("probabilities"), q.options(), what)
		if err != nil {
			return Answer{}, err
		}
		choice, ok := m.Value("choice").(string)
		if !ok || !contains(q.options(), choice) {
			return Answer{}, errorf("%s chose %s, not one of the options", what, pyfmt.PyReprValue(m.Value("choice")))
		}
		confidence, err := number(m.Value("confidence"), what+" confidence", 0, 1)
		if err != nil {
			return Answer{}, err
		}
		return Answer{Type: "choice", Choice: choice, Probabilities: probs, Confidence: confidence}, nil
	}
	levels := make([]string, q.levels())
	for i := range levels {
		levels[i] = strconv.Itoa(i)
	}
	probs, err := distribution(m.Value("probabilities"), levels, what)
	if err != nil {
		return Answer{}, err
	}
	score, err := number(m.Value("score"), what+" score", 0, float64(len(levels)-1))
	if err != nil {
		return Answer{}, err
	}
	confidence, err := number(m.Value("confidence"), what+" confidence", 0, 1)
	if err != nil {
		return Answer{}, err
	}
	return Answer{Type: "score", Score: score, Probabilities: probs, Confidence: confidence}, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func tokens(usage *contracts.OrderedMap, key string) (int, error) {
	v, ok := usage.Get(key)
	if !ok {
		return 0, nil
	}
	n, isNum := v.(json.Number)
	if !isNum || strings.ContainsAny(string(n), ".eE") {
		return 0, errorf("usage %s is not a non-negative integer: %s", key, pyfmt.PyReprValue(v))
	}
	i, err := strconv.Atoi(string(n))
	if err != nil || i < 0 {
		return 0, errorf("usage %s is not a non-negative integer: %s", key, string(n))
	}
	return i, nil
}

// ParseResponse is a response body checked against the questions asked (extra answers are
// ignored).
func ParseResponse(body []byte, questions []Named) (Result, error) {
	data, err := contracts.DecodeOrdered(body)
	if err != nil {
		return Result{}, errorf("the response is not JSON: %v", err)
	}
	return ParseResponseValue(data, questions)
}

// ParseResponseValue is ParseResponse for an already decoded (ordered) value.
func ParseResponseValue(data any, questions []Named) (Result, error) {
	obj, ok := data.(*contracts.OrderedMap)
	if !ok {
		return Result{}, errorf("the response is not an object")
	}
	answers, ok := obj.Value("answers").(*contracts.OrderedMap)
	if !ok {
		return Result{}, errorf("the response has no answers object")
	}
	model := ""
	if v, present := obj.Get("model"); present {
		s, isStr := v.(string)
		if !isStr {
			return Result{}, errorf("the response model is not a string: %s", pyfmt.PyReprValue(v))
		}
		model = s
	}
	usage := contracts.NewOrderedMap()
	if v, present := obj.Get("usage"); present && v != nil {
		m, isMap := v.(*contracts.OrderedMap)
		if !isMap {
			return Result{}, errorf("the response usage is not an object")
		}
		usage = m
	}
	out := Result{Model: model, Answers: map[string]Answer{}}
	var err error
	for _, n := range questions {
		raw, present := answers.Get(n.ID)
		if !present {
			return Result{}, errorf("answer %s is missing", contracts.PyRepr(n.ID))
		}
		a, aerr := ParseAnswer(n.ID, n.Question, raw)
		if aerr != nil {
			return Result{}, aerr
		}
		out.IDs = append(out.IDs, n.ID)
		out.Answers[n.ID] = a
	}
	if out.InputTokens, err = tokens(usage, "input_tokens"); err != nil {
		return Result{}, err
	}
	if out.OutputTokens, err = tokens(usage, "output_tokens"); err != nil {
		return Result{}, err
	}
	return out, nil
}
