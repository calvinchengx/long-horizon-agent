package durable

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
)

// Worker versioning (python: lha.durable.worker deployment_config / promote_build /
// announce_version). With LHA_WORKER_DEPLOYMENT + LHA_WORKER_BUILD_ID the worker polls as one
// build of a Temporal Worker Deployment. Temporal starts new missions on the deployment's current
// version and, with the default "pinned" behaviour, keeps each mission (and its Continue-As-New
// runs and sub-agent children) on the build that started it, so a deploy never replays an
// in-flight mission on changed workflow code; old workers serve their missions until they finish.
// LHA_WORKER_PROMOTE makes the new build current once it polls.

// PromoteTimeout is how long PromoteBuild waits for the server to register the new build.
var PromoteTimeout = 60 * time.Second

// DeploymentOptions is the worker's deployment version per settings (ok false: unversioned); an
// error for a half-configured deployment (config.Settings.WorkerDeploymentVersion).
func DeploymentOptions(s *config.Settings) (opts worker.DeploymentOptions, ok bool, err error) {
	name, build, ok, err := s.WorkerDeploymentVersion()
	if err != nil || !ok {
		return opts, false, err
	}
	behavior := workflow.VersioningBehaviorPinned
	if s.WorkerVersioningBehavior == "auto_upgrade" {
		behavior = workflow.VersioningBehaviorAutoUpgrade
	}
	return worker.DeploymentOptions{
		UseVersioning:             true,
		Version:                   worker.WorkerDeploymentVersion{DeploymentName: name, BuildID: build},
		DefaultVersioningBehavior: behavior,
	}, true, nil
}

// DeploymentService is the slice of the workflow service that versioning needs.
type DeploymentService interface {
	DescribeWorkerDeployment(ctx context.Context, in *workflowservice.DescribeWorkerDeploymentRequest, opts ...grpc.CallOption) (*workflowservice.DescribeWorkerDeploymentResponse, error)
	SetWorkerDeploymentCurrentVersion(ctx context.Context, in *workflowservice.SetWorkerDeploymentCurrentVersionRequest, opts ...grpc.CallOption) (*workflowservice.SetWorkerDeploymentCurrentVersionResponse, error)
}

func setCurrentCommand(deployment, buildID string) string {
	return fmt.Sprintf("temporal worker deployment set-current-version --deployment-name %s --build-id %s", deployment, buildID)
}

func describeDeployment(ctx context.Context, svc DeploymentService, namespace, deployment string) (*workflowservice.DescribeWorkerDeploymentResponse, error) {
	return svc.DescribeWorkerDeployment(ctx, &workflowservice.DescribeWorkerDeploymentRequest{Namespace: namespace, DeploymentName: deployment})
}

// CurrentBuildID is the build id of deployment's current version ("" when there is none yet, or
// no such deployment: new missions then wait for one).
func CurrentBuildID(ctx context.Context, svc DeploymentService, namespace, deployment string) (string, error) {
	resp, err := describeDeployment(ctx, svc, namespace, deployment)
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return resp.GetWorkerDeploymentInfo().GetRoutingConfig().GetCurrentDeploymentVersion().GetBuildId(), nil
}

// retryablePromoteError: the build has not polled yet, a concurrent change moved the conflict
// token, the server rate-limits one deployment's calls, or it is briefly unreachable.
func retryablePromoteError(err error) bool {
	var (
		notFound     *serviceerror.NotFound
		precondition *serviceerror.FailedPrecondition
		exhausted    *serviceerror.ResourceExhausted
		unavailable  *serviceerror.Unavailable
	)
	return errors.As(err, &notFound) || errors.As(err, &precondition) || errors.As(err, &exhausted) ||
		errors.As(err, &unavailable)
}

// PromoteBuild makes buildID the current version of deployment: new missions start on it, while
// missions pinned to older builds stay there. The server registers a build at its worker's first
// poll, so this retries (with backoff) until it knows the build or timeout passes.
func PromoteBuild(ctx context.Context, svc DeploymentService, namespace, deployment, buildID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 500 * time.Millisecond
	for {
		resp, err := describeDeployment(ctx, svc, namespace, deployment)
		if err == nil {
			if resp.GetWorkerDeploymentInfo().GetRoutingConfig().GetCurrentDeploymentVersion().GetBuildId() == buildID {
				return nil
			}
			_, err = svc.SetWorkerDeploymentCurrentVersion(ctx, &workflowservice.SetWorkerDeploymentCurrentVersionRequest{
				Namespace: namespace, DeploymentName: deployment, BuildId: buildID,
				ConflictToken: resp.GetConflictToken(), Identity: WorkerIdentity(),
			})
			if err == nil {
				return nil
			}
		}
		if !retryablePromoteError(err) || time.Now().After(deadline) {
			return fmt.Errorf("cannot make build '%s' the current version of worker deployment '%s': %v. Set it by hand: %s",
				buildID, deployment, err, setCurrentCommand(deployment, buildID))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// AnnounceVersion promotes this build (promote; an error when that fails), or says which build new
// missions start on: with no current version, they wait until one is set.
func AnnounceVersion(ctx context.Context, svc DeploymentService, namespace, deployment, buildID string, promote bool, log *slog.Logger) error {
	if promote {
		if err := PromoteBuild(ctx, svc, namespace, deployment, buildID, PromoteTimeout); err != nil {
			return err
		}
		log.Info(fmt.Sprintf("build '%s' is the current version of worker deployment '%s'", buildID, deployment))
		return nil
	}
	current, err := CurrentBuildID(ctx, svc, namespace, deployment)
	switch {
	case err != nil: // only informational: the worker serves its pinned missions regardless
		log.Warn(fmt.Sprintf("cannot read worker deployment '%s': %v", deployment, err))
	case current == "":
		log.Warn(fmt.Sprintf("worker deployment '%s' has no current version: new missions wait until one is set, e.g. %s (or LHA_WORKER_PROMOTE=true)",
			deployment, setCurrentCommand(deployment, buildID)))
	case current != buildID:
		log.Info(fmt.Sprintf("worker deployment '%s': new missions start on build '%s'; this build ('%s') serves the missions pinned to it",
			deployment, current, buildID))
	}
	return nil
}
