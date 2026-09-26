package durable

import "encoding/json"

// Serializable types that cross the Temporal boundary (python/src/lha/durable/types.py).
//
// The JSON field names are the Python dataclass field names, so Temporal's JSON payload converter
// ("json/plain") produces payloads either implementation decodes. Python's `X | None` fields are
// pointers here (nil <-> null). Python cannot decode null into a `list[...]` field, so every list
// is marshalled as [] when nil (MarshalJSON). Decoding applies the dataclass defaults to absent
// fields (UnmarshalJSON), like Python's converter does.

func nz[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// PendingApproval is an irreversible tool call the agent attempted that needs a human decision.
type PendingApproval struct {
	Fingerprint string `json:"fingerprint"`
	Tool        string `json:"tool"`
	Reason      string `json:"reason"`
	Arguments   string `json:"arguments"`
}

// ApprovedAction is a human-approved tool call (by fingerprint), allowed once in a later cycle.
type ApprovedAction struct {
	Fingerprint string `json:"fingerprint"`
	Summary     string `json:"summary"`
}

// GateView is the open human gate (gate_v1 query; lha mission-status).
type GateView struct {
	GateID           string           `json:"gate_id"`
	Kind             string           `json:"kind"` // "tool_call" | "deadlock"
	Question         string           `json:"question"`
	Options          []string         `json:"options"`
	DefaultAction    string           `json:"default_action"`
	OpenedAt         string           `json:"opened_at"`
	Deadline         string           `json:"deadline"`
	EscalationsSent  int              `json:"escalations_sent"`
	NextEscalationAt string           `json:"next_escalation_at"`
	Recommended      string           `json:"recommended"`
	Request          *PendingApproval `json:"request"`
}

// MarshalJSON never emits null for options.
func (g GateView) MarshalJSON() ([]byte, error) {
	type alias GateView
	a := alias(g)
	a.Options = nz(a.Options)
	return json.Marshal(a)
}

// GateNotice is the input of notify_gate: one gate event.
type GateNotice struct {
	MissionID     string           `json:"mission_id"`
	Workdir       string           `json:"workdir"`
	GateID        string           `json:"gate_id"`
	Kind          string           `json:"kind"`
	Event         string           `json:"event"` // "opened" | "reminder" | "resolved" | "defaulted"
	Question      string           `json:"question"`
	Options       []string         `json:"options"`
	DefaultAction string           `json:"default_action"`
	Decision      string           `json:"decision"`
	Step          int              `json:"step"`
	Deadline      string           `json:"deadline"`
	Request       *PendingApproval `json:"request"`
	// At is when the event happened, in workflow time (ISO-8601 UTC); "" = use the activity clock.
	At string `json:"at"`
}

// MarshalJSON never emits null for options.
func (n GateNotice) MarshalJSON() ([]byte, error) {
	type alias GateNotice
	a := alias(n)
	a.Options = nz(a.Options)
	return json.Marshal(a)
}

// NoticeResult is notify_gate's outcome.
type NoticeResult struct {
	Recorded bool   `json:"recorded"`
	Webhook  string `json:"webhook"` // "off" | "sent" | "failed: <reason>"
	Stored   bool   `json:"stored"`  // written to the mission store's hitl_gates table
}

// UnmarshalJSON applies the dataclass defaults.
func (n *NoticeResult) UnmarshalJSON(data []byte) error {
	type alias NoticeResult
	a := alias{Webhook: "off"}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*n = NoticeResult(a)
	return nil
}

// FinalizeInput is the final checkpoint of a mission declared impossible.
type FinalizeInput struct {
	MissionID string `json:"mission_id"`
	Workdir   string `json:"workdir"`
	CycleID   string `json:"cycle_id"`
	Reason    string `json:"reason"`
}

// MissionStatusInput is a missions-row status the workflow decided (record_mission_status).
type MissionStatusInput struct {
	MissionID string  `json:"mission_id"`
	Workdir   string  `json:"workdir"`
	Status    string  `json:"status"`
	HeadSHA   *string `json:"head_sha"`
	Reason    string  `json:"reason"`
}

// MissionState is carried across Continue-As-New (pointers + small counters only).
type MissionState struct {
	CyclesDone      int              `json:"cycles_done"`
	Status          string           `json:"status"`
	HeadSHA         string           `json:"head_sha"`
	ItemsDone       int              `json:"items_done"`
	ItemsTotal      int              `json:"items_total"`
	LastItem        *string          `json:"last_item"`
	PendingDecision *string          `json:"pending_decision"`
	SteerNotes      []string         `json:"steer_notes"`
	Parks           int              `json:"parks"`
	DeadlockRetries int              `json:"deadlock_retries"`
	ApprovedActions []ApprovedAction `json:"approved_actions"`
	RejectedActions []string         `json:"rejected_actions"`
	FailItem        *string          `json:"fail_item"`
	FailStreak      int              `json:"fail_streak"`
	// ResumeAt: no cycle starts before this epoch time; 0 = none.
	ResumeAt    float64  `json:"resume_at"`
	Escalations int      `json:"escalations"`
	GateLog     []string `json:"gate_log"`
}

// NewMissionState is a MissionState with the dataclass defaults.
func NewMissionState() MissionState { return MissionState{Status: StatusRunning} }

// MarshalJSON never emits null for the lists.
func (s MissionState) MarshalJSON() ([]byte, error) {
	type alias MissionState
	a := alias(s)
	a.SteerNotes = nz(a.SteerNotes)
	a.ApprovedActions = nz(a.ApprovedActions)
	a.RejectedActions = nz(a.RejectedActions)
	a.GateLog = nz(a.GateLog)
	return json.Marshal(a)
}

// UnmarshalJSON applies the dataclass defaults.
func (s *MissionState) UnmarshalJSON(data []byte) error {
	type alias MissionState
	a := alias(NewMissionState())
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = MissionState(a)
	return nil
}

// DefaultGateEscalationSeconds is MissionInput.gate_escalation_seconds' default.
var DefaultGateEscalationSeconds = []int{900, 2700, 14_400, 43_200}

// MissionInput starts a mission; the anchor + checklist must already be initialized at Workdir.
type MissionInput struct {
	MissionID string `json:"mission_id"`
	Workdir   string `json:"workdir"`
	MaxCycles int    `json:"max_cycles"`
	// Continue-As-New after this many cycles (>= 1).
	CyclesBeforeCAN int `json:"cycles_before_can"`
	// CheckCommands: nil = the default Python gate; an explicit empty list is rejected.
	CheckCommands [][]string `json:"check_commands"`
	// BudgetUSD: nil = the worker's LHA_BUDGET_USD_CEILING.
	BudgetUSD               *float64      `json:"budget_usd"`
	ParkInitialSeconds      int           `json:"park_initial_seconds"`
	ParkMaxSeconds          int           `json:"park_max_seconds"`
	DeadlockGateSeconds     int           `json:"deadlock_gate_seconds"`
	ApprovalTimeoutSeconds  int           `json:"approval_timeout_seconds"`
	GateEscalationSeconds   []int         `json:"gate_escalation_seconds"`
	DeadlockGateDefault     string        `json:"deadlock_gate_default"`
	ImpossibleAfterFailures int           `json:"impossible_after_failures"`
	CyclePauseSeconds       int           `json:"cycle_pause_seconds"`
	ResumeAt                float64       `json:"resume_at"`
	ResearchPerItem         int           `json:"research_per_item"`
	Review                  bool          `json:"review"`
	MaxParallel             int           `json:"max_parallel"`
	State                   *MissionState `json:"state"`
}

// NewMissionInput is a MissionInput with the dataclass defaults.
func NewMissionInput(missionID, workdir string) MissionInput {
	return MissionInput{
		MissionID:               missionID,
		Workdir:                 workdir,
		MaxCycles:               1000,
		CyclesBeforeCAN:         200,
		ParkInitialSeconds:      60,
		ParkMaxSeconds:          3600,
		ApprovalTimeoutSeconds:  86_400,
		GateEscalationSeconds:   append([]int{}, DefaultGateEscalationSeconds...),
		DeadlockGateDefault:     "abort",
		ImpossibleAfterFailures: 3,
	}
}

func nzCommands(cmds [][]string) [][]string {
	if cmds == nil {
		return nil // null: the default checks
	}
	out := make([][]string, len(cmds))
	for i, c := range cmds {
		out[i] = nz(c)
	}
	return out
}

// MarshalJSON never emits null for the lists (check_commands null means "the default checks").
func (m MissionInput) MarshalJSON() ([]byte, error) {
	type alias MissionInput
	a := alias(m)
	a.CheckCommands = nzCommands(a.CheckCommands)
	a.GateEscalationSeconds = nz(a.GateEscalationSeconds)
	return json.Marshal(a)
}

// UnmarshalJSON applies the dataclass defaults.
func (m *MissionInput) UnmarshalJSON(data []byte) error {
	type alias MissionInput
	a := alias(NewMissionInput("", ""))
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = MissionInput(a)
	return nil
}

// CycleInput is one agent-cycle invocation.
type CycleInput struct {
	MissionID        string           `json:"mission_id"`
	Workdir          string           `json:"workdir"`
	CycleID          string           `json:"cycle_id"`
	CheckCommands    [][]string       `json:"check_commands"`
	BudgetUSD        *float64         `json:"budget_usd"`
	MaxCycles        int              `json:"max_cycles"`
	SteerNotes       []string         `json:"steer_notes"`
	ApprovedActions  []ApprovedAction `json:"approved_actions"`
	ResearchItem     *string          `json:"research_item"`
	ResearchBriefs   []string         `json:"research_briefs"`
	ResearchFailures []string         `json:"research_failures"`
}

// MarshalJSON never emits null for the lists.
func (c CycleInput) MarshalJSON() ([]byte, error) {
	type alias CycleInput
	a := alias(c)
	a.CheckCommands = nzCommands(a.CheckCommands)
	a.SteerNotes = nz(a.SteerNotes)
	a.ApprovedActions = nz(a.ApprovedActions)
	a.ResearchBriefs = nz(a.ResearchBriefs)
	a.ResearchFailures = nz(a.ResearchFailures)
	return json.Marshal(a)
}

// UnmarshalJSON applies the dataclass defaults.
func (c *CycleInput) UnmarshalJSON(data []byte) error {
	type alias CycleInput
	a := alias{MaxCycles: 1000}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*c = CycleInput(a)
	return nil
}

// CycleResult is the small, journaled result of one cycle (no raw transcripts). IsComplete =
// every item verified done; IsDeadlocked = nothing actionable but items remain.
type CycleResult struct {
	ItemID           *string           `json:"item_id"`
	Advanced         bool              `json:"advanced"`
	HeadSHA          string            `json:"head_sha"`
	IsComplete       bool              `json:"is_complete"`
	ItemsDone        int               `json:"items_done"`
	ItemsTotal       int               `json:"items_total"`
	Note             string            `json:"note"`
	Verdict          string            `json:"verdict"`
	IsDeadlocked     bool              `json:"is_deadlocked"`
	ItemBlocked      bool              `json:"item_blocked"`
	Reason           string            `json:"reason"`
	SpentUSD         float64           `json:"spent_usd"`
	ItemSplit        bool              `json:"item_split"`
	PendingApprovals []PendingApproval `json:"pending_approvals"`
	UsedApprovals    []string          `json:"used_approvals"`
	BaseSHA          string            `json:"base_sha"`
}

// MarshalJSON never emits null for the lists.
func (r CycleResult) MarshalJSON() ([]byte, error) {
	type alias CycleResult
	a := alias(r)
	a.PendingApprovals = nz(a.PendingApprovals)
	a.UsedApprovals = nz(a.UsedApprovals)
	return json.Marshal(a)
}

// HealthInput is a dependency health probe (and the read_mission_snapshot input).
type HealthInput struct {
	MissionID string `json:"mission_id"`
	Workdir   string `json:"workdir"`
}

// HealthReport is check_mission_health's result.
type HealthReport struct {
	Healthy  bool     `json:"healthy"`
	Reason   string   `json:"reason"`
	Degraded []string `json:"degraded"`
}

// MarshalJSON never emits null for degraded.
func (h HealthReport) MarshalJSON() ([]byte, error) {
	type alias HealthReport
	a := alias(h)
	a.Degraded = nz(a.Degraded)
	return json.Marshal(a)
}

// UnblockInput is a human-approved retry of blocked checklist items.
type UnblockInput struct {
	MissionID string `json:"mission_id"`
	Workdir   string `json:"workdir"`
	CycleID   string `json:"cycle_id"`
}

// MissionResult is the terminal mission summary.
type MissionResult struct {
	MissionID  string `json:"mission_id"`
	Completed  bool   `json:"completed"`
	Cycles     int    `json:"cycles"`
	HeadSHA    string `json:"head_sha"`
	ItemsDone  int    `json:"items_done"`
	ItemsTotal int    `json:"items_total"`
	Outcome    string `json:"outcome"` // one of the Outcome* constants
	Reason     string `json:"reason"`
	Status     string `json:"status"`
}

// UnmarshalJSON applies the dataclass defaults.
func (r *MissionResult) UnmarshalJSON(data []byte) error {
	type alias MissionResult
	a := alias{Outcome: OutcomeCompleted}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = MissionResult(a)
	return nil
}

// SubAgentInput is the input to a durable sub-agent child workflow.
type SubAgentInput struct {
	RoleName    string   `json:"role_name"`
	Objective   string   `json:"objective"`
	Workdir     string   `json:"workdir"`
	MissionID   string   `json:"mission_id"`
	AllowEgress bool     `json:"allow_egress"`
	BudgetUSD   *float64 `json:"budget_usd"`
	CycleID     string   `json:"cycle_id"`
}

// SubAgentOutput is the condensed artifact a sub-agent returns.
type SubAgentOutput struct {
	Role      string `json:"role"`
	Brief     string `json:"brief"`
	ToolCalls int    `json:"tool_calls"`
	Turns     int    `json:"turns"`
}

// FanOutResult is the outcome of a sub-agent fan-out: every success AND every failure.
type FanOutResult struct {
	Outputs  []SubAgentOutput `json:"outputs"`
	Failures []string         `json:"failures"`
}

// MarshalJSON never emits null for the lists.
func (f FanOutResult) MarshalJSON() ([]byte, error) {
	type alias FanOutResult
	a := alias(f)
	a.Outputs = nz(a.Outputs)
	a.Failures = nz(a.Failures)
	return json.Marshal(a)
}

// --- the durable organization (research / review / parallel waves) ------------------------

// RoundInput plans the next round (plan_round): which item(s) and whether they form a wave.
type RoundInput struct {
	MissionID   string `json:"mission_id"`
	Workdir     string `json:"workdir"`
	MaxParallel int    `json:"max_parallel"`
}

// RoundItem is one item of a round.
type RoundItem struct {
	ItemID      string `json:"item_id"`
	Description string `json:"description"`
}

// RoundPlan is the committed truth at the start of a round and the round's items (checklist
// order). Parallel: the items form a wave (each owns a disjoint write-set); otherwise Items is the
// next actionable item (or empty when nothing is actionable).
type RoundPlan struct {
	HeadSHA      string      `json:"head_sha"`
	Items        []RoundItem `json:"items"`
	Parallel     bool        `json:"parallel"`
	IsComplete   bool        `json:"is_complete"`
	IsDeadlocked bool        `json:"is_deadlocked"`
}

// MarshalJSON never emits null for items.
func (p RoundPlan) MarshalJSON() ([]byte, error) {
	type alias RoundPlan
	a := alias(p)
	a.Items = nz(a.Items)
	return json.Marshal(a)
}

// ImplementerInput is one parallel implementer (run_implementer): an item, in its own worktree
// at BaseSHA.
type ImplementerInput struct {
	MissionID        string           `json:"mission_id"`
	Workdir          string           `json:"workdir"`
	CycleID          string           `json:"cycle_id"`
	ItemID           string           `json:"item_id"`
	BaseSHA          string           `json:"base_sha"`
	CheckCommands    [][]string       `json:"check_commands"`
	BudgetUSD        *float64         `json:"budget_usd"`
	MaxCycles        int              `json:"max_cycles"`
	SteerNotes       []string         `json:"steer_notes"`
	ApprovedActions  []ApprovedAction `json:"approved_actions"`
	ResearchBriefs   []string         `json:"research_briefs"`
	ResearchFailures []string         `json:"research_failures"`
}

// MarshalJSON never emits null for the lists.
func (i ImplementerInput) MarshalJSON() ([]byte, error) {
	type alias ImplementerInput
	a := alias(i)
	a.CheckCommands = nzCommands(a.CheckCommands)
	a.SteerNotes = nz(a.SteerNotes)
	a.ApprovedActions = nz(a.ApprovedActions)
	a.ResearchBriefs = nz(a.ResearchBriefs)
	a.ResearchFailures = nz(a.ResearchFailures)
	return json.Marshal(a)
}

// UnmarshalJSON applies the dataclass defaults.
func (i *ImplementerInput) UnmarshalJSON(data []byte) error {
	type alias ImplementerInput
	a := alias{MaxCycles: 1000}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*i = ImplementerInput(a)
	return nil
}

// ImplementerOutput is what an implementer left on its branch (JSON strings carry the rich
// models, as pydantic's model_dump_json writes them).
type ImplementerOutput struct {
	ItemID           string              `json:"item_id"`
	CycleID          string              `json:"cycle_id"`
	Branch           string              `json:"branch"`
	Head             string              `json:"head"`
	Brief            string              `json:"brief"`
	ToolCalls        int                 `json:"tool_calls"`
	Error            string              `json:"error"`
	VerificationJSON string              `json:"verification_json"` // VerificationResult
	DecisionsJSON    []string            `json:"decisions_json"`    // DecisionRecord each
	TicketJSON       string              `json:"ticket_json"`       // Ticket
	TicketHistory    []map[string]string `json:"ticket_history"`
	Leases           []string            `json:"leases"` // LeaseDecision each
	SpentUSD         float64             `json:"spent_usd"`
	PendingApprovals []PendingApproval   `json:"pending_approvals"`
	UsedApprovals    []string            `json:"used_approvals"`
}

// MarshalJSON never emits null for the lists.
func (o ImplementerOutput) MarshalJSON() ([]byte, error) {
	type alias ImplementerOutput
	a := alias(o)
	a.DecisionsJSON = nz(a.DecisionsJSON)
	a.TicketHistory = nz(a.TicketHistory)
	a.Leases = nz(a.Leases)
	a.PendingApprovals = nz(a.PendingApprovals)
	a.UsedApprovals = nz(a.UsedApprovals)
	return json.Marshal(a)
}

// IntegrateInput integrates one implementer's branch (integrate_branch) and commits the
// checkpoint.
type IntegrateInput struct {
	MissionID        string             `json:"mission_id"`
	Workdir          string             `json:"workdir"`
	CycleID          string             `json:"cycle_id"`
	ItemID           string             `json:"item_id"`
	BaseSHA          string             `json:"base_sha"`
	Output           *ImplementerOutput `json:"output"`
	Error            string             `json:"error"` // the implementer activity failed (no output)
	CheckCommands    [][]string         `json:"check_commands"`
	BudgetUSD        *float64           `json:"budget_usd"`
	MaxCycles        int                `json:"max_cycles"`
	ResearchBriefs   int                `json:"research_briefs"`
	ResearchFailures []string           `json:"research_failures"`
}

// MarshalJSON never emits null for the lists.
func (i IntegrateInput) MarshalJSON() ([]byte, error) {
	type alias IntegrateInput
	a := alias(i)
	a.CheckCommands = nzCommands(a.CheckCommands)
	a.ResearchFailures = nz(a.ResearchFailures)
	return json.Marshal(a)
}

// UnmarshalJSON applies the dataclass defaults.
func (i *IntegrateInput) UnmarshalJSON(data []byte) error {
	type alias IntegrateInput
	a := alias{MaxCycles: 1000}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*i = IntegrateInput(a)
	return nil
}

// ReviewInput is the independent review (review_cycle) of one verified item's base..head diff.
type ReviewInput struct {
	MissionID string `json:"mission_id"`
	Workdir   string `json:"workdir"`
	// CycleID is the reviewed cycle's id; the review commits as "<cycle_id>-review".
	CycleID string `json:"cycle_id"`
	ItemID  string `json:"item_id"`
	HeadSHA string `json:"head_sha"`
	// BaseSHA "" = the head commit's first parent.
	BaseSHA   string   `json:"base_sha"`
	BudgetUSD *float64 `json:"budget_usd"`
	MaxCycles int      `json:"max_cycles"`
}

// UnmarshalJSON applies the dataclass defaults.
func (r *ReviewInput) UnmarshalJSON(data []byte) error {
	type alias ReviewInput
	a := alias{MaxCycles: 1000}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = ReviewInput(a)
	return nil
}

func strPtr(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
