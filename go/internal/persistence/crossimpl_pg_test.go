package persistence

import (
	"bytes"
	"context"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence/pgtest"
)

// pyPG migrates a Postgres database with Python's `lha db migrate` code (printing the versions it
// applied) and, with "write", performs pyWrite's writes (text-only memory) with PostgresStore.
const pyPG = `
import asyncio, json, sys
from lha.contracts.memory import MemoryRecord
from lha.governor.cost import CostEntry
from lha.memory.skills import Skill
from lha.persistence.db import apply_migrations
from lha.persistence.postgres import PostgresStore
from lha.persistence.store import GateEvent

async def main(dsn, migrations, mode):
    print(json.dumps(await apply_migrations(dsn, migrations_dir=migrations)))
    if mode != "write":
        return
    s = PostgresStore(dsn)
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
    await s.put_memory("m1", [MemoryRecord(id="fact:1", kind="fact", text="alpha é", metadata={"item_id": "01", "z": "y"})])
    await s.put_memory("m1", [MemoryRecord(id="progress:1", kind="progress", text="beta")])
    await s.invalidate_memory(["progress:1"])
    await s.put_skill(Skill(id="s1", name="n", description="desc", code="c", preconditions=["p1", "p2"], namespace="/repo", provenance="m1:c1:abc", expires_at="2099-01-01", verified=True, uses=2))
    await s.put_skill(Skill(id="g", name="g", description="global", code="", namespace="global", verified=True))
    await s.close()

asyncio.run(main(sys.argv[1], sys.argv[2], sys.argv[3]))
`

// TestPostgresStoreIsInterchangeableWithPython: the `lha db migrate` bookkeeping is shared (each
// implementation sees the other's migrations as applied) and each reads what the other wrote.
// Needs LHA_IT_POSTGRES_DSN and uv (it runs Python with the postgres extra).
func TestPostgresStoreIsInterchangeableWithPython(t *testing.T) {
	pyDSN := pgtest.FreshDB(t)
	goDSN := pgtest.FreshDB(t)
	project := pythonProject(t)
	extra := []string{"--extra", "postgres"}
	ctx := context.Background()
	migrations := pgtest.MigrationsDir()

	// Python migrates and writes; Go finds nothing left to migrate and reads it.
	sameJSON(t, "python migrate", RequiredPGMigrations, runPythonWith(t, project, extra, pyPG, pyDSN, migrations, "write"))
	if applied := must(ApplyMigrations(ctx, pyDSN, migrations)); len(applied) != 0 {
		t.Fatalf("go re-applied %v", applied)
	}
	// Go migrates and writes; Python finds nothing left to migrate.
	if applied := must(ApplyMigrations(ctx, goDSN, migrations)); !reflect.DeepEqual(applied, RequiredPGMigrations) {
		t.Fatal(applied)
	}
	goStore := must(OpenPostgres(ctx, goDSN))
	goWriteStore(t, goStore)
	goStore.Close()
	sameJSON(t, "python re-migrate", []string{}, runPythonWith(t, project, extra, pyPG, goDSN, migrations, "migrate"))

	for _, dsn := range []string{pyDSN, goDSN} {
		want := runPythonWith(t, project, extra, pyRead, "postgres", dsn)
		store := must(OpenPostgres(ctx, dsn))
		got := goReadStore(t, store)
		store.Close()
		if got["replay"] != false {
			t.Fatal("a replayed ledger write was recorded twice")
		}
		sameJSON(t, "read postgres", got, want)
	}
}

func runPythonWith(t *testing.T, project string, uvArgs []string, script string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	argv := append([]string{"run", "--quiet", "--project", project}, uvArgs...)
	argv = append(append(argv, "python", "-c", script), args...)
	cmd := exec.CommandContext(ctx, "uv", argv...)
	cmd.Dir = project
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, stderr.String())
	}
	return out
}
