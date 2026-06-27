package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func twoItems() contracts.Checklist {
	return contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{
		contracts.NewChecklistItem("01", "add model"),
		contracts.NewChecklistItem("02", "add tests", "01"),
	}}
}

func oneItem() contracts.Checklist {
	return contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{
		contracts.NewChecklistItem("01", "x"),
	}}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func eventKinds(t *testing.T, text string) []string {
	t.Helper()
	kinds := []string{}
	for _, ln := range strings.Split(text, "\n") {
		if ln == "" {
			continue
		}
		var e contracts.EventRecord
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func TestInitializeCreatesCommittedAnchor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sha := must(NewGitMissionAnchor(dir).Initialize(ctx, "T", "D", twoItems()))
	if sha == "" || !must(HasCommits(ctx, dir)) {
		t.Fatal("no commit")
	}
	for _, f := range []string{ChecklistFile, ProgressFile, MissionFile, DecisionsFile, EventsFile} {
		if !exists(filepath.Join(dir, AnchorDir, f)) {
			t.Fatalf("%s missing", f)
		}
	}
	log := must(LogOneline(ctx, dir, 10))
	if len(log) != 1 || !strings.Contains(log[0], "lha: initialize mission anchor") {
		t.Fatal(log)
	}
	wantProgress := "# Mission: T\n\nD\n\n## Progress\n\n- _initialized; no work yet._\n"
	if got := readText(t, filepath.Join(dir, AnchorDir, ProgressFile)); got != wantProgress {
		t.Fatalf("progress = %q", got)
	}
	wantMission := "{\n  \"title\": \"T\",\n  \"description\": \"D\",\n  \"acceptance\": \"\",\n  \"references\": [],\n  \"schema_version\": 1\n}"
	if got := readText(t, filepath.Join(dir, AnchorDir, MissionFile)); got != wantMission {
		t.Fatalf("mission.json = %q", got)
	}
}

func TestSituationalAwarenessPicksNextActionable(t *testing.T) {
	ctx := context.Background()
	anchor := NewGitMissionAnchor(t.TempDir())
	must(anchor.Initialize(ctx, "T", "D", twoItems()))
	snap := must(anchor.ReadSituationalAwareness(ctx))
	if snap.HeadSHA == "" || len(snap.OpenItems) != 2 || snap.IsComplete {
		t.Fatalf("%+v", snap)
	}
	if snap.ActiveItem == nil || snap.ActiveItem.ID != "01" {
		t.Fatalf("active = %+v", snap.ActiveItem)
	}
	if len(snap.LastDecisions) != 0 || snap.LastDecisions == nil {
		t.Fatal("decisions must be an empty list")
	}
}

func TestCheckpointAdvancesAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	base := must(anchor.Initialize(ctx, "T", "D", twoItems()))
	cl := twoItems()
	cl.Items[0].Status = contracts.StatusDone
	cl.Items[0].VerifiedBy = []string{"pytest"}
	newSHA := must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID:         "c1",
		ProgressSummary: "# Progress\n\nDone 01.\n",
		Checklist:       cl,
		Decisions:       []contracts.DecisionRecord{{Decision: "use dataclass", Rationale: "simple", CycleID: "c1"}},
		Events:          []contracts.EventRecord{{Kind: "item_done", CycleID: "c1", Payload: map[string]any{"id": "01"}}},
		CommitMessage:   "lha: checkpoint c1 (done 01)",
	}))
	if newSHA == "" || newSHA == base {
		t.Fatal("no new commit")
	}
	snap := must(NewGitMissionAnchor(dir).ReadSituationalAwareness(ctx))
	if snap.HeadSHA != newSHA || snap.ActiveItem == nil || snap.ActiveItem.ID != "02" {
		t.Fatalf("%+v", snap)
	}
	if len(snap.OpenItems) != 1 || snap.OpenItems[0].ID != "02" {
		t.Fatal(snap.OpenItems)
	}
	if n := len(snap.LastDecisions); n == 0 || snap.LastDecisions[n-1].Decision != "use dataclass" {
		t.Fatal(snap.LastDecisions)
	}
	if !strings.HasPrefix(snap.RecentCommits[0], newSHA[:7]) ||
		!strings.HasSuffix(snap.RecentCommits[0], "lha: checkpoint c1 (done 01)") {
		t.Fatal(snap.RecentCommits)
	}
}

func TestDefaultCommitMessageAndLastFiveDecisions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "T", "D", oneItem()))
	for i := range 7 {
		must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
			CycleID: fmt.Sprintf("c%d", i), Checklist: oneItem(),
			Decisions: []contracts.DecisionRecord{{Decision: fmt.Sprintf("d%d", i), Rationale: "r"}},
		}))
	}
	log := must(LogOneline(ctx, dir, 1))
	if !strings.HasSuffix(log[0], " lha: checkpoint c6") {
		t.Fatal(log)
	}
	snap := must(anchor.ReadSituationalAwareness(ctx))
	got := []string{}
	for _, d := range snap.LastDecisions {
		got = append(got, d.Decision)
	}
	if !slices.Equal(got, []string{"d2", "d3", "d4", "d5", "d6"}) {
		t.Fatal(got)
	}
	// An empty progress summary leaves progress.md untouched.
	if p := snap.ProgressSummary; strings.Count(p, "- ") != 1 {
		t.Fatalf("progress = %q", p)
	}
}

func TestCompletionDetectedWhenAllDone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "T", "D", twoItems()))
	cl := twoItems()
	for i := range cl.Items {
		cl.Items[i].Status = contracts.StatusDone
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c2", ProgressSummary: "done", Checklist: cl}))
	snap := must(NewGitMissionAnchor(dir).ReadSituationalAwareness(ctx))
	if !snap.IsComplete || snap.ActiveItem != nil || len(snap.OpenItems) != 0 {
		t.Fatalf("%+v", snap)
	}
}

func TestAppendEventWritesLines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "T", "D", twoItems()))
	for _, k := range []string{"started", "acted"} {
		if err := anchor.AppendEvent(ctx, contracts.EventRecord{Kind: k, CycleID: "c1"}); err != nil {
			t.Fatal(err)
		}
	}
	text := readText(t, filepath.Join(dir, AnchorDir, EventsFile))
	if text != "{\"kind\":\"started\",\"cycle_id\":\"c1\",\"payload\":{},\"payload_ref\":null}\n"+
		"{\"kind\":\"acted\",\"cycle_id\":\"c1\",\"payload\":{},\"payload_ref\":null}\n" {
		t.Fatalf("events = %q", text)
	}
}

func TestMissionSpecIsPersistedAndRecited(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.InitializeWithAcceptance(ctx, "Ship X", "Make X work.", "tests pass", twoItems()))
	mission := must(NewGitMissionAnchor(dir).ReadMission(ctx))
	if mission == nil || mission.Title != "Ship X" || mission.Acceptance != "tests pass" {
		t.Fatalf("%+v", mission)
	}
	snap := must(anchor.ReadSituationalAwareness(ctx))
	text := snap.AnchorText()
	if !strings.Contains(text, "Ship X") || !strings.Contains(text, "Make X work.") {
		t.Fatal(text)
	}
	if snap.ItemsDone != 0 || snap.ItemsTotal != 2 {
		t.Fatal(snap.ItemsDone, snap.ItemsTotal)
	}
}

func TestReadsWithoutAnchor(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	anchor := NewGitMissionAnchor(dir)
	if m := must(anchor.ReadMission(ctx)); m != nil {
		t.Fatal(m)
	}
	cl := must(anchor.ReadChecklist(ctx))
	if cl.Items == nil || len(cl.Items) != 0 || cl.SchemaVersion != 1 {
		t.Fatalf("%+v", cl)
	}
	// Untracked working-tree anchor files are the fallback (with universal newlines).
	writeFile(t, filepath.Join(dir, AnchorDir, ProgressFile), "a\r\nb")
	snap := must(anchor.ReadSituationalAwareness(ctx))
	if snap.ProgressSummary != "a\nb" || !snap.IsDeadlocked {
		t.Fatalf("%+v", snap)
	}
}

func TestProgressIsAppendedAndBounded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir, WithMaxProgressChars(300))
	must(anchor.Initialize(ctx, "Ship X", "Make X work.", twoItems()))
	for n := range 40 {
		must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
			CycleID: fmt.Sprintf("c%d", n), ProgressSummary: fmt.Sprintf("- entry %02d", n), Checklist: twoItems(),
		}))
	}
	progress := readText(t, filepath.Join(dir, AnchorDir, ProgressFile))
	if len(progress) > 300 || !strings.HasPrefix(progress, "# Mission: Ship X") {
		t.Fatalf("progress = %q", progress)
	}
	if !strings.Contains(progress, "entry 39") || !strings.Contains(progress, "entry 38") ||
		strings.Contains(progress, "entry 00") || !strings.Contains(progress, "older entries trimmed") {
		t.Fatalf("progress = %q", progress)
	}
	if strings.Count(progress, "older entries trimmed") != 1 {
		t.Fatalf("note duplicated: %q", progress)
	}
}

func TestBoundProgressCountsCharacters(t *testing.T) {
	header := "# Mission: ü\n\n" + progressMarker
	text := header + "- " + strings.Repeat("é", 30) + "\n- 222\n"
	if got := boundProgress(text, runeLen(text)); got != text {
		t.Fatal(got)
	}
	got := boundProgress(text, runeLen(header)+runeLen("- _(older entries trimmed)_\n")+6)
	if want := header + "- _(older entries trimmed)_\n- 222\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// No marker: everything is body.
	if got := boundProgress("aaaa\nbb\n", 3); got != "- _(older entries trimmed)_\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCommitDiscardsAgentEditsToAnchor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "T", "D", twoItems()))
	forged := twoItems()
	for i := range forged.Items {
		forged.Items[i].Status = contracts.StatusDone
	}
	b, _ := json.Marshal(forged)
	writeFile(t, filepath.Join(dir, AnchorDir, ChecklistFile), string(b))
	writeFile(t, filepath.Join(dir, AnchorDir, ProgressFile), "pwned")
	writeFile(t, filepath.Join(dir, AnchorDir, "extra.txt"), "junk")

	cl := must(anchor.ReadChecklist(ctx))
	if cl.IsComplete() {
		t.Fatal("read came from the tampered working tree")
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c1", ProgressSummary: "- c1 attempt", Checklist: twoItems()}))
	progress := readText(t, filepath.Join(dir, AnchorDir, ProgressFile))
	if strings.Contains(progress, "pwned") || !strings.Contains(progress, "- c1 attempt") {
		t.Fatal(progress)
	}
	if exists(filepath.Join(dir, AnchorDir, "extra.txt")) {
		t.Fatal("extra.txt survived")
	}
	cl = must(NewGitMissionAnchor(dir).ReadChecklist(ctx))
	if cl.IsComplete() {
		t.Fatal("forged checklist committed")
	}
}

func TestPendingEventsSurviveAnchorRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "T", "D", twoItems()))
	if err := anchor.AppendEvent(ctx, contracts.EventRecord{Kind: "started", CycleID: "c1"}); err != nil {
		t.Fatal(err)
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID: "c1", ProgressSummary: "- c1", Checklist: twoItems(),
		Events: []contracts.EventRecord{{Kind: "cycle", CycleID: "c1"}},
	}))
	got := eventKinds(t, readText(t, filepath.Join(dir, AnchorDir, EventsFile)))
	if !slices.Equal(got, []string{"started", "cycle"}) {
		t.Fatal(got)
	}
	// Pending events are cleared by the commit: the next checkpoint does not re-add them.
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c2", Checklist: twoItems()}))
	got = eventKinds(t, must(ShowAtHead(ctx, dir, ".lha/events.ndjson")))
	if !slices.Equal(got, []string{"started", "cycle"}) {
		t.Fatal(got)
	}
}

func TestInitializeRejectsUnreachablePlan(t *testing.T) {
	bad := contracts.Checklist{Items: []contracts.ChecklistItem{contracts.NewChecklistItem("01", "x", "1")}}
	dir := filepath.Join(t.TempDir(), "w")
	_, err := NewGitMissionAnchor(dir).Initialize(context.Background(), "T", "D", bad)
	if err == nil || !strings.HasPrefix(err.Error(), "invalid checklist: ") || !strings.Contains(err.Error(), "unknown item") {
		t.Fatalf("err = %v", err)
	}
	if exists(dir) {
		t.Fatal("nothing may be written for a rejected plan")
	}
}

func TestSnapshotReportsDeadlockNotCompletion(t *testing.T) {
	ctx := context.Background()
	anchor := NewGitMissionAnchor(t.TempDir())
	must(anchor.Initialize(ctx, "T", "D", twoItems()))
	blocked := twoItems()
	blocked.Items[0].Status = contracts.StatusBlocked
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c1", ProgressSummary: "- blocked", Checklist: blocked}))
	snap := must(anchor.ReadSituationalAwareness(ctx))
	if snap.ActiveItem != nil || snap.IsComplete || !snap.IsDeadlocked || !strings.Contains(snap.DeadlockReason, "blocked: 01") {
		t.Fatalf("%+v", snap)
	}
}

func TestAnchorInNestedDirGetsItsOwnRepo(t *testing.T) {
	ctx := context.Background()
	parent := parentRepo(t, filepath.Join(t.TempDir(), "parent"), false)
	writeFile(t, filepath.Join(parent, "unrelated.txt"), "user work\n")
	parentHead := git(t, parent, "rev-parse", "HEAD")
	workdir := filepath.Join(parent, ".lha", "workspaces", "durable")
	must(NewGitMissionAnchor(workdir).Initialize(ctx, "t", "d", oneItem()))

	if git(t, parent, "rev-parse", "HEAD") != parentHead {
		t.Fatal("parent repo got a commit")
	}
	if !strings.Contains(git(t, parent, "status", "--porcelain"), "?? unrelated.txt") {
		t.Fatal("parent status changed")
	}
	cmd := exec.Command("git", "config", "--local", "user.name")
	cmd.Dir = parent
	if out, _ := cmd.Output(); strings.TrimSpace(string(out)) == DefaultAuthorName {
		t.Fatal("parent identity changed")
	}
	if !exists(filepath.Join(workdir, ".git")) || !must(ExistsAtHead(ctx, workdir, ".lha/checklist.json")) {
		t.Fatal("workdir is not its own repo")
	}
	if n := len(must(LogOneline(ctx, workdir, 10))); n != 1 {
		t.Fatal(n)
	}
}

func TestAnchorIsCommittedEvenWhenGitignored(t *testing.T) {
	ctx := context.Background()
	repo := parentRepo(t, filepath.Join(t.TempDir(), "repo"), true)
	anchor := NewGitMissionAnchor(repo)
	must(anchor.Initialize(ctx, "t", "d", oneItem()))
	if !must(ExistsAtHead(ctx, repo, ".lha/checklist.json")) {
		t.Fatal("anchor not committed")
	}
	forged := oneItem()
	forged.Items[0].Status = contracts.StatusDone
	b, _ := json.Marshal(forged)
	writeFile(t, filepath.Join(repo, AnchorDir, ChecklistFile), string(b))
	if must(anchor.ReadSituationalAwareness(ctx)).IsComplete {
		t.Fatal("tampered read")
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c1", ProgressSummary: "- c1", Checklist: oneItem()}))
	if final := must(NewGitMissionAnchor(repo).ReadChecklist(ctx)); final.IsComplete() {
		t.Fatal("forged checklist committed")
	}
}

func TestPendingEventsNotDuplicatedWhenAnchorUntracked(t *testing.T) {
	ctx := context.Background()
	repo := parentRepo(t, filepath.Join(t.TempDir(), "repo"), true)
	anchor := NewGitMissionAnchor(repo)
	must(anchor.Initialize(ctx, "t", "d", oneItem()))
	if err := anchor.AppendEvent(ctx, contracts.EventRecord{Kind: "started", CycleID: "c1"}); err != nil {
		t.Fatal(err)
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID: "c1", ProgressSummary: "- c1", Checklist: oneItem(),
		Events: []contracts.EventRecord{{Kind: "cycle", CycleID: "c1"}},
	}))
	got := eventKinds(t, must(ShowAtHead(ctx, repo, ".lha/events.ndjson")))
	if !slices.Equal(got, []string{"started", "cycle"}) {
		t.Fatal(got)
	}
}

func TestRebuiltLogsDoNotDuplicateEvenWithoutRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "t", "d", oneItem()))
	anchor.skipRestore = true
	if err := anchor.AppendEvent(ctx, contracts.EventRecord{Kind: "started", CycleID: "c1"}); err != nil {
		t.Fatal(err)
	}
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c1", ProgressSummary: "- c1", Checklist: oneItem()}))
	got := eventKinds(t, readText(t, filepath.Join(dir, AnchorDir, EventsFile)))
	if !slices.Equal(got, []string{"started"}) {
		t.Fatal(got)
	}
}

const separators = "a\u2028b\u2029c\u0085d"

func TestAnchorReadsDecisionsWithUnicodeLineSeparators(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "t", "d", oneItem()))
	must(anchor.CommitCheckpoint(ctx, contracts.Checkpoint{
		CycleID: "c1", ProgressSummary: "- c1", Checklist: oneItem(),
		Decisions: []contracts.DecisionRecord{{Decision: separators + " <&>", Rationale: "r"}},
	}))
	raw := readText(t, filepath.Join(dir, AnchorDir, DecisionsFile))
	// pydantic writes these raw (no \u escapes): the file bytes match the Python implementation.
	want := "{\"decision\":\"" + separators + " <&>\",\"rationale\":\"r\",\"alternatives_rejected\":\"\",\"affected\":[],\"cycle_id\":\"\"}\n"
	if raw != want {
		t.Fatalf("decisions.ndjson = %q, want %q", raw, want)
	}
	snap := must(NewGitMissionAnchor(dir).ReadSituationalAwareness(ctx))
	if len(snap.LastDecisions) != 1 || snap.LastDecisions[0].Decision != separators+" <&>" {
		t.Fatal(snap.LastDecisions)
	}
}

func TestRestoreFromHead(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	anchor := NewGitMissionAnchor(dir)
	writeFile(t, filepath.Join(dir, "a.txt"), "tampered")
	writeFile(t, filepath.Join(dir, "new.txt"), "new")
	got := must(anchor.RestoreFromHead(ctx, []string{"a.txt", "new.txt"}))
	if !slices.Equal(got, []string{"a.txt"}) || readText(t, filepath.Join(dir, "a.txt")) != "a" {
		t.Fatal(got)
	}
	if anchor.Workdir() != dir {
		t.Fatal(anchor.Workdir())
	}
}

func TestAnchorErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := NewGitMissionAnchor(dir)
	must(anchor.Initialize(ctx, "T", "D", oneItem()))

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := anchor.ReadSituationalAwareness(cancelled); err == nil {
		t.Fatal("cancelled read succeeded")
	}
	if _, err := anchor.CommitCheckpoint(cancelled, contracts.Checkpoint{CycleID: "c", Checklist: oneItem()}); err == nil {
		t.Fatal("cancelled commit succeeded")
	}
	if _, err := anchor.RestoreFromHead(cancelled, []string{"x"}); err == nil {
		t.Fatal("cancelled restore succeeded")
	}
	if _, err := NewGitMissionAnchor(t.TempDir()).Initialize(cancelled, "T", "D", oneItem()); err == nil {
		t.Fatal("cancelled init succeeded")
	}

	// A corrupt committed checklist / mission / decision line is an error, not an empty anchor.
	for _, name := range []string{ChecklistFile, MissionFile, DecisionsFile} {
		repo := t.TempDir()
		a := NewGitMissionAnchor(repo)
		must(a.Initialize(ctx, "T", "D", oneItem()))
		writeFile(t, filepath.Join(repo, AnchorDir, name), "{not json")
		must(RunGit(ctx, repo, "commit", "-qam", "corrupt"))
		if _, err := a.ReadSituationalAwareness(ctx); err == nil {
			t.Fatalf("corrupt %s accepted", name)
		}
	}

	// The anchor path being a file makes every write fail.
	blocked := t.TempDir()
	writeFile(t, filepath.Join(blocked, AnchorDir), "file, not a dir")
	b := NewGitMissionAnchor(blocked)
	if err := b.AppendEvent(ctx, contracts.EventRecord{Kind: "k"}); err == nil {
		t.Fatal("append into a file succeeded")
	}
	if _, err := b.Initialize(ctx, "T", "D", oneItem()); err == nil {
		t.Fatal("init into a file succeeded")
	}
}
