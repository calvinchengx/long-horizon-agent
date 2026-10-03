package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
)

// These tests prove the SQLite mission store is interchangeable between the Go and the Python
// implementation: the same writes produce the same rows (byte-identical JSON columns and ledger
// keys), each reads what the other wrote, and a ledger write replayed by the other implementation
// is still a no-op.

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

// pyWrite performs the fixture's writes with Python's SqliteStore.
const pyWrite = `
import asyncio, sys
from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.memory.skills import Skill
from lha.persistence.sqlite import SqliteStore
from lha.persistence.store import GateEvent

async def main(path):
    s = SqliteStore(path)
    await s.open()
    await s.upsert_mission(mission_id="m1", title="Ünïcode title", description="d", status="RUNNING", workflow_id="mission:m1")
    await s.upsert_mission(mission_id="m1", title="", status="DONE", head_sha="abc")
    await s.upsert_mission(mission_id="m1", title="", status="RUNNING")
    base = dict(mission_id="m1", gate_id="m1:tool:ab12", kind="tool_call", question="push? é",
                options=["approve", "reject"], default_action="reject", risk="irreversible",
                request={"tool": "run_command", "argv": '["git", "push"]'})
    await s.record_gate_event(GateEvent(event="opened", at="2026-01-01T00:00:00+00:00", **base))
    await s.record_gate_event(GateEvent(event="reminder", at="2026-01-01T00:01:00+00:00", step=1, **base))
    await s.record_gate_event(GateEvent(event="resolved", at="2026-01-01T00:05:00+00:00", decision="approve", resolved_by="terminal:me", **base))
    await s.record_gate_event(GateEvent(mission_id="m2", gate_id="deadlock-1", kind="deadlock", event="opened",
        at="2026-01-02T00:00:00+00:00", question="q", options=["retry", "abort"], default_action="abort",
        deadline="2026-01-02T01:00:00+00:00"))
    await s.record_cost("m1", CostEntry(cycle_id="c1", model="claude-x", input_tokens=100, output_tokens=20, usd=0.12, role="lead"), call_key="k#0")
    await s.record_cost("m1", CostEntry(cycle_id="c1", model="x", input_tokens=1, output_tokens=2, usd=0.0, cost_known=False, role="researcher"), call_key="k#1")
    await s.append_event("m1", cycle_id="c1", kind="cycle_outcome", payload={"item_id": "01", "attempts": 2, "verified": True, "tools": ["a", "b"], "summary": "é ok", "n": None})
    await s.append_event("m1", cycle_id="c2", kind="memory_consolidation", payload={"upto_id": 1, "mode": "extractive"})
    from lha.persistence.store import MissionEvent
    await s.append_mission_events([
        MissionEvent("m1", "c1", "tool_call", {"tool": "grep", "ok": False, "error": "bad é", "n": None}, ts="2026-01-03T00:00:00+00:00"),
        MissionEvent("m2", "", "cycle_started", {"item_id": "07"}, ts="2026-01-03T00:00:01+00:00"),
    ])
    await s.put_memory("m1", [MemoryRecord(id="fact:1", kind="fact", text="alpha é", metadata={"item_id": "01", "z": "y"})], vectors=[[0.1, 1.0, 1e-07]], embedding_model="hash", embedding_version="1")
    await s.put_memory("m1", [MemoryRecord(id="progress:1", kind="progress", text="beta")])
    await s.invalidate_memory(["progress:1"])
    await s.put_skill(Skill(id="s1", name="n", description="desc", code="c", preconditions=["p1", "p2"], namespace="/repo", provenance="m1:c1:abc", expires_at="2099-01-01", verified=True, uses=2))
    await s.put_skill(Skill(id="g", name="g", description="global", code="", namespace="global", verified=True))
    await s.close()

asyncio.run(main(sys.argv[1]))
`

// goWrite performs the same writes with the Go SQLiteStore.
func goWrite(t *testing.T, path string) {
	t.Helper()
	s := must(OpenSQLite(context.Background(), path))
	defer s.Close()
	goWriteStore(t, s)
}

// goWriteStore performs the fixture's writes on s (vectors only on SQLite: on Postgres they go
// through pgvector, not the store).
func goWriteStore(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	var emb *Embedding
	if s.Backend() == BackendSQLite {
		emb = &Embedding{Vectors: [][]float64{{0.1, 1.0, 1e-07}}, Model: "hash", Version: "1"}
	}
	ok(t, s.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Title: "Ünïcode title", Description: "d", Status: "RUNNING", WorkflowID: "mission:m1"}))
	ok(t, s.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: "DONE", HeadSHA: "abc"}))
	ok(t, s.UpsertMission(ctx, MissionUpsert{MissionID: "m1", Status: "RUNNING"}))
	base := GateEvent{MissionID: "m1", GateID: "m1:tool:ab12", Kind: "tool_call", Question: "push? é",
		Options: []string{"approve", "reject"}, DefaultAction: "reject", Risk: "irreversible",
		Request: map[string]string{"tool": "run_command", "argv": `["git", "push"]`}}
	ev := func(event, at string, step int, decision, by string) GateEvent {
		e := base
		e.Event, e.At, e.Step, e.Decision, e.ResolvedBy = event, at, step, decision, by
		return e
	}
	ok(t, s.RecordGateEvent(ctx, ev("opened", "2026-01-01T00:00:00+00:00", 0, "", "")))
	ok(t, s.RecordGateEvent(ctx, ev("reminder", "2026-01-01T00:01:00+00:00", 1, "", "")))
	ok(t, s.RecordGateEvent(ctx, ev("resolved", "2026-01-01T00:05:00+00:00", 0, "approve", "terminal:me")))
	ok(t, s.RecordGateEvent(ctx, GateEvent{MissionID: "m2", GateID: "deadlock-1", Kind: "deadlock", Event: "opened",
		At: "2026-01-02T00:00:00+00:00", Question: "q", Options: []string{"retry", "abort"}, DefaultAction: "abort",
		Deadline: "2026-01-02T01:00:00+00:00"}))
	must(s.RecordCost(ctx, "m1", governor.CostEntry{CycleID: "c1", Model: "claude-x", InputTokens: 100, OutputTokens: 20, USD: 0.12, CostKnown: true, Role: "lead"}, "k#0"))
	must(s.RecordCost(ctx, "m1", governor.CostEntry{CycleID: "c1", Model: "x", InputTokens: 1, OutputTokens: 2, Role: "researcher"}, "k#1"))
	must(s.AppendEvent(ctx, "m1", "c1", "cycle_outcome", map[string]any{"item_id": "01", "attempts": 2, "verified": true,
		"tools": []string{"a", "b"}, "summary": "é ok", "n": nil}))
	must(s.AppendEvent(ctx, "m1", "c2", "memory_consolidation", map[string]any{"upto_id": int64(1), "mode": "extractive"}))
	ok(t, s.AppendMissionEvents(ctx, []MissionEvent{
		{MissionID: "m1", CycleID: "c1", Kind: "tool_call", Payload: map[string]any{"tool": "grep", "ok": false, "error": "bad é", "n": nil}, TS: "2026-01-03T00:00:00+00:00"},
		{MissionID: "m2", Kind: "cycle_started", Payload: map[string]any{"item_id": "07"}, TS: "2026-01-03T00:00:01+00:00"},
	}))
	ok(t, s.PutMemory(ctx, "m1", []contracts.MemoryRecord{contracts.NewMemoryRecord("fact:1", "fact", "alpha é",
		map[string]string{"item_id": "01", "z": "y"})}, emb))
	ok(t, s.PutMemory(ctx, "m1", []contracts.MemoryRecord{contracts.NewMemoryRecord("progress:1", "progress", "beta", nil)}, nil))
	must(s.InvalidateMemory(ctx, []string{"progress:1"}))
	ok(t, s.PutSkill(ctx, contracts.Skill{ID: "s1", Name: "n", Description: "desc", Code: "c", Preconditions: []string{"p1", "p2"},
		Namespace: "/repo", Provenance: "m1:c1:abc", ExpiresAt: "2099-01-01", Verified: true, Uses: 2}))
	ok(t, s.PutSkill(ctx, contracts.Skill{ID: "g", Name: "g", Description: "global", Namespace: "global", Verified: true}))
}

// pyRaw dumps every deterministic column (timestamps excluded) with Python's sqlite3.
const pyRaw = `
import json, sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
queries = {
    "schema_migrations": "SELECT version FROM schema_migrations ORDER BY version",
    "missions": "SELECT mission_id, title, description, acceptance, status, workflow_id, run_id, head_sha, schema_version FROM missions",
    "hitl_gates": "SELECT mission_id, gate_id, kind, question, risk, default_action, options, request, status, deadline, decision, resolved_by, reminders, created_at, resolved_at FROM hitl_gates ORDER BY mission_id, gate_id",
    "cost_ledger": "SELECT mission_id, cycle_id, model, input_tokens, output_tokens, usd, idempotency_key, role, cost_known FROM cost_ledger ORDER BY id",
    "episodic_events": "SELECT id, mission_id, cycle_id, kind, payload, payload_ref, schema_version FROM episodic_events ORDER BY id",
    "mission_events": "SELECT id, mission_id, cycle_id, ts, kind, payload, schema_version FROM mission_events ORDER BY id",
    "semantic_memory": "SELECT id, mission_id, kind, text, metadata, embedding, embedding_model, embedding_version, valid, source_event_id FROM semantic_memory ORDER BY rowid",
    "skills": "SELECT id, namespace, name, description, code, preconditions, provenance, expires_at, verified, uses FROM skills ORDER BY id",
}
print(json.dumps({k: [list(r) for r in conn.execute(q)] for k, q in queries.items()}))
`

// pyRead reads a store with Python's SqliteStore and dumps what it returns.
const pyRead = `
import asyncio, json, sys
from lha.persistence.sqlite import SqliteStore

async def main(backend, where):
    if backend == "postgres":
        from lha.persistence.postgres import PostgresStore
        s = PostgresStore(where)
    else:
        s = SqliteStore(where)
    await s.open()
    vectors = []
    if backend == "sqlite":
        vectors = [[r.id, v] for r, v in await s.memory_vectors("m1", embedding_model="hash", embedding_version="1")]
    out = {
        "missions": [[m.mission_id, m.title, m.status, m.description, m.head_sha, m.workflow_id] for m in await s.list_missions()],
        "gates": [[g.mission_id, g.gate_id, g.kind, g.status, g.question, g.options, g.default_action, g.risk, g.deadline, g.decision, g.resolved_by, g.reminders, g.request, g.opened_at, g.resolved_at] for g in await s.list_gates()],
        "costs": [[c.mission_id, c.cycle_id, c.model, c.role, c.input_tokens, c.output_tokens, c.usd, c.cost_known] for c in await s.list_costs("m1")],
        "summary": vars(await s.cost_summary("m1")),
        "events": [[e.id, e.cycle_id, e.kind, e.payload] for e in await s.list_events("m1")],
        "mission_events": [[e.id, e.mission_id, e.cycle_id, e.kind, e.payload, e.ts] for e in await s.read_mission_events()],
        "memory": [[r.id, r.kind, r.text, r.metadata, r.embedding_model, r.embedding_version, r.valid] for r in await s.list_memory("m1")],
        "vectors": vectors,
        "skills": [[k.id, k.name, k.description, k.code, k.preconditions, k.namespace, k.provenance, k.expires_at, k.verified, k.uses] for k in await s.list_skills("/repo")],
        "replay": await s.record_cost("m1", __import__("lha.governor.cost", fromlist=["CostEntry"]).CostEntry(cycle_id="c1", model="claude-x", input_tokens=100, output_tokens=20, usd=0.12, role="lead"), call_key="k#0"),
    }
    await s.close()
    print(json.dumps(out))

asyncio.run(main(sys.argv[1], sys.argv[2]))
`

func nilIfEmpty(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// goRead is pyRead with the Go store.
func goRead(t *testing.T, path string) map[string]any {
	t.Helper()
	s := must(OpenSQLite(context.Background(), path))
	defer s.Close()
	return goReadStore(t, s)
}

func goReadStore(t *testing.T, s Store) map[string]any {
	t.Helper()
	ctx := context.Background()
	optional := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	out := map[string]any{}
	missions := []any{}
	for _, m := range must(s.ListMissions(ctx, 0)) {
		missions = append(missions, []any{m.MissionID, m.Title, m.Status, m.Description, optional(m.HeadSHA), optional(m.WorkflowID)})
	}
	out["missions"] = missions
	gates := []any{}
	for _, g := range must(s.ListGates(ctx, "", 0)) {
		var request any
		if g.Request != nil {
			request = g.Request
		}
		gates = append(gates, []any{g.MissionID, g.GateID, g.Kind, g.Status, g.Question, g.Options, g.DefaultAction, g.Risk,
			g.Deadline, nilIfEmpty(g.Decision), nilIfEmpty(g.ResolvedBy), g.Reminders, request, g.OpenedAt, g.ResolvedAt})
	}
	out["gates"] = gates
	costs := []any{}
	for _, c := range must(s.ListCosts(ctx, "m1", 0)) {
		var usd any
		if c.USD != nil {
			usd = *c.USD
		}
		costs = append(costs, []any{c.MissionID, c.CycleID, c.Model, c.Role, c.InputTokens, c.OutputTokens, usd, c.CostKnown})
	}
	out["costs"] = costs
	sum := must(s.CostSummary(ctx, "m1"))
	out["summary"] = map[string]any{"mission_id": sum.MissionID, "calls": sum.Calls, "known_usd": sum.KnownUSD,
		"unknown_cost_calls": sum.UnknownCostCalls, "input_tokens": sum.InputTokens, "output_tokens": sum.OutputTokens}
	events := []any{}
	for _, e := range must(s.ListEvents(ctx, "m1", EventQuery{})) {
		events = append(events, []any{e.ID, e.CycleID, e.Kind, e.Payload})
	}
	out["events"] = events
	missionEvents := []any{}
	for _, e := range must(s.ReadMissionEvents(ctx, "", 0, 0)) {
		missionEvents = append(missionEvents, []any{e.ID, e.MissionID, e.CycleID, e.Kind, e.Payload, e.TS})
	}
	out["mission_events"] = missionEvents
	memory := []any{}
	for _, r := range must(s.ListMemory(ctx, "m1", 0)) {
		memory = append(memory, []any{r.ID, r.Kind, r.Text, r.Metadata, r.EmbeddingModel, r.EmbeddingVersion, r.Valid})
	}
	out["memory"] = memory
	vectors := []any{}
	if lite, isSQLite := s.(*SQLiteStore); isSQLite {
		for _, v := range must(lite.MemoryVectors(ctx, "m1", "hash", "1", 0)) {
			vectors = append(vectors, []any{v.Record.ID, v.Vector})
		}
	}
	out["vectors"] = vectors
	skills := []any{}
	for _, k := range must(s.ListSkills(ctx, "/repo", 0)) {
		skills = append(skills, []any{k.ID, k.Name, k.Description, k.Code, k.Preconditions, k.Namespace, k.Provenance,
			optional(k.ExpiresAt), k.Verified, k.Uses})
	}
	out["skills"] = skills
	out["replay"] = must(s.RecordCost(ctx, "m1", governor.CostEntry{CycleID: "c1", Model: "claude-x", InputTokens: 100,
		OutputTokens: 20, USD: 0.12, CostKnown: true, Role: "lead"}, "k#0"))
	return out
}

func normalize(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameJSON(t *testing.T, label string, got any, want []byte) {
	t.Helper()
	var w any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: %v in %s", label, err, want)
	}
	if g := normalize(t, got); !reflect.DeepEqual(g, w) {
		gj, _ := json.MarshalIndent(g, "", " ")
		wj, _ := json.MarshalIndent(w, "", " ")
		t.Fatalf("%s differs:\n--- go\n%s\n--- python\n%s", label, gj, wj)
	}
}

func TestSQLiteStoreIsInterchangeableWithPython(t *testing.T) {
	project := pythonProject(t)
	dir := t.TempDir()
	pyPath, goPath := filepath.Join(dir, "py.sqlite3"), filepath.Join(dir, "go.sqlite3")
	runPython(t, project, pyWrite, pyPath)
	goWrite(t, goPath)

	// The same writes leave the same rows: JSON columns, ledger keys and migration ids included.
	sameJSON(t, "raw rows", normalize(t, json.RawMessage(runPython(t, project, pyRaw, goPath))), runPython(t, project, pyRaw, pyPath))

	// Each implementation reads (and replays a ledger write into) the other's file.
	for _, path := range []string{pyPath, goPath} {
		want := runPython(t, project, pyRead, "sqlite", path)
		got := goRead(t, path)
		// Python's replay ran first on the file: both then see an already-recorded key.
		if got["replay"] != false {
			t.Fatalf("%s: a replayed ledger write was recorded twice", path)
		}
		sameJSON(t, "read "+filepath.Base(path), got, want)
	}
}
