package durable

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/proto"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Object store, ClaimCheck codec, spend journal, workdir lock, payload shapes and the worker
// guard (python: tests/durability/test_hardening.py), with the cross-language halves run against
// the Python implementation when uv is on PATH.

func pythonProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping the cross-implementation check")
	}
	if testing.Short() {
		t.Skip("cross-implementation check is slow")
	}
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "python")
}

// runPython runs a Python snippet (sys.argv[1:] = args) with the reference implementation.
func runPython(t *testing.T, script string, args ...string) []byte {
	t.Helper()
	project := pythonProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "uv", append([]string{"run", "--quiet", "--project", project, "python", "-c", script}, args...)...)
	cmd.Dir = project
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, stderr.String())
	}
	return out
}

func TestObjectStoreRoundtripAndDedup(t *testing.T) {
	store, err := NewLocalFileObjectStore(filepath.Join(t.TempDir(), "objs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k1, _ := store.Put(ctx, []byte("hello"))
	k2, _ := store.Put(ctx, []byte("hello"))
	if k1 != k2 || len(k1) != 64 {
		t.Fatalf("keys %s %s", k1, k2)
	}
	got, err := store.Get(ctx, k1)
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q %v", got, err)
	}
	entries, _ := os.ReadDir(store.Root())
	if len(entries) != 1 {
		t.Fatalf("%d files (a temp file leaked?)", len(entries))
	}
}

func TestObjectStoreRejectsBadKeysAndCorruption(t *testing.T) {
	store, _ := NewLocalFileObjectStore(t.TempDir())
	ctx := context.Background()
	var bad *InvalidObjectKeyError
	for _, key := range []string{"../etc/passwd", strings.Repeat("A", 64), "abc"} {
		if _, err := store.Get(ctx, key); !errors.As(err, &bad) {
			t.Fatalf("%q: %v", key, err)
		}
	}
	key, _ := store.Put(ctx, []byte("data"))
	_ = os.WriteFile(filepath.Join(store.Root(), key), []byte("tampered"), 0o644)
	var corrupt *ObjectCorruptError
	if _, err := store.Get(ctx, key); !errors.As(err, &corrupt) {
		t.Fatalf("err %v", err)
	}
	if _, err := NewLocalFileObjectStore("  "); err == nil {
		t.Fatal("an empty root was accepted")
	}
}

func TestObjectStoreRootIsAbsolute(t *testing.T) {
	store, _ := NewLocalFileObjectStore("relative/objs")
	if !filepath.IsAbs(store.Root()) {
		t.Fatalf("root %s", store.Root())
	}
}

func bigPayload(t *testing.T) *commonpb.Payload {
	t.Helper()
	p, err := converter.GetDefaultDataConverter().ToPayload(map[string]string{"blob": strings.Repeat("x", 40_000)})
	if err != nil {
		t.Fatal(err)
	}
	p.Metadata["custom"] = []byte("kept")
	return p
}

func TestClaimCheckOffloadsLargePayloadsAndPreservesMetadata(t *testing.T) {
	codec, err := NewClaimCheckCodec(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	small, _ := converter.GetDefaultDataConverter().ToPayload("small")
	big := bigPayload(t)
	enc, err := codec.Encode([]*commonpb.Payload{small, big})
	if err != nil {
		t.Fatal(err)
	}
	if enc[0] != small || string(enc[1].Metadata["encoding"]) != "lha/claimcheck/v2" || len(enc[1].Data) != 64 {
		t.Fatalf("encoded %v", enc)
	}
	dec, err := codec.Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(dec[1], big) || !proto.Equal(dec[0], small) {
		t.Fatal("decode did not restore the payloads")
	}
}

func TestClaimCheckDecodesLegacyV1Pointers(t *testing.T) {
	codec, _ := NewClaimCheckCodec(t.TempDir())
	key, _ := codec.Store.Put(context.Background(), []byte(`"raw"`))
	dec, err := codec.Decode([]*commonpb.Payload{{
		Metadata: map[string][]byte{"encoding": []byte("lha/claimcheck"), "lha-orig-encoding": []byte("json/plain")},
		Data:     []byte(key),
	}})
	if err != nil || string(dec[0].Data) != `"raw"` || string(dec[0].Metadata["encoding"]) != "json/plain" {
		t.Fatalf("decoded %v %v", dec, err)
	}
}

// A payload offloaded by either implementation decodes in the other: same metadata, same key,
// same store layout.
func TestClaimCheckIsInterchangeableWithPython(t *testing.T) {
	root := t.TempDir()
	codec, _ := NewClaimCheckCodec(root)
	big := bigPayload(t)
	enc, _ := codec.Encode([]*commonpb.Payload{big})
	pointer, _ := proto.Marshal(enc[0])
	script := `
import asyncio, base64, sys
from temporalio.api.common.v1 import Payload
from lha.durable.codec import ClaimCheckCodec
codec = ClaimCheckCodec(root=sys.argv[1])
p = Payload(); p.ParseFromString(base64.b64decode(sys.argv[2]))
orig = asyncio.run(codec.decode([p]))[0]
big = Payload(metadata={"encoding": b"json/plain", "py": b"meta"}, data=b'"' + b"y" * 40000 + b'"')
enc = asyncio.run(codec.encode([big]))[0]
print(base64.b64encode(orig.SerializeToString(deterministic=True)).decode())
print(base64.b64encode(enc.SerializeToString()).decode())
`
	out := strings.Fields(string(runPython(t, script, root, base64.StdEncoding.EncodeToString(pointer))))
	raw, _ := base64.StdEncoding.DecodeString(out[0])
	got := &commonpb.Payload{}
	if err := proto.Unmarshal(raw, got); err != nil || !proto.Equal(got, big) {
		t.Fatalf("python decoded a Go pointer to %v (%v)", got, err)
	}
	raw, _ = base64.StdEncoding.DecodeString(out[1])
	pyPointer := &commonpb.Payload{}
	_ = proto.Unmarshal(raw, pyPointer)
	dec, err := codec.Decode([]*commonpb.Payload{pyPointer})
	if err != nil || string(dec[0].Metadata["py"]) != "meta" || len(dec[0].Data) != 40_002 {
		t.Fatalf("go decoded a Python pointer to %v (%v)", dec, err)
	}
	// The stored Payload is serialized deterministically: the same payload gets the same key.
	again, _ := codec.Encode([]*commonpb.Payload{big})
	if string(again[0].Data) != string(enc[0].Data) {
		t.Fatal("encode is not deterministic")
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := state.NewGitMissionAnchor(dir).Initialize(context.Background(), "t", "d", testChecklist(1, false)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSpendJournalIsIdempotentPerKeyAndSeedsTheMeter(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	ledger := governor.NewCostLedger()
	usd := 0.25
	ledger.Record("c1", contracts.Usage{Model: "m"}, &usd, "lead")
	ledger.Record("c1", contracts.Usage{Model: "m"}, nil, "lead")
	for range 2 { // a replayed append of the same attempt counts once
		if err := RecordSpend(ctx, dir, "k1", "c1", ledger); err != nil {
			t.Fatal(err)
		}
	}
	gitDir, _ := state.GitDir(ctx, dir)
	f, _ := os.OpenFile(filepath.Join(gitDir, "lha", "spend.ndjson"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"key": "torn", "us`) // a torn last line from a crash mid-append
	f.Close()
	got, unknown, err := ReadPriorSpend(ctx, dir)
	if err != nil || got != 0.25 || unknown != 1 {
		t.Fatalf("prior %v %d %v", got, unknown, err)
	}
	cap1 := 1.0
	meter, err := BuildCycleMeter(ctx, testSettings(t), dir, "c2", &cap1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if meter.Ledger.TotalUSD() != 0.25 || meter.Ledger.UnknownCostEntries() != 1 || meter.CycleID() != "c2" {
		t.Fatalf("meter %v %d", meter.Ledger.TotalUSD(), meter.Ledger.UnknownCostEntries())
	}
	// Seeded prior entries are not this attempt's spend.
	if err := RecordSpend(ctx, dir, "k2", "c2", meter.Ledger); err != nil {
		t.Fatal(err)
	}
	// One of three concurrent implementers gets the prior spend plus a third of what is left.
	shared, err := BuildWaveMeter(ctx, testSettings(t), dir, "c3", &cap1, 10, 3)
	if err != nil || shared.Governor.CeilingUSD() != 0.25+0.75/3 || shared.Ledger.TotalUSD() != 0.25 {
		t.Fatalf("wave meter %+v %v", shared, err)
	}
	if again, _, _ := ReadPriorSpend(ctx, dir); again != 0.25 {
		t.Fatalf("prior spend re-journaled: %v", again)
	}
}

func TestSpendJournalIsSharedWithPython(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	ledger := governor.NewCostLedger()
	usd := 0.125
	ledger.Record("c1", contracts.Usage{Model: "m"}, &usd, "lead")
	_ = RecordSpend(ctx, dir, IdempotencyKey("m1", "c1", "1"), "c1", ledger)
	script := `
import sys
from lha.durable.activities import read_prior_spend, record_spend
from lha.governor.cost import CostLedger
from lha.contracts.model import Usage
from lha.ids import idempotency_key
print(read_prior_spend(sys.argv[1]))
ledger = CostLedger()
ledger.record(cycle_id="c2", usage=Usage(model="m", input_tokens=1, output_tokens=1), usd=0.5)
ledger.record(cycle_id="c2", usage=Usage(model="m", input_tokens=1, output_tokens=1), usd=None)
record_spend(sys.argv[1], key="py", cycle_id="c2", ledger=ledger)
print(idempotency_key("m1", "c1", 1))
`
	out := strings.Split(strings.TrimSpace(string(runPython(t, script, dir))), "\n")
	if out[0] != "(0.125, 0)" || out[1] != IdempotencyKey("m1", "c1", "1") {
		t.Fatalf("python read %q", out)
	}
	if got, unknown, _ := ReadPriorSpend(ctx, dir); got != 0.625 || unknown != 1 {
		t.Fatalf("go read %v %d", got, unknown)
	}
}

func TestWorkdirLockExcludesAPythonHolder(t *testing.T) {
	dir := initRepo(t)
	ctx := context.Background()
	release, err := WorkdirLock(ctx, dir, CycleLock, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	var busy *WorkdirBusyError
	if _, err := WorkdirLock(ctx, dir, CycleLock, time.Second, nil); !errors.As(err, &busy) {
		t.Fatalf("a second holder got the lock: %v", err)
	}
	// Python's workdir_flock sees Go's lock (and the reverse): the same flock on the same file.
	script := `
import asyncio, sys
from lha.state.locks import workdir_flock, WorkdirBusyError
async def main():
    try:
        async with workdir_flock(sys.argv[1], wait_s=0.5):
            print("acquired")
    except WorkdirBusyError:
        print("busy")
asyncio.run(main())
`
	if out := strings.TrimSpace(string(runPython(t, script, dir))); out != "busy" {
		t.Fatalf("python while Go holds the lock: %s", out)
	}
	release()
	if out := strings.TrimSpace(string(runPython(t, script, dir))); out != "acquired" {
		t.Fatalf("python after Go released the lock: %s", out)
	}
}

// The payload types marshal with the Python field names and never emit null for a list.
func TestPayloadsUsePythonFieldNames(t *testing.T) {
	data, _ := json.Marshal(NewMissionInput("m", "/w"))
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	for _, key := range []string{"mission_id", "cycles_before_can", "check_commands", "budget_usd", "gate_escalation_seconds",
		"deadlock_gate_default", "resume_at", "research_per_item", "review", "max_parallel", "state"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in %s", key, data)
		}
	}
	if got["check_commands"] != nil || got["state"] != nil {
		t.Fatalf("nullable fields %s", data)
	}
	data, _ = json.Marshal(CycleResult{})
	if !strings.Contains(string(data), `"pending_approvals":[]`) || !strings.Contains(string(data), `"item_id":null`) {
		t.Fatalf("cycle result %s", data)
	}
	data, _ = json.Marshal(MissionState{})
	for _, list := range []string{"steer_notes", "approved_actions", "rejected_actions", "gate_log"} {
		if !strings.Contains(string(data), `"`+list+`":[]`) {
			t.Fatalf("state %s", data)
		}
	}
	var in MissionInput
	_ = json.Unmarshal([]byte(`{"mission_id": "x", "workdir": "/w"}`), &in)
	if in.MaxCycles != 1000 || in.CyclesBeforeCAN != 200 || in.DeadlockGateDefault != "abort" || len(in.GateEscalationSeconds) != 4 {
		t.Fatalf("defaults %+v", in)
	}
}

// Payloads round-trip through the Python dataclasses (Go -> Python -> Go).
func TestPayloadsRoundTripThroughPython(t *testing.T) {
	budget := 2.5
	inp := NewMissionInput("m1", "/w")
	inp.BudgetUSD = &budget
	inp.CheckCommands = [][]string{{"true"}}
	st := NewMissionState()
	st.PendingDecision = strPtr("approve")
	st.ApprovedActions = []ApprovedAction{{Fingerprint: "f", Summary: "s"}}
	inp.State = &st
	data, _ := converter.GetDefaultDataConverter().ToPayload(inp)
	script := `
import sys
from temporalio.converter import DataConverter
from temporalio.api.common.v1 import Payload
from lha.durable.types import MissionInput, GateView, PendingApproval
p = Payload(metadata={"encoding": b"json/plain"}, data=sys.argv[1].encode())
inp = DataConverter.default.payload_converter.from_payloads([p], [MissionInput])[0]
assert inp.state.approved_actions[0].fingerprint == "f", inp
assert inp.budget_usd == 2.5 and inp.check_commands == [["true"]]
gate = GateView(gate_id="g", kind="tool_call", question="q?", options=["approve", "reject"], default_action="reject",
                request=PendingApproval(fingerprint="f", tool="run_command", reason="r"))
print(DataConverter.default.payload_converter.to_payloads([inp, gate])[0].data.decode())
print(DataConverter.default.payload_converter.to_payloads([gate])[0].data.decode())
`
	out := strings.Split(strings.TrimSpace(string(runPython(t, script, string(data.Data)))), "\n")
	var back MissionInput
	if err := json.Unmarshal([]byte(out[0]), &back); err != nil {
		t.Fatal(err)
	}
	if back.State == nil || *back.State.PendingDecision != "approve" || *back.BudgetUSD != 2.5 || back.MaxCycles != 1000 {
		t.Fatalf("back %+v", back)
	}
	var gate GateView
	if err := json.Unmarshal([]byte(out[1]), &gate); err != nil || gate.Request == nil || gate.Request.Tool != "run_command" {
		t.Fatalf("gate %+v %v", gate, err)
	}
}

type fakeDescriber map[enumspb.TaskQueueType][]string

func (f fakeDescriber) DescribeTaskQueue(_ context.Context, _ string, kind enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error) {
	resp := &workflowservice.DescribeTaskQueueResponse{}
	for _, id := range f[kind] {
		resp.Pollers = append(resp.Pollers, &taskqueuepb.PollerInfo{Identity: id})
	}
	return resp, nil
}

func TestWorkerGuardRefusesPythonPollers(t *testing.T) {
	ctx := context.Background()
	ok := fakeDescriber{enumspb.TASK_QUEUE_TYPE_WORKFLOW: {"lha-go:1@host", "42@legacy-host"}}
	if err := CheckTaskQueuePollers(ctx, ok, "lha-mission"); err != nil {
		t.Fatal(err)
	}
	mixed := fakeDescriber{enumspb.TASK_QUEUE_TYPE_ACTIVITY: {"lha-py:7@host"}}
	err := CheckTaskQueuePollers(ctx, mixed, "lha-mission")
	var mw *MixedWorkersError
	if !errors.As(err, &mw) || !strings.Contains(err.Error(), "LHA_TASK_QUEUE=lha-mission-go") ||
		!strings.Contains(err.Error(), "lha-py:7@host") {
		t.Fatalf("err %v", err)
	}
	if !strings.HasPrefix(WorkerIdentity(), "lha-go:") {
		t.Fatalf("identity %s", WorkerIdentity())
	}
}

// sequenceDescriber answers its n-th DescribeTaskQueue call with answers[n] (the last repeats):
// a list of poller identities, or an error.
type sequenceDescriber struct {
	mu      sync.Mutex
	answers []any
	calls   int
}

func (f *sequenceDescriber) DescribeTaskQueue(context.Context, string, enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	answer := f.answers[min(f.calls, len(f.answers)-1)]
	f.calls++
	if err, ok := answer.(error); ok {
		return nil, err
	}
	resp := &workflowservice.DescribeTaskQueueResponse{}
	for _, id := range answer.([]string) {
		resp.Pollers = append(resp.Pollers, &taskqueuepb.PollerInfo{Identity: id})
	}
	return resp, nil
}

// A running worker keeps re-checking; a failed check is reported and retried, a Python poller
// that appears ends the guard.
func TestWorkerGuardRechecksUntilAPythonPollerAppears(t *testing.T) {
	goOnly, both := []string{"lha-go:1@h"}, []string{"lha-go:1@h", "lha-py:9@h"}
	d := &sequenceDescriber{answers: []any{goOnly, goOnly, errors.New("unavailable"), both}}
	var reported []error
	err := GuardTaskQueue(context.Background(), d, "lha-mission", time.Millisecond, func(err error) { reported = append(reported, err) })
	var mw *MixedWorkersError
	if !errors.As(err, &mw) || mw.Identity != "lha-py:9@h" || len(reported) != 1 || d.calls != 4 {
		t.Fatalf("err %v, reported %v, calls %d", err, reported, d.calls)
	}
	// Until ctx ends, when nobody else appears.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := GuardTaskQueue(ctx, &sequenceDescriber{answers: []any{goOnly}}, "q", time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
}

// fakeWorker runs until stopped (or fails at once with err).
type fakeWorker struct {
	worker.Worker
	err     error
	stopped atomic.Bool
}

func (w *fakeWorker) Run(stop <-chan any) error {
	if w.err != nil {
		return w.err
	}
	<-stop
	w.stopped.Store(true)
	return nil
}

func TestRunGuardedStopsTheWorker(t *testing.T) {
	// The guard trips: the worker is stopped and the mixed-workers error returned.
	w := &fakeWorker{}
	err := RunGuarded(context.Background(), w, &sequenceDescriber{answers: []any{[]string{}, []string{}, []string{"lha-py:3@h"}}},
		"q", time.Millisecond, nil)
	var mw *MixedWorkersError
	if !errors.As(err, &mw) || !w.stopped.Load() || !strings.Contains(err.Error(), "LHA_TASK_QUEUE=q-go") {
		t.Fatalf("err %v stopped %v", err, w.stopped.Load())
	}
	// ctx ends: a clean stop.
	w = &fakeWorker{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := RunGuarded(ctx, w, &sequenceDescriber{answers: []any{[]string{}}}, "q", time.Millisecond, nil); err != nil || !w.stopped.Load() {
		t.Fatalf("err %v stopped %v", err, w.stopped.Load())
	}
	// The worker fails: its error, and the guard ends too.
	boom := errors.New("boom")
	if err := RunGuarded(context.Background(), &fakeWorker{err: boom}, &sequenceDescriber{answers: []any{[]string{}}}, "q", time.Hour, nil); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	// An interval that overflowed time.Duration never ticks.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if err := GuardTaskQueue(ctx2, &sequenceDescriber{answers: []any{[]string{"lha-py:1@h"}}}, "q", -1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIsoFormatting(t *testing.T) {
	if got := IsoFull(1727172000.5); got != "2024-09-24T10:00:00.500000+00:00" {
		t.Fatal(got)
	}
	if got := IsoFull(1727172000); got != "2024-09-24T10:00:00+00:00" {
		t.Fatal(got)
	}
	if got := isoSeconds(time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("x", 3600))); got != "2026-01-02T02:04:05+00:00" {
		t.Fatal(got)
	}
}

func TestGateNoticePayloadMatchesPython(t *testing.T) {
	n := GateNotice{MissionID: "m", Workdir: "/w", GateID: "approval-abc", Kind: GateToolCall, Event: "reminder",
		Question: "Allow?", Options: []string{"approve", "reject"}, DefaultAction: "reject", Step: 2,
		Deadline: "2026-01-01T00:00:00+00:00", Request: &PendingApproval{Fingerprint: "f", Tool: "run_command", Reason: "git push", Arguments: "{'argv': ['git', 'push']}"}}
	goJSON := string(GateNoticePayload(n).JSON())
	nj, _ := json.Marshal(n)
	script := `
import json, sys
from temporalio.converter import DataConverter
from temporalio.api.common.v1 import Payload
from lha.durable.activities import gate_notice_payload
from lha.durable.types import GateNotice
p = Payload(metadata={"encoding": b"json/plain"}, data=sys.argv[1].encode())
n = DataConverter.default.payload_converter.from_payloads([p], [GateNotice])[0]
print(json.dumps(gate_notice_payload(n), ensure_ascii=False, separators=(",", ":")))
`
	if py := strings.TrimSpace(string(runPython(t, script, string(nj)))); py != goJSON {
		t.Fatalf("payload differs:\n go %s\n py %s", goJSON, py)
	}
}
