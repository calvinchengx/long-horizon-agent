//go:build unix

package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
)

// Worker versioning (python: tests/durability/test_worker_versioning.py): a mission stays on the
// worker build that started it when a newer build becomes the deployment's current version.
func TestMissionsStayOnTheBuildThatStartedThem(t *testing.T) {
	addr := temporalAddress(t)
	dir := t.TempDir()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	queue, deployment := "lha-it-versions-"+suffix, "lha-it-"+suffix
	settings, err := config.LoadFrom(durableEnv(t, addr, dir, queue), "")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := durable.Dial(context.Background(), settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pinned := func(id string) (enumspb.VersioningBehavior, string) {
		resp, err := cl.DescribeWorkflowExecution(ctx, id, "")
		if err != nil {
			return 0, ""
		}
		info := resp.GetWorkflowExecutionInfo().GetVersioningInfo()
		return info.GetBehavior(), info.GetDeploymentVersion().GetBuildId()
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); !ok(); time.Sleep(500 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	started := []string{}
	defer func() {
		for _, id := range started {
			_ = cl.TerminateWorkflow(context.Background(), id, "", "test done")
		}
	}()
	errs := make(chan error, 2)
	for _, build := range []string{"b1", "b2"} {
		opts := worker.DeploymentOptions{UseVersioning: true, DefaultVersioningBehavior: workflow.VersioningBehaviorPinned,
			Version: worker.WorkerDeploymentVersion{DeploymentName: deployment, BuildID: build}}
		w := durable.NewVersionedWorker(cl, queue, &durable.Activities{Settings: settings, OpenToolbox: openToolbox}, opts)
		go func() {
			errs <- durable.RunGuardedWith(ctx, w, cl, queue, time.Hour, nil, func(ctx context.Context) error {
				return durable.AnnounceVersion(ctx, cl.WorkflowService(), settings.TemporalNamespace, deployment, build, true, log)
			})
		}()
		eventually("build "+build+" to become current", func() bool {
			current, _ := durable.CurrentBuildID(ctx, cl.WorkflowService(), settings.TemporalNamespace, deployment)
			return current == build
		})
		// A mission whose workdir has no anchor: its first activity fails and retries, but its
		// first workflow task already pins it to the build that ran it.
		id := "mission:" + build + "-" + suffix
		inp := durable.MissionInput{MissionID: id[len("mission:"):], Workdir: filepath.Join(dir, "missing"), MaxCycles: 50}
		if _, err := cl.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, durable.WorkflowMission, inp); err != nil {
			t.Fatal(err)
		}
		started = append(started, id)
		eventually(id+" pinned to "+build, func() bool {
			behavior, got := pinned(id)
			return behavior == enumspb.VERSIONING_BEHAVIOR_PINNED && got == build
		})
	}
	if behavior, build := pinned(started[0]); behavior != enumspb.VERSIONING_BEHAVIOR_PINNED || build != "b1" {
		t.Fatalf("the first mission moved: %v %s", behavior, build)
	}
	select {
	case err := <-errs:
		t.Fatalf("a worker stopped: %v", err)
	default:
	}
}
