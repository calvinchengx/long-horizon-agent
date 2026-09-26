// Package durable is the Temporal control plane of a mission (python/src/lha/durable): the
// MissionWorkflow scheduler, the SubAgentWorkflow, their activities, the ClaimCheck payload codec
// and the worker wiring.
//
// Every name and payload shape crossing Temporal is the Python implementation's (docs/19): the
// workflow type names, activity names, signal and query names, the task queue setting and the
// JSON field names of the payload types. A Python client can start, query, signal and cancel a
// mission served by a Go worker, and the reverse. What cannot be shared is ONE workflow execution:
// the Go and Python SDKs number timers and activities differently, so a history recorded by one
// cannot be replayed by the other. `lha worker` therefore refuses to poll a task queue another
// implementation already polls (CheckTaskQueuePollers).
//
// Versioning: Python guards behaviour added after histories were recorded with
// workflow.patched(). Go histories never replay Python histories, so the Go workflow starts from
// the latest Python behaviour with no patch branches. A Go behaviour change after this point is
// guarded with workflow.GetVersion(ctx, "lha-go-<change>-v<n>", workflow.DefaultVersion, <n>),
// and the recorded Go histories in testdata/histories must keep replaying (replay_test.go).
package durable

// Workflow type names (python: the @workflow.defn class names).
const (
	WorkflowMission  = "MissionWorkflow"
	WorkflowSubAgent = "SubAgentWorkflow"
)

// Activity names (python: the @activity.defn function names).
const (
	ActivityRunAgentCycle       = "run_agent_cycle"
	ActivityCheckMissionHealth  = "check_mission_health"
	ActivityNotifyGate          = "notify_gate"
	ActivityDeclareImpossible   = "declare_impossible"
	ActivityUnblockItems        = "unblock_items"
	ActivityReadMissionSnapshot = "read_mission_snapshot"
	ActivityRecordMissionStatus = "record_mission_status"
	ActivityRunSubAgent         = "run_subagent"
)

// Signals and queries (python: lha.durable.signals and the MissionWorkflow query methods).
const (
	SignalHumanDecision = "human_decision_v1"
	SignalSteer         = "steer_v1"
	SignalSnooze        = "snooze_v1"

	QueryStatus            = "status_v1"
	QueryCycles            = "cycles_done"
	QueryGate              = "gate_v1"
	QueryGateLog           = "gate_log_v1"
	QueryLastItem          = "last_item"
	QueryParkReason        = "park_reason"
	QueryResumeAt          = "resume_at"
	QueryOpenQuestion      = "open_question"
	QueryRejectedDecisions = "rejected_decisions"

	UpdateVerifyVerdict = "verify_verdict_v1" // constant only; no handler exists (as in Python)
)

// Mission status values (the text column missions.status).
const (
	StatusRunning        = "RUNNING"
	StatusSleeping       = "SLEEPING"         // on a durable timer by design (pause / scheduled start / snooze)
	StatusWaitingOnHuman = "WAITING_ON_HUMAN" // a gate is open
	StatusDegradedPark   = "DEGRADED_PARK"    // a critical dependency is down
	StatusDone           = "DONE"
	StatusAborted        = "ABORTED"
	StatusImpossible     = "IMPOSSIBLE"
)

// Gate kinds and options.
const (
	GateToolCall = "tool_call"
	GateDeadlock = "deadlock"
)

// ApprovalOptions are a tool-call gate's options; DeadlockOptions the deadlock gate's;
// DeadlockDefaults the deadlock gate's allowed unattended defaults ("retry" never is one).
var (
	ApprovalOptions  = []string{"approve", "reject"}
	DeadlockOptions  = []string{"retry", "abort", "impossible"}
	DeadlockDefaults = []string{"abort", "impossible"}
)

// Mission outcomes reported in MissionResult.Outcome.
const (
	OutcomeCompleted       = "completed"
	OutcomeDeadlocked      = "deadlocked"
	OutcomeBudgetExhausted = "budget_exhausted"
	OutcomeMaxCycles       = "max_cycles"
	OutcomeAborted         = "aborted"
	OutcomeImpossible      = "impossible" // a human (or the deadlock gate's default) declared it
)

// Bounds of the durable organization (MissionInput); the organization itself is not yet ported.
const (
	MaxResearchPerItem = 4
	MaxParallel        = 8
)

// ApplicationError type values raised (non-retryable) by the activities.
const (
	ErrorBudgetExceeded = "BudgetExceeded"
	ErrorConfig         = "MissionConfigError"
)

// MissionWorkflowID is the workflow id of a mission ("mission:<mission_id>").
func MissionWorkflowID(missionID string) string { return "mission:" + missionID }
