package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Sleep-time memory consolidation: episodes -> durable semantic facts (python:
// lha.memory.consolidation). The model distills episodes into reusable facts in bounded batches,
// with a deterministic fallback so it never loses anything: if a reply is unusable, that batch's
// episodes are kept verbatim (and the failure is counted). Forgetting is conservative:
// SoftInvalidate flips Valid=false, never rewrites a fact.

const consolidationInstructions = "Distill the episodes below into a short list of durable, reusable FACTS about the " +
	"repository/domain (each a single sentence). Reply with ONLY a JSON array of strings."

// DefaultBatchSize is the episodes sent to the model per consolidation call.
const DefaultBatchSize = 200

// ConsolidationResult is the outcome of a pass: every distilled fact plus, for any batch whose
// reply could not be parsed, that batch's episodes verbatim (metadata consolidation=verbatim).
type ConsolidationResult struct {
	Facts         []contracts.MemoryRecord
	Batches       int
	FailedBatches int
}

// OK is true when no batch failed.
func (r ConsolidationResult) OK() bool { return r.FailedBatches == 0 }

// parseFacts parses a JSON array of facts; ok=false means the reply was unparseable.
func parseFacts(text string) ([]string, bool) {
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start == -1 || end == -1 || end <= start {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text[start : end+1])))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil || dec.More() {
		return nil, false
	}
	list, ok := parsed.([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, x := range list {
		if s := pyfmt.PyStrip(pyfmt.PyStr(x)); s != "" {
			out = append(out, s)
		}
	}
	return out, true
}

func newFactID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "fact_" + hex.EncodeToString(b[:])
}

// Consolidate distills ALL episodes (batches of at most batchSize, <= 0 => DefaultBatchSize...
// a negative size is an error) into semantic facts; an unparseable batch is kept verbatim.
func Consolidate(ctx context.Context, model contracts.ModelProvider, episodes []string, missionID string, batchSize int) (ConsolidationResult, error) {
	if batchSize == 0 {
		batchSize = DefaultBatchSize
	}
	if batchSize < 1 {
		return ConsolidationResult{}, errors.New("batch_size must be >= 1")
	}
	result := ConsolidationResult{Facts: []contracts.MemoryRecord{}}
	for offset := 0; offset < len(episodes); offset += batchSize {
		batch := episodes[offset:min(len(episodes), offset+batchSize)]
		result.Batches++
		lines := make([]string, len(batch))
		for i, e := range batch {
			lines[i] = "- " + e
		}
		reply, err := model.Complete(ctx, []contracts.ModelMessage{
			{Role: "system", Content: "You are the Librarian; consolidate memory."},
			{Role: "user", Content: consolidationInstructions + "\n\nEpisodes:\n" + strings.Join(lines, "\n")},
		}, nil, 0)
		if err != nil {
			return result, err
		}
		metadata := map[string]string{}
		if missionID != "" {
			metadata["mission_id"] = missionID
		}
		facts, ok := parseFacts(reply.Text)
		if !ok {
			result.FailedBatches++
			seen := map[string]bool{}
			facts = []string{}
			for _, e := range batch {
				if !seen[e] {
					seen[e] = true
					facts = append(facts, e)
				}
			}
			metadata["consolidation"] = "verbatim"
		}
		for _, f := range facts {
			md := make(map[string]string, len(metadata))
			for k, v := range metadata {
				md[k] = v
			}
			result.Facts = append(result.Facts, contracts.NewMemoryRecord(newFactID(), "semantic", f, md))
		}
	}
	return result, nil
}

// SoftInvalidate marks matching valid records invalid (never deletes or rewrites); returns count.
func SoftInvalidate(records []contracts.MemoryRecord, predicate func(contracts.MemoryRecord) bool) int {
	count := 0
	for i := range records {
		if records[i].Valid && predicate(records[i]) {
			records[i].Valid = false
			count++
		}
	}
	return count
}
