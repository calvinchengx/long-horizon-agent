package state

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Anchor layout inside the worked repo:
//
//	.lha/mission.json       — the immutable MissionSpec, written at init
//	.lha/checklist.json     — the machine-readable Checklist
//	.lha/progress.md        — human-readable progress narrative (appended, size-bounded)
//	.lha/decisions.ndjson   — append-only, SHA-256 hash-chained DecisionRecord log (never
//	                          compacted; format in decision_chain.go)
//	.lha/events.ndjson      — append-only EventRecord (episodic) log
//	.lha/ownership.json     — the file-ownership map, when the mission declared one (the
//	                          orchestrator's; coordination.ReadOwnership / StageOwnership)
//
// The committed decisions.ndjson is verified on every snapshot read and before every checkpoint
// appends to it: a chain that does not verify is a *DecisionChainError and nothing is committed
// on top of it. Bare (pre-chain) DecisionRecord lines still load as a legacy prefix.
const (
	AnchorDir     = ".lha"
	MissionFile   = "mission.json"
	ChecklistFile = "checklist.json"
	ProgressFile  = "progress.md"
	DecisionsFile = "decisions.ndjson"
	EventsFile    = "events.ndjson"
	OwnershipFile = "ownership.json"
)

// AnchorFiles are the harness-owned anchor files. They are force-added on every commit (when
// present) so a target repo whose .gitignore excludes .lha/ still gets them committed.
var AnchorFiles = []string{MissionFile, ChecklistFile, ProgressFile, DecisionsFile, EventsFile, OwnershipFile}

// MaxProgressChars bounds progress.md (oldest entries are trimmed first), in characters.
const MaxProgressChars = 16_000

const progressMarker = "## Progress\n\n"

// GitMissionAnchor is a contracts.DurableState backed by a git repo at its workdir.
//
// Anchor files are committed alongside the agent's code changes, so every checkpoint is one
// atomic commit capturing both the work and the progress. Reads come from the committed HEAD
// (not the agent-editable working tree), and CommitCheckpoint restores .lha/ to HEAD before
// rewriting it from the harness's own in-memory state: agent edits to anchor files are
// discarded, never committed. The on-disk format is shared with the Python implementation.
type GitMissionAnchor struct {
	workdir     string
	anchor      string
	maxProgress int

	mu sync.Mutex
	// Events appended (uncommitted) via AppendEvent; re-applied after the .lha restore in
	// CommitCheckpoint so they are not lost.
	pendingEvents []contracts.EventRecord
	// Decisions queued mid-cycle by the record_decision tool (RecordDecision); chained onto
	// decisions.ndjson by the next CommitCheckpoint.
	pendingDecisions []contracts.DecisionRecord
	// An ownership map (its .lha/ownership.json bytes) staged for the next checkpoint
	// (StageOwnershipJSON); nil when none is staged.
	pendingOwnership []byte
	// skipRestore disables the .lha restore (tests prove the log rebuild is idempotent anyway).
	skipRestore bool
}

var _ contracts.DurableState = (*GitMissionAnchor)(nil)

// AnchorOption configures a GitMissionAnchor.
type AnchorOption func(*GitMissionAnchor)

// WithMaxProgressChars overrides MaxProgressChars.
func WithMaxProgressChars(n int) AnchorOption {
	return func(a *GitMissionAnchor) { a.maxProgress = n }
}

// NewGitMissionAnchor returns an anchor for the repo at workdir.
func NewGitMissionAnchor(workdir string, opts ...AnchorOption) *GitMissionAnchor {
	a := &GitMissionAnchor{
		workdir:     workdir,
		anchor:      filepath.Join(workdir, AnchorDir),
		maxProgress: MaxProgressChars,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Workdir is the repository directory.
func (a *GitMissionAnchor) Workdir() string { return a.workdir }

func (a *GitMissionAnchor) path(name string) string { return filepath.Join(a.anchor, name) }

// Initialize writes the immutable mission spec + initial anchor and commits (acceptance "").
func (a *GitMissionAnchor) Initialize(ctx context.Context, title, description string, items contracts.Checklist) (string, error) {
	return a.InitializeWithAcceptance(ctx, title, description, "", items)
}

// InitializeWithAcceptance writes the mission spec (with its definition of done) + the initial
// anchor and commits; a checklist with dependency errors is rejected before anything is written.
func (a *GitMissionAnchor) InitializeWithAcceptance(ctx context.Context, title, description, acceptance string, items contracts.Checklist) (string, error) {
	return a.InitializeSpec(ctx, contracts.MissionSpec{Title: title, Description: description, Acceptance: acceptance}, items)
}

// InitializeSpec writes spec (title, description, acceptance and the vendored references) + the
// initial anchor and commits (python: initialize(..., acceptance=, references=)). The spec's
// schema version is always 1; a checklist with dependency errors is rejected before anything is
// written.
func (a *GitMissionAnchor) InitializeSpec(ctx context.Context, spec contracts.MissionSpec, items contracts.Checklist) (string, error) {
	return a.InitializeSpecWithOwnership(ctx, spec, items, nil)
}

// InitializeSpecWithOwnership is InitializeSpec plus the file-ownership map (python:
// initialize(..., ownership=)): ownershipJSON is written as .lha/ownership.json; nil means the
// mission has no map, and a stale ownership.json from an earlier initialization is removed.
func (a *GitMissionAnchor) InitializeSpecWithOwnership(ctx context.Context, spec contracts.MissionSpec, items contracts.Checklist, ownershipJSON []byte) (string, error) {
	if errs := items.DependencyErrors(); len(errs) > 0 {
		return "", errors.New("invalid checklist: " + strings.Join(errs, "; "))
	}
	spec.References = append([]string{}, spec.References...)
	spec.SchemaVersion = 1
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := InitRepo(ctx, a.workdir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(a.anchor, 0o777); err != nil {
		return "", err
	}
	missionJSON, err := pydanticJSON(spec, true)
	if err != nil {
		return "", err
	}
	if err := a.writeFile(MissionFile, string(missionJSON)); err != nil {
		return "", err
	}
	if err := a.writeChecklist(items); err != nil {
		return "", err
	}
	if ownershipJSON != nil {
		if err := a.writeFile(OwnershipFile, string(ownershipJSON)); err != nil {
			return "", err
		}
	} else if err := os.Remove(a.path(OwnershipFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err // re-initialized without a map: never inherit a stale one
	}
	progress := "# Mission: " + spec.Title + "\n\n" + spec.Description + "\n\n" +
		progressMarker + "- _initialized; no work yet._\n"
	if err := a.writeFile(ProgressFile, progress); err != nil {
		return "", err
	}
	// Create the append-only logs (empty).
	for _, name := range []string{DecisionsFile, EventsFile} {
		if err := a.writeFile(name, ""); err != nil {
			return "", err
		}
	}
	a.pendingEvents = nil
	a.pendingDecisions = nil
	a.pendingOwnership = nil
	return a.commitAll(ctx, "lha: initialize mission anchor")
}

// ReadSituationalAwareness reconstructs the full picture from the committed anchor + git log.
func (a *GitMissionAnchor) ReadSituationalAwareness(ctx context.Context) (contracts.SituationSnapshot, error) {
	var snap contracts.SituationSnapshot
	checklist, err := a.ReadChecklist(ctx)
	if err != nil {
		return snap, err
	}
	if snap.HeadSHA, err = HeadSHA(ctx, a.workdir); err != nil {
		return snap, err
	}
	if snap.RecentCommits, err = LogOneline(ctx, a.workdir, 10); err != nil {
		return snap, err
	}
	if snap.Members, err = MemberPaths(ctx, a.workdir); err != nil {
		return snap, err
	}
	if snap.Mission, err = a.ReadMission(ctx); err != nil {
		return snap, err
	}
	progress, _, err := a.readAnchorFile(ctx, ProgressFile)
	if err != nil {
		return snap, err
	}
	snap.ProgressSummary = progress
	snap.OpenItems = []contracts.ChecklistItem{}
	for _, it := range checklist.Items {
		if it.IsOpen() {
			snap.OpenItems = append(snap.OpenItems, it)
		}
	}
	chain, err := a.loadDecisions(ctx)
	if err != nil {
		return snap, err
	}
	snap.LastDecisions = chain.Records
	if n := len(snap.LastDecisions); n > RecentDecisions {
		snap.LastDecisions = snap.LastDecisions[n-RecentDecisions:]
	}
	if next := checklist.NextActionable(); next != nil {
		item := *next
		snap.ActiveItem = &item
	}
	snap.IsComplete = checklist.IsComplete()
	snap.IsDeadlocked = checklist.IsDeadlocked()
	snap.DeadlockReason = checklist.DeadlockReason()
	snap.ItemsDone = checklist.ItemsDone()
	snap.ItemsTotal = len(checklist.Items)
	return snap, nil
}

// ReadChecklist returns the full committed checklist (all items, not just the open ones).
func (a *GitMissionAnchor) ReadChecklist(ctx context.Context) (contracts.Checklist, error) {
	raw, ok, err := a.readAnchorFile(ctx, ChecklistFile)
	if err != nil {
		return contracts.Checklist{}, err
	}
	if !ok {
		return contracts.Checklist{Items: []contracts.ChecklistItem{}, SchemaVersion: 1}, nil
	}
	var cl contracts.Checklist
	if err := json.Unmarshal([]byte(raw), &cl); err != nil {
		return contracts.Checklist{}, err
	}
	return cl, nil
}

// ReadMission returns the immutable mission spec (nil for anchors created before it existed).
func (a *GitMissionAnchor) ReadMission(ctx context.Context) (*contracts.MissionSpec, error) {
	raw, ok, err := a.readAnchorFile(ctx, MissionFile)
	if err != nil || !ok {
		return nil, err
	}
	var spec contracts.MissionSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// AppendEvent appends an event to the working-tree events log; it is committed (exactly once)
// by the next CommitCheckpoint.
func (a *GitMissionAnchor) AppendEvent(_ context.Context, event contracts.EventRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(a.anchor, 0o777); err != nil {
		return err
	}
	line, err := pydanticJSON(event, false)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(a.path(EventsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if _, err := fh.Write(append(line, '\n')); err != nil {
		_ = fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	a.pendingEvents = append(a.pendingEvents, event)
	return nil
}

// RecordDecision queues record for the next checkpoint (in memory) and returns how many are
// queued. The record_decision tool calls this mid-cycle (it is the anchor's
// execution/tools.DecisionSink); the next CommitCheckpoint chains the queued records onto
// decisions.ndjson — stamped with the checkpoint's cycle id if they have none — so they are
// committed together with the cycle's work.
func (a *GitMissionAnchor) RecordDecision(record contracts.DecisionRecord) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingDecisions = append(a.pendingDecisions, record)
	return len(a.pendingDecisions)
}

// HasPendingRecords reports whether events, decisions or an ownership map await the next commit.
func (a *GitMissionAnchor) HasPendingRecords() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pendingEvents) > 0 || len(a.pendingDecisions) > 0 || a.pendingOwnership != nil
}

// PendingDecisions returns a copy of the decisions queued since the last checkpoint.
func (a *GitMissionAnchor) PendingDecisions() []contracts.DecisionRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]contracts.DecisionRecord{}, a.pendingDecisions...)
}

// CommitCheckpoint restores .lha/ to HEAD, rewrites it from the checkpoint and commits
// everything (work + anchor) atomically; it returns the new HEAD sha.
func (a *GitMissionAnchor) CommitCheckpoint(ctx context.Context, cp contracts.Checkpoint) (string, error) {
	return a.commitSync(ctx, cp, false)
}

// CommitAnchorUpdate is CommitCheckpoint, but the commit holds ONLY .lha/ files: for harness
// bookkeeping between cycles (e.g. a lease granted mid-wave), whatever else is in the work tree
// or the index is neither committed nor discarded.
func (a *GitMissionAnchor) CommitAnchorUpdate(ctx context.Context, cp contracts.Checkpoint) (string, error) {
	return a.commitSync(ctx, cp, true)
}

func (a *GitMissionAnchor) commitSync(ctx context.Context, cp contracts.Checkpoint, anchorOnly bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Verify the committed decision chain BEFORE touching anything: an altered history is never
	// extended.
	chain, err := a.loadDecisions(ctx)
	if err != nil {
		return "", err
	}
	// Harness truth: discard whatever the agent did to .lha/ during the cycle.
	if !a.skipRestore {
		if err := a.restoreAnchorDir(ctx); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(a.anchor, 0o777); err != nil {
		return "", err
	}
	if err := a.writeChecklist(cp.Checklist); err != nil {
		return "", err
	}
	if pyStrip(cp.ProgressSummary) != "" {
		if err := a.appendProgress(ctx, cp.ProgressSummary); err != nil {
			return "", err
		}
	}
	if a.pendingOwnership != nil {
		if err := a.writeFile(OwnershipFile, string(a.pendingOwnership)); err != nil {
			return "", err
		}
	}
	// The append-only logs are REBUILT from their committed content + the new records, so the
	// write is idempotent: pending events already appended to the working tree are never
	// duplicated. Decisions are chained onto the verified committed chain; a record without a
	// cycle id gets the checkpoint's.
	prev := chain.LastHash
	queued := append(append([]contracts.DecisionRecord{}, a.pendingDecisions...), cp.Decisions...)
	lines := make([]string, 0, len(queued))
	for _, d := range queued {
		if d.CycleID == "" {
			d.CycleID = cp.CycleID
		}
		line, digest, err := EncodeDecisionLink(prev, d)
		if err != nil {
			return "", err
		}
		lines = append(lines, line)
		prev = digest
	}
	if err := a.rebuildLines(ctx, DecisionsFile, lines); err != nil {
		return "", err
	}
	events := make([]any, 0, len(a.pendingEvents)+len(cp.Events))
	for _, e := range a.pendingEvents {
		events = append(events, e)
	}
	for _, e := range cp.Events {
		events = append(events, e)
	}
	if err := a.rebuildLog(ctx, EventsFile, events); err != nil {
		return "", err
	}
	message := cp.CommitMessage
	if message == "" {
		message = "lha: checkpoint " + cp.CycleID
	}
	var sha string
	if anchorOnly {
		sha, err = a.commitAnchor(ctx, message)
	} else {
		sha, err = a.commitAll(ctx, message)
	}
	if err != nil {
		return "", err
	}
	a.pendingEvents = nil
	a.pendingDecisions = nil
	a.pendingOwnership = nil
	return sha, nil
}

// ReadEvents returns the committed episodic events, oldest first (unreadable lines are skipped).
func (a *GitMissionAnchor) ReadEvents(ctx context.Context) ([]contracts.EventRecord, error) {
	text, err := a.committedText(ctx, EventsFile)
	if err != nil {
		return nil, err
	}
	out := []contracts.EventRecord{}
	for _, line := range strings.Split(text, "\n") {
		if pyStrip(line) == "" {
			continue
		}
		var event contracts.EventRecord
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Kind == "" {
			continue
		}
		if event.Payload == nil {
			event.Payload = contracts.Payload()
		}
		out = append(out, event)
	}
	return out, nil
}

// ReadOwnershipJSON returns the committed .lha/ownership.json (falling back to the working tree
// before the first commit); ok is false when the mission never declared a map.
func (a *GitMissionAnchor) ReadOwnershipJSON(ctx context.Context) (text string, ok bool, err error) {
	return a.readAnchorFile(ctx, OwnershipFile)
}

// StageOwnershipJSON writes data as .lha/ownership.json with the next checkpoint (or anchor
// update). The staging is not sticky: it is cleared by that commit.
func (a *GitMissionAnchor) StageOwnershipJSON(data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingOwnership = append([]byte{}, data...)
}

// RestoreFromHead restores tracked relpaths to their HEAD content; it returns the ones restored.
func (a *GitMissionAnchor) RestoreFromHead(ctx context.Context, relpaths []string) ([]string, error) {
	restored := []string{}
	for _, rel := range relpaths {
		tracked, err := ExistsAtHead(ctx, a.workdir, rel)
		if err != nil {
			return restored, err
		}
		if tracked {
			if _, err := RunGit(ctx, a.workdir, "checkout", "HEAD", "--", rel); err != nil {
				return restored, err
			}
			restored = append(restored, rel)
		}
	}
	return restored, nil
}

// --- helpers -----------------------------------------------------------------------------

func (a *GitMissionAnchor) commitAll(ctx context.Context, message string) (string, error) {
	force := make([]string, 0, len(AnchorFiles))
	for _, name := range AnchorFiles {
		force = append(force, AnchorDir+"/"+name)
	}
	return CommitAll(ctx, a.workdir, message, force...)
}

func (a *GitMissionAnchor) commitAnchor(ctx context.Context, message string) (string, error) {
	paths := make([]string, 0, len(AnchorFiles))
	for _, name := range AnchorFiles {
		paths = append(paths, AnchorDir+"/"+name)
	}
	return CommitPaths(ctx, a.workdir, message, paths...)
}

func (a *GitMissionAnchor) writeFile(name, content string) error {
	return os.WriteFile(a.path(name), []byte(content), 0o666)
}

func (a *GitMissionAnchor) writeChecklist(cl contracts.Checklist) error {
	data, err := pydanticJSON(cl, true)
	if err != nil {
		return err
	}
	return a.writeFile(ChecklistFile, string(data))
}

// committedText is the committed (HEAD) content of anchor file name; "" if not committed.
func (a *GitMissionAnchor) committedText(ctx context.Context, name string) (string, error) {
	rel := AnchorDir + "/" + name
	tracked, err := ExistsAtHead(ctx, a.workdir, rel)
	if err != nil || !tracked {
		return "", err
	}
	return ShowAtHead(ctx, a.workdir, rel)
}

// rebuildLog rewrites an append-only log as committed content + records (idempotent).
func (a *GitMissionAnchor) rebuildLog(ctx context.Context, name string, records []any) error {
	lines := make([]string, 0, len(records))
	for _, rec := range records {
		line, err := pydanticJSON(rec, false)
		if err != nil {
			return err
		}
		lines = append(lines, string(line))
	}
	return a.rebuildLines(ctx, name, lines)
}

// rebuildLines rewrites an append-only log as committed content + lines (idempotent).
func (a *GitMissionAnchor) rebuildLines(ctx context.Context, name string, lines []string) error {
	base, err := a.committedText(ctx, name)
	if err != nil {
		return err
	}
	if base != "" && !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	var b strings.Builder
	b.WriteString(base)
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return a.writeFile(name, b.String())
}

// restoreAnchorDir resets .lha/ to HEAD (tracked files restored, untracked additions removed).
func (a *GitMissionAnchor) restoreAnchorDir(ctx context.Context) error {
	tracked, err := ExistsAtHead(ctx, a.workdir, AnchorDir)
	if err != nil || !tracked {
		return err
	}
	if _, err := RunGit(ctx, a.workdir, "checkout", "HEAD", "--", AnchorDir); err != nil {
		return err
	}
	_, err = RunGit(ctx, a.workdir, "clean", "-fdq", "--", AnchorDir)
	return err
}

// readAnchorFile reads an anchor file from HEAD (committed truth), falling back to the working
// tree; ok is false when neither has it.
func (a *GitMissionAnchor) readAnchorFile(ctx context.Context, name string) (text string, ok bool, err error) {
	rel := AnchorDir + "/" + name
	tracked, err := ExistsAtHead(ctx, a.workdir, rel)
	if err != nil {
		return "", false, err
	}
	if tracked {
		text, err := ShowAtHead(ctx, a.workdir, rel)
		return text, err == nil, err
	}
	data, err := os.ReadFile(a.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return universalNewlines(string(data)), true, nil
}

func (a *GitMissionAnchor) appendProgress(ctx context.Context, entry string) error {
	// Committed truth first (never the agent-editable working tree when HEAD has it).
	current, _, err := a.readAnchorFile(ctx, ProgressFile)
	if err != nil {
		return err
	}
	if current == "" {
		current = progressMarker
	}
	if !strings.HasSuffix(current, "\n") {
		current += "\n"
	}
	current += pyStrip(entry) + "\n"
	return a.writeFile(ProgressFile, boundProgress(current, a.maxProgress))
}

// RecentDecisions is how many of the newest decisions the situational snapshot carries.
const RecentDecisions = 5

// ReadDecisions returns every committed decision, oldest first (the chain is verified first).
func (a *GitMissionAnchor) ReadDecisions(ctx context.Context) ([]contracts.DecisionRecord, error) {
	chain, err := a.loadDecisions(ctx)
	return chain.Records, err
}

// VerifyDecisions verifies the committed decision chain (a failed check is not an error).
func (a *GitMissionAnchor) VerifyDecisions(ctx context.Context) (ChainVerification, error) {
	data, err := a.decisionsBytes(ctx)
	if err != nil {
		return ChainVerification{}, err
	}
	check := VerifyDecisionChain(data)
	if check.OK && check.TornTail {
		// The harness writes the whole file before committing it, so an incomplete final line in
		// the anchor is never a crash artefact: treat it as an altered log.
		check.OK = false
		check.Problem = "the final line is incomplete (no trailing newline)"
	}
	return check, nil
}

// decisionsBytes is the decision log's exact content: HEAD first (not stripped: the trailing
// newline is significant), else the never-committed working-tree file.
func (a *GitMissionAnchor) decisionsBytes(ctx context.Context) ([]byte, error) {
	rel := AnchorDir + "/" + DecisionsFile
	tracked, err := ExistsAtHead(ctx, a.workdir, rel)
	if err != nil {
		return nil, err
	}
	if tracked {
		return ShowAtHeadRaw(ctx, a.workdir, rel)
	}
	data, err := os.ReadFile(a.path(DecisionsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// loadDecisions parses the committed decision chain after verifying it (*DecisionChainError).
func (a *GitMissionAnchor) loadDecisions(ctx context.Context) (DecisionChain, error) {
	check, err := a.VerifyDecisions(ctx)
	if err != nil {
		return DecisionChain{}, err
	}
	if !check.OK {
		return DecisionChain{}, &DecisionChainError{Msg: AnchorDir + "/" + DecisionsFile + " in " +
			a.workdir + " failed hash-chain verification (" + check.Problem + "): the committed " +
			"decision history was altered, so the mission refuses to continue"}
	}
	data, err := a.decisionsBytes(ctx)
	if err != nil {
		return DecisionChain{}, err
	}
	return ParseDecisionChain(data)
}

// boundProgress trims the OLDEST progress entries so text fits in limit characters (the header
// up to and including the "## Progress" marker is kept).
func boundProgress(text string, limit int) string {
	if runeLen(text) <= limit {
		return text
	}
	split := 0
	if at := strings.Index(text, progressMarker); at != -1 {
		split = at + len(progressMarker)
	}
	header, body := text[:split], text[split:]
	const note = "- _(older entries trimmed)_\n"
	var lines []string
	for _, ln := range pySplitlines(body, true) {
		if ln != note {
			lines = append(lines, ln)
		}
	}
	size := runeLen(header) + runeLen(note)
	for _, ln := range lines {
		size += runeLen(ln)
	}
	for len(lines) > 0 && size > limit {
		size -= runeLen(lines[0])
		lines = lines[1:]
	}
	return header + note + strings.Join(lines, "")
}
