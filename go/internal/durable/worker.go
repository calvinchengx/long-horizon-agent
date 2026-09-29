package durable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
)

// Worker wiring (python: lha.durable.worker) and the cross-language guard.
//
// The Go and Python Temporal SDKs assign timer and activity sequence ids differently, so one
// workflow execution cannot be served by workers of both languages: a workflow task that lands on
// the other implementation's worker fails to replay the history. Each implementation therefore
// marks its worker identity (IdentityMarker "lha-go" here, "lha-py" in Python) and, before it
// polls, asks the server who polls the task queue (DescribeTaskQueue); a poller of the other
// implementation makes the worker refuse to start (fail closed). Temporal lists a poller for a
// few minutes after it stopped, so switching a queue from one implementation to the other means
// waiting for the old pollers to age out, or using another LHA_TASK_QUEUE.
//
// Two workers of different implementations started at the same moment can both pass that
// startup check, so a running worker re-checks the pollers every LHA_WORKER_GUARD_INTERVAL_S
// seconds (GuardTaskQueue, RunGuarded) and, when a poller of the other implementation appears,
// stops polling (a graceful worker stop) and exits non-zero with the same message (fail closed:
// in such a race both stop).

// Identity markers of the two implementations' workers.
const (
	IdentityMarker       = "lha-go"
	PythonIdentityMarker = "lha-py"
)

// WorkerIdentity is this process's worker identity: "lha-go:<pid>@<host>" (Python's is
// "lha-py:<pid>@<host>"; the SDK default is "<pid>@<host>").
func WorkerIdentity() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s:%d@%s", IdentityMarker, os.Getpid(), host)
}

// MixedWorkersError: the task queue is already polled by the other implementation's workers.
type MixedWorkersError struct {
	TaskQueue string
	Identity  string
}

func (e *MixedWorkersError) Error() string {
	return fmt.Sprintf("task queue %s is already polled by a Python lha worker (%s). A Go and a Python worker "+
		"cannot serve the same missions: their Temporal SDKs number timers and activities differently, so a "+
		"history recorded by one does not replay on the other. Stop the Python workers (Temporal lists a poller "+
		"for a few minutes after it stops), or start this worker on another queue, e.g. LHA_TASK_QUEUE=%s-go "+
		"(and start its missions with the same LHA_TASK_QUEUE)",
		"'"+e.TaskQueue+"'", e.Identity, e.TaskQueue)
}

// TaskQueueDescriber is the slice of client.Client the guard needs.
type TaskQueueDescriber interface {
	DescribeTaskQueue(ctx context.Context, taskQueue string, taskQueueType enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error)
}

// CheckTaskQueuePollers fails with *MixedWorkersError when a poller of taskQueue (workflow or
// activity tasks) carries the other implementation's identity marker.
func CheckTaskQueuePollers(ctx context.Context, c TaskQueueDescriber, taskQueue string) error {
	for _, kind := range []enumspb.TaskQueueType{enumspb.TASK_QUEUE_TYPE_WORKFLOW, enumspb.TASK_QUEUE_TYPE_ACTIVITY} {
		resp, err := c.DescribeTaskQueue(ctx, taskQueue, kind)
		if err != nil {
			return fmt.Errorf("cannot check who polls task queue %s: %w", "'"+taskQueue+"'", err)
		}
		var identities []string
		for _, p := range resp.GetPollers() {
			identities = append(identities, p.GetIdentity())
		}
		sort.Strings(identities)
		for _, id := range identities {
			if strings.Contains(id, PythonIdentityMarker) {
				return &MixedWorkersError{TaskQueue: taskQueue, Identity: id}
			}
		}
	}
	return nil
}

// GuardTaskQueue re-checks the pollers of taskQueue every interval until ctx ends (nil) or a
// poller of the other implementation appears (*MixedWorkersError). A failed check (the server
// briefly unreachable) goes to onError and is retried at the next interval: the worker already
// passed the fail-closed startup check.
func GuardTaskQueue(ctx context.Context, c TaskQueueDescriber, taskQueue string, interval time.Duration, onError func(error)) error {
	if interval <= 0 { // a huge LHA_WORKER_GUARD_INTERVAL_S overflowed time.Duration
		interval = math.MaxInt64
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		err := CheckTaskQueuePollers(ctx, c, taskQueue)
		var mixed *MixedWorkersError
		switch {
		case errors.As(err, &mixed):
			return err
		case err != nil && ctx.Err() == nil && onError != nil:
			onError(err)
		}
	}
}

// RunGuarded runs w until ctx ends, the worker fails, or GuardTaskQueue sees a poller of the
// other implementation on taskQueue: then w stops polling and shuts down (worker.Stop) and the
// *MixedWorkersError is returned.
func RunGuarded(ctx context.Context, w worker.Worker, c TaskQueueDescriber, taskQueue string, interval time.Duration, onError func(error)) error {
	return RunGuardedWith(ctx, w, c, taskQueue, interval, onError, nil)
}

// RunGuardedWith is RunGuarded that also runs onStart once the worker runs (e.g.
// AnnounceVersion); an error from onStart stops the worker and is returned.
func RunGuardedWith(ctx context.Context, w worker.Worker, c TaskQueueDescriber, taskQueue string, interval time.Duration, onError func(error), onStart func(context.Context) error) error {
	guardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan any)
	ran := make(chan error, 1)
	go func() { ran <- w.Run(stop) }()
	guarded := make(chan error, 1)
	go func() {
		if onStart != nil {
			if err := onStart(guardCtx); err != nil && guardCtx.Err() == nil {
				guarded <- err
				return
			}
		}
		guarded <- GuardTaskQueue(guardCtx, c, taskQueue, interval, onError)
	}()
	select {
	case err := <-ran: // the worker failed to start or failed fatally
		cancel()
		<-guarded
		return err
	case err := <-guarded: // ctx ended (nil) or the guard tripped
		close(stop)
		if runErr := <-ran; err == nil {
			return runErr
		}
		return err
	}
}

// Logger is a Temporal SDK logger writing to w at level (the SDK's default logs to stdout).
func Logger(w io.Writer, level slog.Level) tlog.Logger {
	return tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
}

// Dial connects to Temporal (LHA_TEMPORAL_ADDRESS, LHA_TEMPORAL_NAMESPACE) with the ClaimCheck
// data converter over LHA_OBJECT_STORE_ROOT (python: connect_client).
func Dial(ctx context.Context, settings *config.Settings, logger tlog.Logger) (client.Client, error) {
	dc, err := NewDataConverter(settings.ObjectStoreRoot)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = Logger(io.Discard, slog.LevelError)
	}
	return client.DialContext(ctx, client.Options{
		HostPort:      settings.TemporalAddress,
		Namespace:     settings.TemporalNamespace,
		DataConverter: dc,
		Logger:        logger,
	})
}

// Register registers the mission + sub-agent workflows and every activity under their Python
// names.
func Register(r worker.Registry, acts *Activities) { registerWith(r, acts, nil) }

// registerWith is Register with some activities replaced by name (tests).
func registerWith(r worker.Registry, acts *Activities, overrides map[string]any) {
	r.RegisterWorkflowWithOptions(MissionWorkflow, workflow.RegisterOptions{Name: WorkflowMission})
	r.RegisterWorkflowWithOptions(SubAgentWorkflow, workflow.RegisterOptions{Name: WorkflowSubAgent})
	fns := map[string]any{
		ActivityRunAgentCycle:       acts.RunAgentCycle,
		ActivityCheckMissionHealth:  acts.CheckMissionHealth,
		ActivityNotifyGate:          acts.NotifyGate,
		ActivityDeclareImpossible:   acts.DeclareImpossible,
		ActivityUnblockItems:        acts.UnblockItems,
		ActivityReadMissionSnapshot: acts.ReadMissionSnapshot,
		ActivityRecordMissionStatus: acts.RecordMissionStatus,
		ActivityRunSubAgent:         acts.RunSubAgent,
		ActivityPlanRound:           acts.PlanRound,
		ActivityRunImplementer:      acts.RunImplementer,
		ActivityIntegrateBranch:     acts.IntegrateBranch,
		ActivityReviewCycle:         acts.ReviewCycle,
	}
	for name, fn := range overrides {
		fns[name] = fn
	}
	for name, fn := range fns {
		r.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
}

// NewWorker is a worker on taskQueue, identified as an lha-go worker, hosting every durable
// workflow and activity (python: build_worker).
func NewWorker(c client.Client, taskQueue string, acts *Activities) worker.Worker {
	return NewVersionedWorker(c, taskQueue, acts, worker.DeploymentOptions{})
}

// NewVersionedWorker is NewWorker polling as one build of a worker deployment (DeploymentOptions;
// the zero value is unversioned).
func NewVersionedWorker(c client.Client, taskQueue string, acts *Activities, deployment worker.DeploymentOptions) worker.Worker {
	w := worker.New(c, taskQueue, worker.Options{Identity: WorkerIdentity(), DeploymentOptions: deployment})
	Register(w, acts)
	return w
}

// NewReplayer is a replayer for every durable workflow with the worker's data converter (python:
// replay_test_harness.build_replayer).
func NewReplayer(dc converter.DataConverter) (worker.WorkflowReplayer, error) {
	r, err := worker.NewWorkflowReplayerWithOptions(worker.WorkflowReplayerOptions{DataConverter: dc})
	if err != nil {
		return nil, err
	}
	r.RegisterWorkflowWithOptions(MissionWorkflow, workflow.RegisterOptions{Name: WorkflowMission})
	r.RegisterWorkflowWithOptions(SubAgentWorkflow, workflow.RegisterOptions{Name: WorkflowSubAgent})
	return r, nil
}

// ReplayHistories replays every *.json history in dir; it fails on the first replay failure and
// returns how many replayed (python: replay_histories).
func ReplayHistories(dir string, dc converter.DataConverter, logger tlog.Logger) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0, err
	}
	sort.Strings(paths)
	r, err := NewReplayer(dc)
	if err != nil {
		return 0, err
	}
	for i, path := range paths {
		if err := r.ReplayWorkflowHistoryFromJSONFile(logger, path); err != nil {
			return i, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
	return len(paths), nil
}
