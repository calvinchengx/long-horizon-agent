package coordination

import "sync"

// A shared blackboard with response-board separation (python: lha.coordination.blackboard).
// Agents read/write a shared board instead of chatting directly. Writes within a round go to a
// SEPARATE response board and are only promoted into the main board between rounds — so one
// agent's output can't silently bias another mid-round.

// BoardEntry is one post.
type BoardEntry struct {
	Author  string
	Content string
}

// MaxBoardEntries is how many main-board entries a Blackboard keeps (the newest; python:
// MAX_BOARD_ENTRIES); the orchestrator shows later rounds only the last few.
var MaxBoardEntries = 256

// Blackboard is the main board plus this round's response board (safe for concurrent use).
type Blackboard struct {
	mu        sync.Mutex
	main      []BoardEntry
	responses []BoardEntry
	posted    int
}

// Post appends to the main board (its newest MaxBoardEntries are kept).
func (b *Blackboard) Post(author, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.main = append(b.main, BoardEntry{author, content})
	b.posted++
	b.trimLocked()
}

func (b *Blackboard) trimLocked() {
	if len(b.main) > MaxBoardEntries {
		b.main = append([]BoardEntry(nil), b.main[len(b.main)-MaxBoardEntries:]...)
	}
}

// Posted is every entry ever promoted to the main board, kept or not.
func (b *Blackboard) Posted() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.posted
}

// Respond writes to the per-round response board (not yet visible on the main board).
func (b *Blackboard) Respond(author, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.responses = append(b.responses, BoardEntry{author, content})
}

// Read is a copy of the main board.
func (b *Blackboard) Read() []BoardEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BoardEntry{}, b.main...)
}

// ReadResponses is a copy of this round's response board.
func (b *Blackboard) ReadResponses() []BoardEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BoardEntry{}, b.responses...)
}

// CommitRound promotes this round's responses into the main board and clears the response board.
func (b *Blackboard) CommitRound() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.main = append(b.main, b.responses...)
	b.posted += len(b.responses)
	b.responses = nil
	b.trimLocked()
}
