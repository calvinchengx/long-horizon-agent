package state

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// These tests prove the .lha/ anchor is interchangeable between the Go and the Python
// implementation: each writes a mission anchor that the other reads back identically.

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
func runPython(t *testing.T, project, script string, args ...string) []byte {
	t.Helper()
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

func sameJSON(t *testing.T, label string, got any, want []byte) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var gv, wv any
	if err := json.Unmarshal(g, &gv); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wv); err != nil {
		t.Fatalf("%s: %v in %s", label, err, want)
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Errorf("%s differs:\n go:     %s\n python: %s", label, g, want)
	}
}

const pyReadSnapshot = `
import asyncio, json, sys
from lha.contracts.state import EventRecord
from lha.state import git_ops
from lha.state.mission_anchor import GitMissionAnchor
a = GitMissionAnchor(sys.argv[1])
snap = asyncio.run(a.read_situational_awareness())
events = git_ops.show_at_head(sys.argv[1], ".lha/events.ndjson")
kinds = [EventRecord.model_validate_json(ln).kind for ln in events.split("\n") if ln]
print(json.dumps({"snapshot": json.loads(snap.model_dump_json()), "event_kinds": kinds}))
`

const pyCheckpoint = `
import asyncio, sys
from lha.contracts.state import Checkpoint, DecisionRecord, EventRecord
from lha.state.mission_anchor import GitMissionAnchor
async def main():
    a = GitMissionAnchor(sys.argv[1])
    cl = await a.read_checklist()
    cl.items[1].status = "done"
    cl.items[1].verified_by = ["pytest"]
    await a.commit_checkpoint(Checkpoint(
        cycle_id="py1", progress_summary="- python did 02", checklist=cl,
        decisions=[DecisionRecord(decision="py <&> \u2028 ok", rationale="r", affected=["b.py"])],
        events=[EventRecord(kind="py_event", cycle_id="py1", payload={"id": "02"})],
    ))
asyncio.run(main())
`

func TestCrossImplGoWritesPythonReads(t *testing.T) {
	project := pythonProject(t)
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	cl := twoItems()
	must(anchor.InitializeWithAcceptance(ctx, "Ship <X> & ü", "Make X work.\nReally.", "tests pass", cl))
	if err := anchor.AppendEvent(ctx, contracts.EventRecord{Kind: "started", CycleID: "c1"}); err != nil {
		t.Fatal(err)
	}
	cl.Items[0].Status = contracts.StatusDone
	cl.Items[0].VerifiedBy = []string{"pytest"}
	cl.Items[1].Notes = "line\u2028sep \u0085 <tag>"
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID: "c1", ProgressSummary: "- go did 01", Checklist: cl,
		Decisions: []contracts.DecisionRecord{{Decision: "go \u2029 decision & <b>", Rationale: "why", CycleID: "c1"}},
		Events:    []contracts.EventRecord{{Kind: "cycle", CycleID: "c1", Payload: map[string]any{"id": "01"}}},
	}))

	var py struct {
		Snapshot   json.RawMessage `json:"snapshot"`
		EventKinds []string        `json:"event_kinds"`
	}
	if err := json.Unmarshal(runPython(t, project, pyReadSnapshot, dir), &py); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "snapshot (Go-written, Python-read vs Go-read)", must(anchor.ReadSituationalAwareness(ctx)), py.Snapshot)
	if strings.Join(py.EventKinds, ",") != "started,cycle" {
		t.Fatalf("python saw events %v", py.EventKinds)
	}

	// Python continues the mission on top of the Go-written history; Go reads it back.
	runPython(t, project, pyCheckpoint, dir)
	if err := json.Unmarshal(runPython(t, project, pyReadSnapshot, dir), &py); err != nil {
		t.Fatal(err)
	}
	snap := must(NewGitMissionAnchor(dir).ReadSituationalAwareness(ctx))
	sameJSON(t, "snapshot after a Python checkpoint", snap, py.Snapshot)
	if !snap.IsComplete || len(snap.LastDecisions) != 2 || snap.LastDecisions[1].Decision != "py <&> \u2028 ok" {
		t.Fatalf("%+v", snap)
	}
	if strings.Join(py.EventKinds, ",") != "started,cycle,py_event" {
		t.Fatalf("python saw events %v", py.EventKinds)
	}
}

const pyWriteAnchor = `
import asyncio, sys
from lha.contracts.state import Checklist, ChecklistItem, Checkpoint, DecisionRecord, EventRecord
from lha.state.mission_anchor import GitMissionAnchor
async def main():
    a = GitMissionAnchor(sys.argv[1])
    items = Checklist(items=[
        ChecklistItem(id="01", description="add <model> & ü"),
        ChecklistItem(id="02", description="add tests", depends_on=["01"], notes="a\u2028b\x85c"),
    ])
    await a.initialize(title="Py mission", description="Made by Python.", items=items, acceptance="green")
    await a.append_event(EventRecord(kind="started", cycle_id="c1"))
    items.items[0].status = "done"
    items.items[0].verified_by = ["pytest", "ruff"]
    await a.commit_checkpoint(Checkpoint(
        cycle_id="c1", progress_summary="- did 01", checklist=items,
        decisions=[DecisionRecord(decision="d \u2028 <&>", rationale="r", alternatives_rejected="x", affected=["a.py"], cycle_id="c1")],
        events=[EventRecord(kind="cycle", cycle_id="c1", payload={"id": "01", "n": 1, "nested": {"l": [1, "two"]}, "ok": True})],
    ))
    snap = await a.read_situational_awareness()
    print(snap.model_dump_json())
asyncio.run(main())
`

func TestCrossImplPythonWritesGoReads(t *testing.T) {
	project := pythonProject(t)
	ctx := context.Background()
	dir := t.TempDir()
	pySnap := runPython(t, project, pyWriteAnchor, dir)

	anchor := NewGitMissionAnchor(dir)
	snap := must(anchor.ReadSituationalAwareness(ctx))
	sameJSON(t, "snapshot (Python-written, Go-read vs Python-read)", snap, pySnap)
	if snap.Mission == nil || snap.Mission.Acceptance != "green" || snap.ActiveItem == nil || snap.ActiveItem.ID != "02" {
		t.Fatalf("%+v", snap)
	}

	// Byte parity: Go re-serializes what Python wrote to exactly the same bytes. (Event payload
	// keys are written in sorted order here: Go's map[string]any payload cannot keep Python's
	// insertion order, the one known byte-level difference; the JSON is semantically equal.)
	checkBytes := func(rel string, v any, indent bool) {
		t.Helper()
		committed := must(ShowAtHead(ctx, dir, rel))
		if err := json.Unmarshal([]byte(committed), v); err != nil {
			t.Fatal(err)
		}
		got := must(pydanticJSON(v, indent))
		if string(got) != committed {
			t.Errorf("%s: Go re-serialization differs\n go:     %q\n python: %q", rel, got, committed)
		}
	}
	checkBytes(".lha/checklist.json", &contracts.Checklist{}, true)
	checkBytes(".lha/mission.json", &contracts.MissionSpec{}, true)
	for _, name := range []string{DecisionsFile, EventsFile} {
		text := must(ShowAtHead(ctx, dir, ".lha/"+name))
		for _, ln := range strings.Split(text, "\n") {
			var v any = &contracts.DecisionRecord{}
			if name == EventsFile {
				v = &contracts.EventRecord{}
			}
			if err := json.Unmarshal([]byte(ln), v); err != nil {
				t.Fatal(err)
			}
			if got := must(pydanticJSON(v, false)); string(got) != ln {
				t.Errorf("%s line: Go re-serialization differs\n go:     %q\n python: %q", name, got, ln)
			}
		}
	}

	// Go continues the Python mission; the pending/committed logs stay exactly-once.
	cl := must(anchor.ReadChecklist(ctx))
	if _, err := cl.RecordSuccess("02", []string{"pytest"}); err != nil {
		t.Fatal(err)
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c2", ProgressSummary: "- go did 02", Checklist: cl,
		Events: []contracts.EventRecord{{Kind: "go_event", CycleID: "c2"}}}))
	var py struct {
		Snapshot   json.RawMessage `json:"snapshot"`
		EventKinds []string        `json:"event_kinds"`
	}
	if err := json.Unmarshal(runPython(t, project, pyReadSnapshot, dir), &py); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, "snapshot after a Go checkpoint", must(anchor.ReadSituationalAwareness(ctx)), py.Snapshot)
	if strings.Join(py.EventKinds, ",") != "started,cycle,go_event" {
		t.Fatalf("python saw events %v", py.EventKinds)
	}
}
