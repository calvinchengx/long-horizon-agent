package durable

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	deploymentpb "go.temporal.io/api/deployment/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/grpc"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
)

// fakeDeployments answers DescribeWorkerDeployment with current (or describeErr) and fails
// SetWorkerDeploymentCurrentVersion with setErrs in order, then records the promotion.
type fakeDeployments struct {
	current     string
	describeErr error
	setErrs     []error
	promoted    []string
}

func (f *fakeDeployments) DescribeWorkerDeployment(context.Context, *workflowservice.DescribeWorkerDeploymentRequest, ...grpc.CallOption) (*workflowservice.DescribeWorkerDeploymentResponse, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &workflowservice.DescribeWorkerDeploymentResponse{ConflictToken: []byte("t"),
		WorkerDeploymentInfo: &deploymentpb.WorkerDeploymentInfo{RoutingConfig: &deploymentpb.RoutingConfig{
			CurrentDeploymentVersion: &deploymentpb.WorkerDeploymentVersion{BuildId: f.current}}}}, nil
}

func (f *fakeDeployments) SetWorkerDeploymentCurrentVersion(_ context.Context, in *workflowservice.SetWorkerDeploymentCurrentVersionRequest, _ ...grpc.CallOption) (*workflowservice.SetWorkerDeploymentCurrentVersionResponse, error) {
	if len(f.setErrs) > 0 {
		err := f.setErrs[0]
		f.setErrs = f.setErrs[1:]
		return nil, err
	}
	f.promoted = append(f.promoted, in.GetBuildId()+"@"+string(in.GetConflictToken()))
	f.current = in.GetBuildId()
	return &workflowservice.SetWorkerDeploymentCurrentVersionResponse{}, nil
}

func TestDeploymentOptionsFollowTheSettings(t *testing.T) {
	load := func(env ...string) *config.Settings {
		s, err := config.LoadFrom(env, "")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, ok, err := DeploymentOptions(load()); ok || err != nil {
		t.Fatal(ok, err)
	}
	opts, ok, err := DeploymentOptions(load("LHA_WORKER_DEPLOYMENT=lha", "LHA_WORKER_BUILD_ID=b7"))
	if !ok || err != nil || !opts.UseVersioning || opts.Version.DeploymentName != "lha" || opts.Version.BuildID != "b7" ||
		opts.DefaultVersioningBehavior != workflow.VersioningBehaviorPinned {
		t.Fatalf("%+v %v %v", opts, ok, err)
	}
	opts, _, _ = DeploymentOptions(load("LHA_WORKER_DEPLOYMENT=lha", "LHA_WORKER_BUILD_ID=b7", "LHA_WORKER_VERSIONING_BEHAVIOR=auto_upgrade"))
	if opts.DefaultVersioningBehavior != workflow.VersioningBehaviorAutoUpgrade {
		t.Fatalf("%+v", opts)
	}
	if _, _, err := DeploymentOptions(load("LHA_WORKER_BUILD_ID=b7")); err == nil {
		t.Fatal("a half-set deployment was accepted")
	}
}

func TestAnnounceVersionSaysWhereNewMissionsStart(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	for _, f := range []*fakeDeployments{
		{describeErr: serviceerror.NewNotFound("no deployment")},
		{},
		{current: "b1"},
		{current: "b2"},
		{describeErr: serviceerror.NewPermissionDenied("denied", "")},
	} {
		if err := AnnounceVersion(context.Background(), f, "default", "lha", "b2", false, log); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	command := "temporal worker deployment set-current-version --deployment-name lha --build-id b2"
	if len(lines) != 4 || !strings.Contains(lines[0], "no current version") || !strings.Contains(lines[0], command) ||
		!strings.Contains(lines[1], command) || !strings.Contains(lines[2], "new missions start on build 'b1'") ||
		!strings.Contains(lines[3], "cannot read worker deployment 'lha'") {
		t.Fatalf("%q", lines)
	}
}

func TestPromotionRetriesUntilTheServerTakesIt(t *testing.T) {
	f := &fakeDeployments{setErrs: []error{
		serviceerror.NewNotFound("the build has not polled yet"),
		serviceerror.NewResourceExhausted(0, "too many requests"),
	}}
	if err := PromoteBuild(context.Background(), f, "default", "lha", "b2", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.promoted, ",") != "b2@t" || len(f.setErrs) != 0 {
		t.Fatal(f.promoted)
	}
	if err := PromoteBuild(context.Background(), f, "default", "lha", "b2", 0); err != nil || len(f.promoted) != 1 {
		t.Fatal(err, f.promoted) // already current
	}
	var buf bytes.Buffer
	denied := &fakeDeployments{describeErr: serviceerror.NewPermissionDenied("denied", "")}
	err := AnnounceVersion(context.Background(), denied, "default", "lha", "b1", true, slog.New(slog.NewTextHandler(&buf, nil)))
	if err == nil || !strings.Contains(err.Error(), "Set it by hand: temporal worker deployment set-current-version --deployment-name lha --build-id b1") {
		t.Fatal(err)
	}
}
