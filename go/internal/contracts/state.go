package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Item statuses (python: ItemStatus Literal).
const (
	StatusTodo       = "todo"
	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusDone       = "done"
)

// MissionSpec is the immutable mission definition, written once at initialization.
type MissionSpec struct {
	Title         string `json:"title"`
	Description   string `json:"description"`
	Acceptance    string `json:"acceptance"`
	SchemaVersion int    `json:"schema_version"`
}

// UnmarshalJSON applies the pydantic defaults for missing fields.
func (m *MissionSpec) UnmarshalJSON(data []byte) error {
	type alias MissionSpec
	a := alias{SchemaVersion: 1}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = MissionSpec(a)
	return nil
}

// RenderAnchor is the mission recitation placed at the top of every cycle's system prompt.
func (m MissionSpec) RenderAnchor() string {
	text := strings.TrimRight(fmt.Sprintf("Mission: %s\n\n%s", m.Title, m.Description), pyWhitespace)
	if acc := strings.TrimSpace(m.Acceptance); acc != "" {
		text += "\n\nDefinition of done: " + acc
	}
	return text
}

// pyWhitespace is what Python's str.rstrip() removes for ASCII text.
const pyWhitespace = " \t\n\r\x0b\x0c"

// ChecklistItem is one mission work-unit; it flips to done only after deterministic verification.
type ChecklistItem struct {
	ID                  string   `json:"id"`
	Description         string   `json:"description"`
	Status              string   `json:"status"`
	VerifiedBy          []string `json:"verified_by"`
	DependsOn           []string `json:"depends_on"`
	Attempts            int      `json:"attempts"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
	LastFailure         string   `json:"last_failure"`
	AllowHarnessEdits   bool     `json:"allow_harness_edits"`
	Notes               string   `json:"notes"`
	SchemaVersion       int      `json:"schema_version"`
}

// NewChecklistItem builds an item with the pydantic defaults.
func NewChecklistItem(id, description string, dependsOn ...string) ChecklistItem {
	return ChecklistItem{
		ID: id, Description: description, Status: StatusTodo,
		VerifiedBy: []string{}, DependsOn: append([]string{}, dependsOn...), SchemaVersion: 1,
	}
}

// MarshalJSON never emits null for the list fields (pydantic dumps []).
func (i ChecklistItem) MarshalJSON() ([]byte, error) {
	type alias ChecklistItem
	a := alias(i)
	a.VerifiedBy = orEmpty(a.VerifiedBy)
	a.DependsOn = orEmpty(a.DependsOn)
	return json.Marshal(a)
}

// UnmarshalJSON applies defaults and validates the status literal.
func (i *ChecklistItem) UnmarshalJSON(data []byte) error {
	type alias ChecklistItem
	a := alias{Status: StatusTodo, SchemaVersion: 1}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	switch a.Status {
	case StatusTodo, StatusInProgress, StatusBlocked, StatusDone:
	default:
		return fmt.Errorf("invalid checklist item status %q", a.Status)
	}
	a.VerifiedBy = orEmpty(a.VerifiedBy)
	a.DependsOn = orEmpty(a.DependsOn)
	*i = ChecklistItem(a)
	return nil
}

// IsOpen reports todo / in_progress / blocked.
func (i ChecklistItem) IsOpen() bool {
	return i.Status == StatusTodo || i.Status == StatusInProgress || i.Status == StatusBlocked
}

// IsActionableStatus reports todo / in_progress.
func (i ChecklistItem) IsActionableStatus() bool {
	return i.Status == StatusTodo || i.Status == StatusInProgress
}

// Checklist is the ordered set of mission items.
type Checklist struct {
	Items         []ChecklistItem `json:"items"`
	SchemaVersion int             `json:"schema_version"`
}

// MarshalJSON never emits null for items.
func (c Checklist) MarshalJSON() ([]byte, error) {
	type alias Checklist
	a := alias(c)
	if a.Items == nil {
		a.Items = []ChecklistItem{}
	}
	return json.Marshal(a)
}

// UnmarshalJSON applies defaults.
func (c *Checklist) UnmarshalJSON(data []byte) error {
	type alias Checklist
	a := alias{SchemaVersion: 1}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if a.Items == nil {
		a.Items = []ChecklistItem{}
	}
	*c = Checklist(a)
	return nil
}

// Get returns a pointer to the item with id, or nil.
func (c *Checklist) Get(id string) *ChecklistItem {
	for idx := range c.Items {
		if c.Items[idx].ID == id {
			return &c.Items[idx]
		}
	}
	return nil
}

// NextActionable is the next item to work: an in_progress one first, else the first ready todo.
func (c *Checklist) NextActionable() *ChecklistItem {
	done := map[string]bool{}
	for _, it := range c.Items {
		if it.Status == StatusDone {
			done[it.ID] = true
		}
	}
	var firstReady *ChecklistItem
	for idx := range c.Items {
		it := &c.Items[idx]
		if !it.IsActionableStatus() {
			continue
		}
		ready := true
		for _, dep := range it.DependsOn {
			if !done[dep] {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if it.Status == StatusInProgress {
			return it
		}
		if firstReady == nil {
			firstReady = it
		}
	}
	return firstReady
}

// AllDone reports a non-empty checklist whose items are all done.
func (c *Checklist) AllDone() bool {
	if len(c.Items) == 0 {
		return false
	}
	for _, it := range c.Items {
		if it.Status != StatusDone {
			return false
		}
	}
	return true
}

// IsComplete reports every item verified done (an empty checklist is NOT complete).
func (c *Checklist) IsComplete() bool { return c.AllDone() }

// IsDeadlocked reports not complete, yet nothing actionable.
func (c *Checklist) IsDeadlocked() bool { return !c.AllDone() && c.NextActionable() == nil }

// ItemsDone counts done items.
func (c *Checklist) ItemsDone() int {
	n := 0
	for _, it := range c.Items {
		if it.Status == StatusDone {
			n++
		}
	}
	return n
}

// BlockedItems lists blocked items.
func (c *Checklist) BlockedItems() []ChecklistItem {
	out := []ChecklistItem{}
	for _, it := range c.Items {
		if it.Status == StatusBlocked {
			out = append(out, it)
		}
	}
	return out
}

// DependencyErrors lists structural problems: duplicate ids, unknown deps, cycles. The traversal
// order matches the Python implementation exactly, so the messages are identical.
func (c *Checklist) DependencyErrors() []string {
	errs := []string{}
	seen := map[string]bool{}
	for _, it := range c.Items {
		if seen[it.ID] {
			errs = append(errs, fmt.Sprintf("duplicate item id %s", PyRepr(it.ID)))
		}
		seen[it.ID] = true
	}
	// graph keeps first-insertion order of ids (a Python dict re-assigned on duplicates keeps its
	// original position but takes the LAST value).
	graph := map[string][]string{}
	order := []string{}
	for _, it := range c.Items {
		deps := []string{}
		for _, dep := range it.DependsOn {
			switch {
			case dep == it.ID:
				errs = append(errs, fmt.Sprintf("item %s depends on itself", PyRepr(it.ID)))
			case !seen[dep]:
				errs = append(errs, fmt.Sprintf("item %s depends on unknown item %s", PyRepr(it.ID), PyRepr(dep)))
			default:
				deps = append(deps, dep)
			}
		}
		if _, ok := graph[it.ID]; !ok {
			order = append(order, it.ID)
		}
		graph[it.ID] = deps
	}
	state := map[string]int{}
	type frame struct {
		node string
		idx  int
	}
	for _, root := range order {
		if state[root] != 0 {
			continue
		}
		stack := []frame{{root, 0}}
		path := []string{}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if f.idx == 0 {
				state[f.node] = 1
				path = append(path, f.node)
			}
			deps := graph[f.node]
			if f.idx < len(deps) {
				stack = append(stack, frame{f.node, f.idx + 1})
				nxt := deps[f.idx]
				if state[nxt] == 1 {
					start := indexOf(path, nxt)
					cycle := append(append([]string{}, path[start:]...), nxt)
					errs = append(errs, "dependency cycle: "+strings.Join(cycle, " -> "))
				} else if state[nxt] == 0 {
					stack = append(stack, frame{nxt, 0})
				}
			} else {
				state[f.node] = 2
				path = path[:len(path)-1]
			}
		}
	}
	return errs
}

// DeadlockReason explains why no item is actionable ("" if not deadlocked).
func (c *Checklist) DeadlockReason() string {
	if !c.IsDeadlocked() {
		return ""
	}
	if len(c.Items) == 0 {
		return "checklist has no items"
	}
	reasons := []string{}
	if blocked := c.BlockedItems(); len(blocked) > 0 {
		ids := make([]string, len(blocked))
		for i, it := range blocked {
			ids[i] = it.ID
		}
		reasons = append(reasons, "blocked: "+strings.Join(ids, ", "))
	}
	reasons = append(reasons, c.DependencyErrors()...)
	if len(reasons) == 0 {
		reasons = append(reasons, "open items wait on dependencies that can never complete")
	}
	return strings.Join(reasons, "; ")
}

// ErrNoItem is returned for an unknown item id.
var ErrNoItem = errors.New("no checklist item")

func (c *Checklist) require(id string) (*ChecklistItem, error) {
	if it := c.Get(id); it != nil {
		return it, nil
	}
	return nil, fmt.Errorf("%w %s", ErrNoItem, PyRepr(id))
}

// Start marks id as being worked this cycle.
func (c *Checklist) Start(id string) (*ChecklistItem, error) {
	it, err := c.require(id)
	if err != nil {
		return nil, err
	}
	it.Status = StatusInProgress
	return it, nil
}

// RecordSuccess marks id done, recording the gating checks that proved it.
func (c *Checklist) RecordSuccess(id string, verifiedBy []string) (*ChecklistItem, error) {
	it, err := c.require(id)
	if err != nil {
		return nil, err
	}
	if len(verifiedBy) == 0 {
		return nil, errors.New("an item can only be marked done by at least one gating check")
	}
	it.Attempts++
	it.Status = StatusDone
	it.VerifiedBy = append([]string{}, verifiedBy...)
	it.ConsecutiveFailures = 0
	it.LastFailure = ""
	return it, nil
}

// RecordFailure records a failed attempt; blocks after maxConsecutiveFailures in a row.
func (c *Checklist) RecordFailure(id, reason string, maxConsecutiveFailures int) (*ChecklistItem, error) {
	it, err := c.require(id)
	if err != nil {
		return nil, err
	}
	it.Attempts++
	it.ConsecutiveFailures++
	it.LastFailure = reason
	if maxConsecutiveFailures > 0 && it.ConsecutiveFailures >= maxConsecutiveFailures {
		it.Status = StatusBlocked
	} else {
		it.Status = StatusInProgress
	}
	return it, nil
}

// Unblock makes a blocked item actionable again with a fresh failure budget.
func (c *Checklist) Unblock(id string) (*ChecklistItem, error) {
	it, err := c.require(id)
	if err != nil {
		return nil, err
	}
	if it.Status == StatusBlocked {
		it.Status = StatusTodo
	}
	it.ConsecutiveFailures = 0
	return it, nil
}

// DecisionRecord is a durable, never-compacted record of a design decision.
type DecisionRecord struct {
	Decision             string   `json:"decision"`
	Rationale            string   `json:"rationale"`
	AlternativesRejected string   `json:"alternatives_rejected"`
	Affected             []string `json:"affected"`
	CycleID              string   `json:"cycle_id"`
}

// MarshalJSON never emits null for affected.
func (d DecisionRecord) MarshalJSON() ([]byte, error) {
	type alias DecisionRecord
	a := alias(d)
	a.Affected = orEmpty(a.Affected)
	return json.Marshal(a)
}

// EventRecord is one entry in the append-only episodic event log.
type EventRecord struct {
	Kind       string         `json:"kind"`
	CycleID    string         `json:"cycle_id"`
	Payload    map[string]any `json:"payload"`
	PayloadRef *string        `json:"payload_ref"`
}

// MarshalJSON never emits null for payload.
func (e EventRecord) MarshalJSON() ([]byte, error) {
	type alias EventRecord
	a := alias(e)
	if a.Payload == nil {
		a.Payload = map[string]any{}
	}
	return json.Marshal(a)
}

// SituationSnapshot is reconstructed situational awareness. ActiveItem == nil does NOT mean
// complete: check IsComplete / IsDeadlocked.
type SituationSnapshot struct {
	HeadSHA         string           `json:"head_sha"`
	RecentCommits   []string         `json:"recent_commits"`
	Mission         *MissionSpec     `json:"mission"`
	ProgressSummary string           `json:"progress_summary"`
	OpenItems       []ChecklistItem  `json:"open_items"`
	LastDecisions   []DecisionRecord `json:"last_decisions"`
	ActiveItem      *ChecklistItem   `json:"active_item"`
	IsComplete      bool             `json:"is_complete"`
	IsDeadlocked    bool             `json:"is_deadlocked"`
	DeadlockReason  string           `json:"deadlock_reason"`
	ItemsDone       int              `json:"items_done"`
	ItemsTotal      int              `json:"items_total"`
}

// MarshalJSON never emits null for the list fields.
func (s SituationSnapshot) MarshalJSON() ([]byte, error) {
	type alias SituationSnapshot
	a := alias(s)
	a.RecentCommits = orEmpty(a.RecentCommits)
	if a.OpenItems == nil {
		a.OpenItems = []ChecklistItem{}
	}
	if a.LastDecisions == nil {
		a.LastDecisions = []DecisionRecord{}
	}
	return json.Marshal(a)
}

// AnchorText is the mission anchor to recite (falls back to the progress file).
func (s SituationSnapshot) AnchorText() string {
	if s.Mission != nil {
		return s.Mission.RenderAnchor()
	}
	if s.ProgressSummary != "" {
		return s.ProgressSummary
	}
	return "Mission (spec unavailable)"
}

// Checkpoint is the input to one atomic checkpoint commit.
type Checkpoint struct {
	CycleID         string           `json:"cycle_id"`
	ProgressSummary string           `json:"progress_summary"`
	Checklist       Checklist        `json:"checklist"`
	Decisions       []DecisionRecord `json:"decisions"`
	Events          []EventRecord    `json:"events"`
	CommitMessage   string           `json:"commit_message"`
	CommittedAt     *time.Time       `json:"committed_at"`
}

// DurableState reads and writes the mission's durable source of truth.
type DurableState interface {
	Initialize(ctx context.Context, title, description string, items Checklist) (string, error)
	ReadSituationalAwareness(ctx context.Context) (SituationSnapshot, error)
	AppendEvent(ctx context.Context, event EventRecord) error
	CommitCheckpoint(ctx context.Context, checkpoint Checkpoint) (string, error)
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
