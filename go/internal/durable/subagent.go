package durable

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// SubAgentWorkflow runs one sub-agent durably (python: lha.durable.subagent_workflow): its body
// is a single run_subagent activity, so a sub-agent is independently retried and its result
// journaled. Registered under the Python name so a Python MissionWorkflow's children and a Go
// worker agree; the activity itself is not yet available in Go (Activities.SubAgent).
func SubAgentWorkflow(ctx workflow.Context, inp SubAgentInput) (SubAgentOutput, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			NonRetryableErrorTypes: []string{ErrorBudgetExceeded, ErrorConfig},
		},
	})
	var out SubAgentOutput
	err := workflow.ExecuteActivity(actx, ActivityRunSubAgent, inp).Get(ctx, &out)
	return out, err
}
