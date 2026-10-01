//go:build unix

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// Cross-language durable tests against a real Temporal server: a mission started and served by
// Go is queried, signalled and aborted with the Python CLI, a mission served by a Python worker is
// driven with the Go CLI, and each implementation's worker refuses a task queue the other
// polls. They need uv and a Temporal server: LHA_IT_TEMPORAL_ADDRESS (an existing server), else
// `temporal server start-dev` when the temporal CLI is on PATH, else the test server the Python
// SDK caches for its durability tests; otherwise they are skipped.

func temporalAddress(t *testing.T) string {
	t.Helper()
	if !pythonAvailable(t) {
		t.Skip("uv not on PATH")
	}
	if testing.Short() {
		t.Skip("cross-language Temporal tests are slow")
	}
	if addr := os.Getenv("LHA_IT_TEMPORAL_ADDRESS"); addr != "" {
		return addr
	}
	port := freePort(t)
	var cmd *exec.Cmd
	if bin, err := exec.LookPath("temporal"); err == nil {
		cmd = exec.Command(bin, "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(port),
			"--ui-port", strconv.Itoa(freePort(t)), "--http-port", strconv.Itoa(freePort(t)), "--metrics-port", strconv.Itoa(freePort(t)),
			"--db-filename", filepath.Join(t.TempDir(), "temporal.db"), "--log-level", "error")
	} else if bin := cachedTestServer(); bin != "" {
		cmd = exec.Command(bin, strconv.Itoa(port)) // the Python SDK's test server (normal time)
	} else {
		t.Skip("no Temporal server: set LHA_IT_TEMPORAL_ADDRESS, put the temporal CLI on PATH, or run the Python " +
			"durability tests once (they cache Temporal's test server)")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(60 * time.Second)
	for {
		cl, err := client.Dial(client.Options{HostPort: addr, Logger: durable.Logger(io.Discard, slog.LevelError)})
		if err == nil {
			_, err = cl.CheckHealth(context.Background(), &client.CheckHealthRequest{})
			cl.Close()
			if err == nil {
				return addr
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporal dev server did not start: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// cachedTestServer is the Temporal test server the Python SDK downloads for its tests
// (<tmp>/temporal-test-server-sdk-python-<version>), "" when there is none.
func cachedTestServer() string {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "temporal-test-server-sdk-python-*"))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		if info, err := os.Stat(matches[i]); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return matches[i]
		}
	}
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// durableEnv is one test's environment: a unique task queue, the object store and SQLite store
// in dir, the stub model and the (unsafe, test-only) local sandbox.
func durableEnv(t *testing.T, addr, dir, queue string) []string {
	return []string{
		"LHA_TEMPORAL_ADDRESS=" + addr, "LHA_TASK_QUEUE=" + queue,
		"LHA_OBJECT_STORE_ROOT=" + filepath.Join(dir, "objects"), "LHA_SQLITE_PATH=" + filepath.Join(dir, "lha.sqlite3"),
		"LHA_MODEL_BACKEND=stub", "LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MAX_REPLANS=0",
		"LHA_MAX_TURNS_PER_CYCLE=4",
	}
}

// missionFiles writes a 2-item roadmap and a gating check (more work files than done items).
func missionFiles(t *testing.T, dir string) (roadmap, check string) {
	t.Helper()
	roadmap = filepath.Join(dir, "roadmap.md")
	check = filepath.Join(dir, "check.sh")
	_ = os.WriteFile(roadmap, []byte("# Durable\n\n- [ ] task one\n- [ ] task two\n"), 0o644)
	_ = os.WriteFile(check, []byte(`done=$(grep -o '"status": *"done"' .lha/checklist.json | wc -l)
work=$(ls work/*.txt 2>/dev/null | wc -l)
[ "$work" -gt "$done" ]
`), 0o755)
	return roadmap, check
}

// gatedFactory: each cycle runs a flagged `git push` (counted in marker), writes the item's work
// file and is done — so every cycle queues an irreversible action for a human.
func gatedFactory(marker string) durable.ModelFactory {
	return func(_ *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		stop := func(s string) *string { return &s }
		return model.NewStub([]contracts.TurnResult{
			{ToolCalls: []contracts.ToolCall{{ID: "r1", Name: "run_command", Arguments: map[string]any{
				"argv": []any{"sh", "-c", "echo ran >> " + marker + "; git push lha-no-such-remote HEAD"}}}}, StopReason: stop("tool_use")},
			{ToolCalls: []contracts.ToolCall{{ID: "w1", Name: "write_file", Arguments: map[string]any{
				"path": "work/" + snap.ActiveItem.ID + ".txt", "content": "done"}}}, StopReason: stop("tool_use")},
			{Text: `{"done": true, "summary": "ok"}`, StopReason: stop("end_turn")},
		}), nil
	}
}

const pythonWorkerScript = `
import asyncio, sys
from temporalio.service import RPCError, RPCStatusCode
from temporalio.worker import Worker
from lha.config import get_settings
from lha.contracts.model import ToolCall, TurnResult
from lha.durable.activities import (check_mission_health, declare_impossible, make_cycle_activity,
    notify_gate, read_mission_snapshot, record_mission_status, unblock_items)
from lha.durable.worker import check_task_queue_pollers, connect_client, worker_identity
from lha.durable.workflows import MissionWorkflow
from lha.model.stub import StubModel

marker = sys.argv[1]

def factory(settings, snap):
    argv = ["sh", "-c", f"echo ran >> {marker}; git push lha-no-such-remote HEAD"]
    return StubModel(script=[
        TurnResult(tool_calls=[ToolCall(id="r1", name="run_command", arguments={"argv": argv})], stop_reason="tool_use"),
        TurnResult(tool_calls=[ToolCall(id="w1", name="write_file", arguments={"path": f"work/{snap.active_item.id}.txt", "content": "done"})], stop_reason="tool_use"),
        TurnResult(text='{"done": true, "summary": "ok"}', stop_reason="end_turn"),
    ])

async def main():
    settings = get_settings()
    client = await connect_client(settings)
    try:
        await check_task_queue_pollers(client, settings.task_queue)
    except RPCError as exc:  # a test server without DescribeTaskQueue
        if exc.status != RPCStatusCode.UNIMPLEMENTED:
            raise
    worker = Worker(client, task_queue=settings.task_queue, identity=worker_identity(), workflows=[MissionWorkflow],
        activities=[make_cycle_activity(model_factory=factory), check_mission_health, notify_gate, declare_impossible,
                    unblock_items, read_mission_snapshot, record_mission_status])
    async with worker:
        print("ready", flush=True)
        await asyncio.Event().wait()

asyncio.run(main())
`

// startPythonWorker runs a Python lha worker (scripted model) until the test ends.
func startPythonWorker(t *testing.T, dir string, env []string, marker string) {
	t.Helper()
	cmd := exec.Command("uv", "run", "--quiet", "--project", filepath.Join(repoRoot, "python"), "python", "-c", pythonWorkerScript, marker)
	cmd.Dir, cmd.Env = dir, processEnv(env...)
	// Its own process group: uv starts python as a child, and both must go at the end.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(90 * time.Second):
		t.Fatalf("python worker did not start: %s", stderr.String())
	}
}

// startGoWorker runs the Go worker in-process (scripted model, the CLI's real toolbox).
func startGoWorker(t *testing.T, env []string, marker string) client.Client {
	t.Helper()
	settings, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := durable.Dial(context.Background(), settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.CheckTaskQueuePollers(context.Background(), cl, settings.TaskQueue); err != nil {
		if !unimplemented(err) { // a test server without DescribeTaskQueue
			t.Fatal(err)
		}
	}
	w := durable.NewWorker(cl, settings.TaskQueue, &durable.Activities{
		Settings: settings, ModelFactory: gatedFactory(marker), OpenToolbox: openToolbox,
	})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Stop(); cl.Close() })
	return cl
}

// goLHA runs the Go CLI in-process with env.
func goLHAInProcess(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	cleanEnv(t, env...)
	t.Chdir(dir)
	return runCLI(t, nil, args...)
}

func startedID(t *testing.T, r result) string {
	t.Helper()
	m := startedRE.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil || m[1] != m[2] {
		t.Fatalf("mission-start: %+v", r)
	}
	return m[1]
}

// waitStatus polls status_v1 until it is want.
func waitStatus(t *testing.T, cl client.Client, id, want string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		var status string
		if v, err := cl.QueryWorkflow(context.Background(), durable.MissionWorkflowID(id), "", durable.QueryStatus); err == nil {
			_ = v.Get(&status)
			if status == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mission %s never reached %s (last %q)", id, want, status)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func missionResult(t *testing.T, cl client.Client, id string) (durable.MissionResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var res durable.MissionResult
	err := cl.GetWorkflow(ctx, durable.MissionWorkflowID(id), "").Get(ctx, &res)
	return res, err
}

// sameStatus runs mission-status with both CLIs and requires identical output.
func sameStatus(t *testing.T, dir string, env []string, id string) string {
	t.Helper()
	py := runPythonLHA(t, dir, processEnv(env...), "mission-status", id)
	gor := goLHAInProcess(t, dir, env, "mission-status", id)
	if py.code != 0 || gor.code != 0 || py.stdout != gor.stdout {
		t.Fatalf("mission-status differs:\n--- python (%d)\n%s%s\n--- go (%d)\n%s%s", py.code, py.stdout, py.stderr, gor.code, gor.stdout, gor.stderr)
	}
	return gor.stdout
}

// A mission started with the Go CLI and served by a Go worker, driven by the Python CLI.
func TestGoMissionDrivenByThePythonCLI(t *testing.T) {
	addr := temporalAddress(t)
	dir := t.TempDir()
	env := durableEnv(t, addr, dir, "lha-it-go-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	marker := filepath.Join(dir, "marker.txt")
	cl := startGoWorker(t, env, marker)
	roadmap, check := missionFiles(t, dir)

	id := startedID(t, goLHAInProcess(t, dir, env, "mission-start", "--checklist", roadmap, "--no-default-checks",
		"--check", "sh "+check, "--workdir", filepath.Join(dir, "ws1"), "--deadlock-gate-hours", "0"))
	waitStatus(t, cl, id, durable.StatusWaitingOnHuman)
	out := sameStatus(t, dir, env, id)
	if !strings.Contains(out, "gate: tool_call approval-") || !strings.Contains(out, "pending action: run_command") {
		t.Fatalf("status:\n%s", out)
	}
	// Validated against the open gate: a deadlock decision is refused (exit 2).
	if r := runPythonLHA(t, dir, processEnv(env...), "mission-approve", id, "--decision", "retry"); r.code != 2 ||
		!strings.Contains(r.stderr, "for the open tool_call gate; expected approve, reject") {
		t.Fatalf("python approve retry: %+v", r)
	}
	if r := runPythonLHA(t, dir, processEnv(env...), "mission-approve", id, "--decision", "approve"); r.code != 0 ||
		!strings.Contains(r.stdout, "sent decision 'approve' to mission "+id) {
		t.Fatalf("python approve: %+v", r)
	}
	// Item 02's cycle runs the approved action (the same call: the same fingerprint) once.
	res, err := missionResult(t, cl, id)
	if err != nil || !res.Completed || res.ItemsDone != 2 {
		t.Fatalf("result %+v %v", res, err)
	}
	if data, _ := os.ReadFile(marker); strings.Count(string(data), "ran") != 1 {
		t.Fatalf("the approved action ran %q", data)
	}

	// A scheduled start sleeps; the Python CLI snoozes it, then aborts it.
	id2 := startedID(t, goLHAInProcess(t, dir, env, "mission-start", "--checklist", roadmap, "--no-default-checks",
		"--check", "sh "+check, "--workdir", filepath.Join(dir, "ws2"), "--start-in-seconds", "3600"))
	waitStatus(t, cl, id2, durable.StatusSleeping)
	if r := runPythonLHA(t, dir, processEnv(env...), "mission-snooze", id2, "--seconds", "7200"); r.code != 0 ||
		r.stdout != fmt.Sprintf("mission %s: snoozed 7200s\n", id2) {
		t.Fatalf("python snooze: %+v", r)
	}
	if out := sameStatus(t, dir, env, id2); !strings.Contains(out, "sleeping until ") {
		t.Fatalf("status:\n%s", out)
	}
	if r := runPythonLHA(t, dir, processEnv(env...), "mission-abort", id2); r.code != 0 || r.stdout != "cancelled mission "+id2+"\n" {
		t.Fatalf("python abort: %+v", r)
	}
	if _, err := missionResult(t, cl, id2); !temporal.IsCanceledError(err) && !isCanceled(err) {
		t.Fatalf("aborted mission ended with %v", err)
	}
}

func isCanceled(err error) bool {
	var ce *temporal.CanceledError
	return errors.As(err, &ce)
}

// A mission started with the Python CLI and served by a Python worker, driven by the Go CLI.
func TestPythonMissionDrivenByTheGoCLI(t *testing.T) {
	addr := temporalAddress(t)
	dir := t.TempDir()
	env := durableEnv(t, addr, dir, "lha-it-py-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	marker := filepath.Join(dir, "marker.txt")
	startPythonWorker(t, dir, env, marker)
	roadmap, check := missionFiles(t, dir)
	settings, _ := config.LoadFrom(env, "")
	cl, err := durable.Dial(context.Background(), settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	id := startedID(t, runPythonLHA(t, dir, processEnv(env...), "mission-start", "--checklist", roadmap,
		"--no-default-checks", "--check", "sh "+check, "--workdir", filepath.Join(dir, "ws1"), "--deadlock-gate-hours", "0"))
	waitStatus(t, cl, id, durable.StatusWaitingOnHuman)
	out := sameStatus(t, dir, env, id)
	if !strings.Contains(out, "gate: tool_call approval-") {
		t.Fatalf("status:\n%s", out)
	}
	if r := goLHAInProcess(t, dir, env, "mission-approve", id, "--decision", "impossible"); r.code != 2 ||
		!strings.Contains(r.stderr, "error: unknown --decision 'impossible' for the open tool_call gate; expected approve, reject") {
		t.Fatalf("go approve impossible: %+v", r)
	}
	if r := goLHAInProcess(t, dir, env, "mission-approve", id, "--decision", "approve"); r.code != 0 ||
		r.stdout != "sent decision 'approve' to mission "+id+"\n" {
		t.Fatalf("go approve: %+v", r)
	}
	res, err := missionResult(t, cl, id)
	if err != nil || !res.Completed || res.ItemsDone != 2 {
		t.Fatalf("result %+v %v", res, err)
	}
	if data, _ := os.ReadFile(marker); strings.Count(string(data), "ran") != 1 {
		t.Fatalf("the approved action ran %q", data)
	}

	id2 := startedID(t, runPythonLHA(t, dir, processEnv(env...), "mission-start", "--checklist", roadmap,
		"--no-default-checks", "--check", "sh "+check, "--workdir", filepath.Join(dir, "ws2"), "--start-in-seconds", "3600"))
	waitStatus(t, cl, id2, durable.StatusSleeping)
	if r := goLHAInProcess(t, dir, env, "mission-snooze", id2, "--seconds", "7200"); r.code != 0 || r.stdout != fmt.Sprintf("mission %s: snoozed 7200s\n", id2) {
		t.Fatalf("go snooze: %+v", r)
	}
	sameStatus(t, dir, env, id2)
	if r := goLHAInProcess(t, dir, env, "mission-abort", id2); r.code != 0 || r.stdout != "cancelled mission "+id2+"\n" {
		t.Fatalf("go abort: %+v", r)
	}
	if _, err := missionResult(t, cl, id2); !isCanceled(err) {
		t.Fatalf("aborted mission ended with %v", err)
	}
}

// waitPoller waits until the task queue lists a poller whose identity contains marker.
func waitPoller(t *testing.T, cl client.Client, queue, marker string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := cl.DescribeTaskQueue(context.Background(), queue, enumspb.TASK_QUEUE_TYPE_WORKFLOW)
		if err == nil {
			for _, p := range resp.GetPollers() {
				if strings.Contains(p.GetIdentity(), marker) {
					return
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no %s poller on %s", marker, queue)
}

func unimplemented(err error) bool {
	var u *serviceerror.Unimplemented
	return errors.As(err, &u)
}

// Each implementation's worker refuses a task queue the other implementation polls.
func TestWorkersRefuseMixedTaskQueues(t *testing.T) {
	addr := temporalAddress(t)
	needsDescribeTaskQueue(t, addr)
	dir := t.TempDir()

	goQueue := "lha-it-guard-go-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	goEnv := durableEnv(t, addr, dir, goQueue)
	cl := startGoWorker(t, goEnv, filepath.Join(dir, "m1"))
	waitPoller(t, cl, goQueue, durable.IdentityMarker)
	// Bounded: a worker the guard failed to stop would poll forever.
	ctx, cancelPy := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelPy()
	py := exec.CommandContext(ctx, "uv", "run", "--quiet", "--project", filepath.Join(repoRoot, "python"), "lha", "worker")
	py.Dir, py.Env = dir, pythonEnv(dir, processEnv(goEnv...))
	var pyOut, pyErr strings.Builder
	py.Stdout, py.Stderr = &pyOut, &pyErr
	_ = py.Run()
	r := result{pyOut.String(), pyErr.String(), py.ProcessState.ExitCode()}
	if r.code != 2 || !strings.Contains(r.stderr, "is already polled by a Go lha worker") ||
		!strings.Contains(r.stderr, "LHA_TASK_QUEUE="+goQueue+"-py") {
		t.Fatalf("python worker on a Go queue: %+v", r)
	}

	pyQueue := "lha-it-guard-py-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	pyEnv := durableEnv(t, addr, dir, pyQueue)
	startPythonWorker(t, dir, pyEnv, filepath.Join(dir, "m2"))
	waitPoller(t, cl, pyQueue, durable.PythonIdentityMarker)
	cleanEnv(t, pyEnv...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, errOut syncBuffer // the SDK's pollers may still log after run returns
	code := (&cli{stdout: &out, stderr: &errOut, ctx: ctx}).run([]string{"worker"})
	r = result{out.String(), errOut.String(), code}
	if r.code != 2 || !strings.Contains(r.stderr, "is already polled by a Python lha worker") ||
		!strings.Contains(r.stderr, "LHA_TASK_QUEUE="+pyQueue+"-go") {
		t.Fatalf("go worker on a Python queue: %+v", r)
	}
}

// needsDescribeTaskQueue skips on a server without DescribeTaskQueue (the Python SDK's test server).
func needsDescribeTaskQueue(t *testing.T, addr string) {
	t.Helper()
	if probe, err := client.Dial(client.Options{HostPort: addr, Logger: durable.Logger(io.Discard, slog.LevelError)}); err == nil {
		_, err := probe.DescribeTaskQueue(context.Background(), "lha-probe", enumspb.TASK_QUEUE_TYPE_WORKFLOW)
		probe.Close()
		if unimplemented(err) {
			t.Skip("this Temporal server does not implement DescribeTaskQueue (a test server)")
		}
	}
}

// startPythonLHAWorker starts `lha worker` (the Python CLI) in its own process group (uv starts
// python as a child; both go when ctx ends); done is closed when it exited.
func startPythonLHAWorker(t *testing.T, ctx context.Context, dir string, env []string) (py *exec.Cmd, stderr *strings.Builder, done chan struct{}) {
	t.Helper()
	py = exec.CommandContext(ctx, "uv", "run", "--quiet", "--project", filepath.Join(repoRoot, "python"), "lha", "worker")
	py.Dir, py.Env = dir, pythonEnv(dir, processEnv(env...))
	py.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	py.Cancel = func() error { return syscall.Kill(-py.Process.Pid, syscall.SIGKILL) }
	py.WaitDelay = 5 * time.Second
	stderr = &strings.Builder{}
	py.Stdout, py.Stderr = io.Discard, stderr
	if err := py.Start(); err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() { _ = py.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-py.Process.Pid, syscall.SIGKILL); <-done })
	return py, stderr, done
}

// A Go and a Python worker started on the same queue at the same moment: whichever way the race
// goes (a startup refusal, or both past their startup checks and then a re-check), at most one
// keeps running, and every one that stopped exits 2 with the guard's message.
func TestWorkersStartedTogetherLeaveAtMostOneRunning(t *testing.T) {
	addr := temporalAddress(t)
	needsDescribeTaskQueue(t, addr)
	dir := t.TempDir()
	queue := "lha-it-race-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	env := append(durableEnv(t, addr, dir, queue), "LHA_WORKER_GUARD_INTERVAL_S=0.5")
	cleanEnv(t, env...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type exited struct {
		who string
		r   result
	}
	done := make(chan exited, 2)
	py, pyErr, pyDone := startPythonLHAWorker(t, ctx, dir, env)
	go func() {
		<-pyDone
		done <- exited{"python", result{"", pyErr.String(), py.ProcessState.ExitCode()}}
	}()
	var goOut, goErr syncBuffer // the SDK's pollers may still log after run returns
	goDone := make(chan struct{})
	go func() {
		defer close(goDone)
		code := (&cli{stdout: &goOut, stderr: &goErr, ctx: ctx}).run([]string{"worker"})
		done <- exited{"go", result{goOut.String(), goErr.String(), code}}
	}()
	defer func() { cancel(); <-goDone }()

	check := func(e exited) {
		t.Helper()
		t.Logf("the %s worker stopped", e.who)
		other := map[string]string{"go": "Python", "python": "Go"}[e.who]
		if e.r.code != 2 || !strings.Contains(e.r.stderr, "is already polled by a "+other+" lha worker") {
			t.Fatalf("%s worker: %+v", e.who, e.r)
		}
	}
	select {
	case first := <-done:
		check(first)
	case <-time.After(90 * time.Second):
		t.Fatal("both workers still poll the same task queue")
	}
	// The survivor, if any, keeps running for several guard intervals.
	select {
	case second := <-done:
		check(second) // both stopped: fail closed
	case <-time.After(5 * time.Second):
	}
}

// A Go worker that raced past its startup check (it skips it here) onto a queue a Python worker
// already serves: the Python worker's re-check stops it (exit 2), and the Go worker's re-check
// stops the Go worker (the stopped Python poller is still listed): neither keeps serving.
func TestTheGuardRecheckStopsWorkersThatRacedPastTheStartupCheck(t *testing.T) {
	addr := temporalAddress(t)
	needsDescribeTaskQueue(t, addr)
	dir := t.TempDir()
	queue := "lha-it-recheck-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	env := append(durableEnv(t, addr, dir, queue), "LHA_WORKER_GUARD_INTERVAL_S=0.5")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	py, pyErr, pyDone := startPythonLHAWorker(t, ctx, dir, env)
	settings, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := durable.Dial(context.Background(), settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	waitPoller(t, cl, queue, durable.PythonIdentityMarker)

	w := durable.NewWorker(cl, queue, &durable.Activities{Settings: settings, ModelFactory: gatedFactory(filepath.Join(dir, "m")), OpenToolbox: openToolbox})
	goErr := durable.RunGuarded(ctx, w, cl, queue, 500*time.Millisecond, nil)
	var mixed *durable.MixedWorkersError
	if !errors.As(goErr, &mixed) || !strings.Contains(mixed.Identity, durable.PythonIdentityMarker) {
		t.Fatalf("go worker: %v", goErr)
	}
	select {
	case <-pyDone:
	case <-ctx.Done():
		t.Fatal("the python worker kept polling")
	}
	if code := py.ProcessState.ExitCode(); code != 2 || !strings.Contains(pyErr.String(), "is already polled by a Go lha worker") ||
		!strings.Contains(pyErr.String(), "LHA_TASK_QUEUE="+queue+"-py") {
		t.Fatalf("python worker (%d): %s", code, pyErr.String())
	}
}
