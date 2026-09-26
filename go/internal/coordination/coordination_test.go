package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ports of python/tests/unit/test_coordination.py, test_coord_fixes.py (tickets, ownership) and
// the lease halves of test_leases_and_resume.py / test_ownership_integration.py.

// --- tickets -----------------------------------------------------------------------------------

func TestTicketTransitionIsPureAndCountsAttempts(t *testing.T) {
	ticket := NewTicket("t", TaskContract{Objective: "x"}, "", 0)
	advanced, err := ticket.Transition(TicketInProgress)
	if err != nil || advanced.Status != TicketInProgress || ticket.Status != TicketCreated {
		t.Fatalf("%v %v %v", advanced, ticket, err)
	}
	cur := ticket
	for _, to := range []TicketStatus{TicketInProgress, TicketAwaitingVerify, TicketInProgress,
		TicketAwaitingVerify, TicketAwaitingMerge, TicketDone} {
		if cur, err = cur.Transition(to); err != nil {
			t.Fatal(err)
		}
	}
	if cur.Status != TicketDone || cur.Attempts != 2 {
		t.Fatalf("%+v", cur)
	}
}

func TestIllegalTicketTransitions(t *testing.T) {
	for _, c := range [][2]TicketStatus{
		{TicketCreated, TicketDone}, {TicketDone, TicketCreated}, {TicketFailed, TicketInProgress},
		{TicketInProgress, TicketAwaitingMerge}, {TicketInProgress, TicketInProgress},
	} {
		ticket := Ticket{ID: "t", Status: c[0]}
		_, err := ticket.Transition(c[1])
		var illegal *IllegalTransitionError
		if !errors.As(err, &illegal) || !strings.Contains(err.Error(), "illegal transition "+string(c[0])+" -> "+string(c[1])) {
			t.Errorf("%v -> %v: %v", c[0], c[1], err)
		}
	}
}

// --- the ownership map -------------------------------------------------------------------------

func shared(t *testing.T, p string) bool {
	t.Helper()
	ok, err := IsShared(p)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestSharedFilesDetected(t *testing.T) {
	for _, p := range []string{"pyproject.toml", "uv.lock", "src/pkg/__init__.py", "tests/conftest.py",
		"app/db/migrations/0001_init.py", "Cargo.toml", "PYPROJECT.TOML", "services/api/pyproject.toml",
		"crates/core/Cargo.toml", "tools/go.mod", "package.json", "web/package-lock.json", "pnpm-lock.yaml",
		"yarn.lock", "setup.py", "setup.cfg", "requirements.txt", "requirements-dev.txt", "./uv.lock",
		"app/Migrations/0002.py"} {
		if !shared(t, p) {
			t.Errorf("%s should be shared", p)
		}
	}
	for _, p := range []string{"src/pkg/feature.py", "src/requirements_parser.py", ".env"} {
		if shared(t, p) {
			t.Errorf("%s should not be shared", p)
		}
	}
}

func TestOwnershipSingleWriterPerFile(t *testing.T) {
	owners := NewFileOwnershipMap()
	if err := owners.Assign("src/pkg/feature_a.py", "impl-1"); err != nil {
		t.Fatal(err)
	}
	if !owners.Permits("impl-1", "src/pkg/feature_a.py") || owners.Permits("impl-2", "src/pkg/feature_a.py") ||
		owners.Permits(Lead, "src/pkg/feature_a.py") {
		t.Fatal("assigned file")
	}
	if !owners.Permits(Lead, "src/pkg/other.py") || owners.Permits("impl-1", "src/pkg/other.py") {
		t.Fatal("unassigned space belongs to the lead")
	}
	if err := owners.Assign("pyproject.toml", "impl-1"); err == nil || !strings.Contains(err.Error(), "lead") {
		t.Fatalf("shared file assigned: %v", err)
	}
	if err := owners.Assign("Package.JSON", "impl-1"); err == nil {
		t.Fatal("case trick")
	}
	if !owners.Permits(Lead, "pyproject.toml") || owners.Permits("impl-1", "pyproject.toml") {
		t.Fatal("shared files are the lead's")
	}
	var conflict *OwnershipConflictError
	if err := owners.Assign("SRC/pkg/feature_a.py", "impl-2"); !errors.As(err, &conflict) {
		t.Fatalf("stolen: %v", err)
	}
	if prev, err := owners.Reassign("src/pkg/feature_a.py", "impl-2"); err != nil || prev != "impl-1" {
		t.Fatalf("%q %v", prev, err)
	}
	violations := owners.Violations("impl-1", []string{"src/pkg/b.py", "pyproject.toml"})
	if len(violations) != 2 || violations[0].Path != "src/pkg/b.py" || violations[1].Path != "pyproject.toml" {
		t.Fatalf("%+v", violations)
	}
}

func TestNormalizationKeepsLeadingDotsAndRejectsEscapes(t *testing.T) {
	owners := NewFileOwnershipMap()
	_ = owners.Assign(".env", "impl-1")
	if owners.OwnerOf("./.env") != "impl-1" || owners.OwnerOf("env") != "" || owners.OwnerOf(`.hidden\..\.env`) != "impl-1" {
		t.Fatal("leading dots")
	}
	_ = owners.Assign("src/a.py", "impl-1")
	if !owners.Permits("impl-1", "src/tmp/../a.py") || owners.Permits("impl-1", "../outside.py") || owners.Permits(Lead, "/etc/passwd") {
		t.Fatal("dotdot")
	}
	var invalid *InvalidPathError
	if err := owners.Assign("../../x.py", "impl-1"); !errors.As(err, &invalid) {
		t.Fatalf("%v", err)
	}
	if bad := owners.Violations("impl-1", []string{"../x"}); len(bad) != 1 || bad[0].Path != "../x" {
		t.Fatalf("%+v", bad)
	}
}

func TestReleaseAndEffectiveOwnership(t *testing.T) {
	owners := NewFileOwnershipMap()
	_ = owners.Assign("a.py", "implementer-01")
	_ = owners.Assign("b.py", "implementer-02")
	if !owners.PermitsAny([]string{Lead, "implementer-01"}, "a.py") || owners.PermitsAny([]string{Lead, "implementer-01"}, "b.py") {
		t.Fatal("permits_any")
	}
	v := owners.ViolationsAny([]string{Lead, "implementer-01"}, []string{"a.py", "b.py"})
	if len(v) != 1 || v[0].Path != "b.py" || v[0].Owner != "implementer-02" || v[0].Writer != "lead+implementer-01" {
		t.Fatalf("%+v", v)
	}
	effective := EffectiveOwnership(owners, []string{"implementer-01", Lead})
	if !reflect.DeepEqual(effective.Snapshot(), map[string]string{"b.py": "implementer-02"}) || len(owners.Snapshot()) != 2 {
		t.Fatalf("%v %v", effective.Snapshot(), owners.Snapshot())
	}
	if got := owners.Release("implementer-02"); !reflect.DeepEqual(got, []string{"b.py"}) || len(owners.Release("nobody")) != 0 {
		t.Fatalf("%v", got)
	}
	data, err := OwnershipJSON(effective)
	if err != nil || string(data) != "{\n  \"owners\": {\n    \"b.py\": \"implementer-02\"\n  }\n}" {
		t.Fatalf("%s %v", data, err)
	}
	empty, _ := OwnershipJSON(NewFileOwnershipMap())
	if string(empty) != "{\n  \"owners\": {}\n}" {
		t.Fatalf("%s", empty)
	}
}

// --- the guard -----------------------------------------------------------------------------------

type fakeInner struct{ calls []string }

func (f *fakeInner) Specs() []contracts.ToolSpec {
	return []contracts.ToolSpec{
		{Name: "write_file", Mutating: true, PathArgs: []string{"path"}},
		{Name: "read_file", PathArgs: []string{"path"}},
		{Name: "run_command", Mutating: true},
	}
}

func (f *fakeInner) Dispatch(_ context.Context, call contracts.ToolCall, _ contracts.ToolContext) contracts.ToolResult {
	f.calls = append(f.calls, call.Name)
	return contracts.Success("ok")
}

func TestOwnershipGuardRefusesForeignWrites(t *testing.T) {
	owners := NewFileOwnershipMap()
	_ = owners.Assign("mine.py", "implementer-01")
	_ = owners.Assign("theirs.py", "implementer-02")
	inner := &fakeInner{}
	guard := NewOwnershipGuard(inner, owners, []string{"implementer-01"}, false)
	write := func(p string) (bool, string) {
		r := guard.Dispatch(context.Background(), contracts.ToolCall{Name: "write_file", Arguments: map[string]any{"path": p}}, contracts.ToolContext{})
		return r.OK, r.ErrorText()
	}
	if ok, _ := write("mine.py"); !ok {
		t.Fatal("own file")
	}
	for p, want := range map[string]string{
		"./theirs.py": "owned by 'implementer-02'", "pyproject.toml": "shared file",
		"new.py": "outside your write-set", "../x.py": "escapes the repository root",
	} {
		if ok, msg := write(p); ok || !strings.Contains(msg, want) {
			t.Errorf("%s: %v %q", p, ok, msg)
		}
	}
	if ok, msg := write("theirs.py"); ok || !strings.Contains(msg, "lease") {
		t.Fatalf("%q", msg)
	}
	read := guard.Dispatch(context.Background(), contracts.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "theirs.py"}}, contracts.ToolContext{})
	shell := guard.Dispatch(context.Background(), contracts.ToolCall{Name: "run_command", Arguments: map[string]any{}}, contracts.ToolContext{})
	if !read.OK || !shell.OK || !reflect.DeepEqual(guard.Specs(), inner.Specs()) {
		t.Fatal("reads and argv-only tools are not path-checked")
	}
	guard.Update(owners, []string{Lead, "implementer-02"})
	if !reflect.DeepEqual(guard.Writers(), []string{Lead, "implementer-02"}) {
		t.Fatal(guard.Writers())
	}
	if ok, _ := write("theirs.py"); !ok {
		t.Fatal("theirs")
	}
	if ok, _ := write("new.py"); !ok {
		t.Fatal("new")
	}
	if ok, _ := write("mine.py"); ok {
		t.Fatal("mine")
	}
	if len(guard.DrainEvents()) != 0 {
		t.Fatal("events")
	}
}

// --- leases --------------------------------------------------------------------------------------

func TestDecideLeaseGrantsFreeFilesAndRefusesContendedOnes(t *testing.T) {
	owners := NewFileOwnershipMap()
	_ = owners.Assign("a.py", "implementer-01")
	_ = owners.Assign("b.py", "implementer-02")
	ask := func(writer, p string, finished ...string) LeaseDecision {
		return DecideLease(owners, LeaseRequest{Writer: writer, Path: p, Reason: " why "}, finished)
	}
	free := ask("implementer-01", "src/new.py")
	if !free.Granted || free.HasPrevious || free.Why != "it was unassigned" || free.Reason != "why" || !strings.Contains(free.Message(), "lease granted") {
		t.Fatalf("%+v", free)
	}
	if ask("implementer-01", "./a.py").Why != "you already own it" {
		t.Fatal("own")
	}
	taken := ask("implementer-01", "b.py")
	if taken.Granted || taken.PreviousOwner != "implementer-02" || !strings.Contains(taken.Why, "still open") || !strings.Contains(taken.Message(), "lease refused") {
		t.Fatalf("%+v", taken)
	}
	done := ask("implementer-01", "B.py", "implementer-02")
	if !done.Granted || done.Path != "B.py" || !strings.Contains(done.Why, "has finished") {
		t.Fatalf("%+v", done)
	}
	for _, p := range []string{"pyproject.toml", ".lha/ownership.json", ".git/config"} {
		if ask("implementer-01", p).Granted {
			t.Errorf("%s granted", p)
		}
	}
	if !strings.Contains(ask("implementer-01", "../x.py").Why, "escapes") || ask(Lead, "c.py").Granted {
		t.Fatal("invalid / lead")
	}
	finished := FinishedWriters(contracts.Checklist{Items: []contracts.ChecklistItem{
		{ID: "01", Status: "done"}, {ID: "02", Status: "split"}, {ID: "03", Status: "todo"},
	}})
	if !reflect.DeepEqual(finished, []string{"implementer-01", "implementer-02"}) {
		t.Fatal(finished)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func twoItems() (contracts.Checklist, *FileOwnershipMap) {
	checklist := contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{
		contracts.NewChecklistItem("01", "write a"), contracts.NewChecklistItem("02", "write b"),
	}}
	owners := NewFileOwnershipMap()
	_ = owners.Assign("a.py", WriterForItem("01"))
	_ = owners.Assign("b.py", WriterForItem("02"))
	return checklist, owners
}

func initAnchor(t *testing.T, dir string, checklist contracts.Checklist, owners *FileOwnershipMap) *state.GitMissionAnchor {
	t.Helper()
	anchor := state.NewGitMissionAnchor(dir)
	var data []byte
	if owners != nil {
		var err error
		if data, err = OwnershipJSON(owners); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := anchor.InitializeSpecWithOwnership(context.Background(), contracts.MissionSpec{Title: "T", Description: "D"}, checklist, data); err != nil {
		t.Fatal(err)
	}
	return anchor
}

func events(t *testing.T, dir, kind string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(git(t, dir, "show", "HEAD:.lha/events.ndjson"), "\n") {
		var ev struct {
			Kind    string         `json:"kind"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Kind == kind {
			out = append(out, ev.Payload)
		}
	}
	return out
}

func TestAnchorPersistsTheOwnershipMap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	anchor := initAnchor(t, dir, checklist, owners)
	got, err := ReadOwnership(ctx, state.NewGitMissionAnchor(dir))
	if err != nil || !got.Equal(owners) {
		t.Fatalf("%v %v", got.Snapshot(), err)
	}
	// The agent editing ownership.json is discarded; a staged map is what gets committed.
	_ = os.WriteFile(filepath.Join(dir, ".lha", "ownership.json"), []byte(`{"owners": {"a.py": "lead"}}`), 0o644)
	if err := StageOwnership(anchor, NewFileOwnershipMap()); err != nil {
		t.Fatal(err)
	}
	cp := contracts.Checkpoint{CycleID: "c1", ProgressSummary: "- c1", Checklist: checklist}
	if _, err := anchor.CommitCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadOwnership(ctx, anchor); len(got.Snapshot()) != 0 {
		t.Fatal(got.Snapshot())
	}
	cp.CycleID = "c2"
	if _, err := anchor.CommitCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadOwnership(ctx, anchor); len(got.Snapshot()) != 0 {
		t.Fatal("the staging is not sticky")
	}
	// Re-initializing without a map drops the old file; anchors without one read as empty.
	initAnchor(t, dir, checklist, nil)
	if _, err := os.Stat(filepath.Join(dir, ".lha", "ownership.json")); !os.IsNotExist(err) {
		t.Fatal("stale ownership.json kept")
	}
	if got, _ := ReadOwnership(ctx, anchor); len(got.Snapshot()) != 0 {
		t.Fatal(got.Snapshot())
	}
}

func TestBrokerCommitsEachDecisionAndOnlyAnchorFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	anchor := initAnchor(t, dir, checklist, owners)
	_ = os.WriteFile(filepath.Join(dir, "work_in_progress.py"), []byte("uncommitted\n"), 0o644)
	broker := NewLeaseBroker(anchor)
	granted, err := broker.Request(ctx, LeaseRequest{Writer: "implementer-01", Path: "lib/x.py", Reason: "helper"}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	refused, err := broker.Request(ctx, LeaseRequest{Writer: "implementer-01", Path: "b.py", Reason: "need it"}, "c1")
	if err != nil || !granted.Granted || refused.Granted {
		t.Fatalf("%+v %+v %v", granted, refused, err)
	}
	persisted, _ := ReadOwnership(ctx, state.NewGitMissionAnchor(dir))
	if persisted.OwnerOf("lib/x.py") != "implementer-01" || persisted.OwnerOf("b.py") != "implementer-02" {
		t.Fatal(persisted.Snapshot())
	}
	leases := events(t, dir, LeaseEvent)
	if len(leases) != 2 || leases[0]["path"] != "lib/x.py" || leases[0]["granted"] != true || leases[1]["path"] != "b.py" ||
		leases[1]["granted"] != false || leases[1]["previous_owner"] != "implementer-02" || leases[0]["previous_owner"] != nil {
		t.Fatalf("%v", leases)
	}
	log := strings.Split(git(t, dir, "log", "-3", "--format=%s"), "\n")
	if log[0] != "lha: lease refused: b.py (implementer-01)" || log[1] != "lha: lease granted: lib/x.py (implementer-01)" {
		t.Fatalf("%q", log)
	}
	if _, err := os.Stat(filepath.Join(dir, "work_in_progress.py")); err != nil || strings.Contains(git(t, dir, "ls-files"), "work_in_progress.py") {
		t.Fatal("a lease commit must hold only .lha/ files")
	}
	if progress := git(t, dir, "show", "HEAD:.lha/progress.md"); !strings.Contains(progress, "- c1 lease granted: lib/x.py to implementer-01") {
		t.Fatal(progress)
	}
	// Once item 02 is done its files are free: a later request is granted.
	done, _ := anchor.ReadChecklist(ctx)
	if _, err := done.RecordSuccess("02", []string{"check"}); err != nil {
		t.Fatal(err)
	}
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c2", Checklist: done}); err != nil {
		t.Fatal(err)
	}
	later, err := broker.Request(ctx, LeaseRequest{Writer: "implementer-01", Path: "b.py", Reason: "now"}, "c3")
	if err != nil || !later.Granted || later.HasPrevious {
		t.Fatalf("%+v %v", later, err)
	}
	if got, _ := ReadOwnership(ctx, anchor); got.OwnerOf("b.py") != "implementer-01" {
		t.Fatal(got.Snapshot())
	}
}

func TestLeaseHandlerUpdatesTheLiveMap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	checklist, owners := twoItems()
	anchor := initAnchor(t, dir, checklist, owners)
	live := owners.Clone()
	log := &LeaseLog{}
	handle := NewLeaseHandler(NewLeaseBroker(anchor), "implementer-01", "c1", live, log)
	ok, message, err := handle(ctx, "docs/a.md", "document a")
	if err != nil || !ok || !strings.Contains(message, "granted") || live.OwnerOf("docs/a.md") != "implementer-01" {
		t.Fatalf("%v %q %v", ok, message, err)
	}
	ok, message, _ = handle(ctx, "b.py", "steal")
	if ok || !strings.Contains(message, "refused") || live.OwnerOf("b.py") != "implementer-02" {
		t.Fatalf("%v %q", ok, message)
	}
	if d := log.Decisions(); len(d) != 2 || !d[0].Granted || d[1].Granted {
		t.Fatalf("%+v", d)
	}
}

func TestWorkdirFlockIsExclusive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initAnchor(t, dir, contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{contracts.NewChecklistItem("01", "x")}}, nil)
	unlock, err := WorkdirFlock(ctx, dir, CycleLock)
	if err != nil {
		t.Fatal(err)
	}
	saved := LockWait
	LockWait = 600 * time.Millisecond
	defer func() { LockWait = saved }()
	var busy *WorkdirBusyError
	if _, err := WorkdirFlock(ctx, dir, CycleLock); !errors.As(err, &busy) {
		t.Fatalf("second holder: %v", err)
	}
	unlock()
	again, err := WorkdirFlock(ctx, dir, CycleLock)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestBlackboardSeparatesRounds(t *testing.T) {
	var b Blackboard
	b.Post("a", "main")
	b.Respond("r", "this round")
	if len(b.Read()) != 1 || len(b.ReadResponses()) != 1 {
		t.Fatal("responses must not be visible mid-round")
	}
	b.CommitRound()
	if got := b.Read(); len(got) != 2 || got[1].Author != "r" || len(b.ReadResponses()) != 0 {
		t.Fatalf("%+v", got)
	}
}
