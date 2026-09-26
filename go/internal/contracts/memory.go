package contracts

import (
	"context"
	"fmt"
)

// The memory contracts (python: lha.contracts.memory, and the Skill model of lha.memory.skills).
//
// Tiered memory keeps the agent coherent across thousands of steps and many context resets:
// episodic (what happened), semantic (distilled facts, retrieved by similarity) and procedural
// (reusable, self-verified skills). An Embedder turns text into vectors; a SemanticIndex stores
// and retrieves by similarity. Every record carries the embedding model+version so vectors are
// never compared across versions.

// MemoryRecord is a single memory item. EmbeddingModel/EmbeddingVersion are "" when unset
// (python: None).
type MemoryRecord struct {
	ID               string            `json:"id"`
	Kind             string            `json:"kind"` // "episodic" | "semantic" | "procedural" | "fact" | ...
	Text             string            `json:"text"`
	Metadata         map[string]string `json:"metadata"`
	EmbeddingModel   string            `json:"embedding_model"`
	EmbeddingVersion string            `json:"embedding_version"`
	Valid            bool              `json:"valid"` // soft invalidation (never hard-delete)
}

// NewMemoryRecord is a valid record with empty metadata.
func NewMemoryRecord(id, kind, text string, metadata map[string]string) MemoryRecord {
	if metadata == nil {
		metadata = map[string]string{}
	}
	return MemoryRecord{ID: id, Kind: kind, Text: text, Metadata: metadata, Valid: true}
}

// RetrievalHit is a retrieved record with its score.
type RetrievalHit struct {
	Record MemoryRecord
	Score  float64
}

// Embedder turns text into vectors (one per input text, each of length Dim()).
type Embedder interface {
	Name() string
	Version() string
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// SemanticIndex stores records and retrieves them by semantic similarity.
type SemanticIndex interface {
	Add(ctx context.Context, records []MemoryRecord) error
	Query(ctx context.Context, text string, k int) ([]RetrievalHit, error)
}

// Skill is a learned, reusable capability (python: lha.memory.skills.Skill). ExpiresAt is an ISO
// date ("" = never; python: None).
type Skill struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Code          string   `json:"code"`
	Preconditions []string `json:"preconditions"`
	Namespace     string   `json:"namespace"` // usually the repo; "global" only after cross-repo validation
	Provenance    string   `json:"provenance"`
	ExpiresAt     string   `json:"expires_at"`
	Verified      bool     `json:"verified"` // admitted only when it passed the deterministic test gate
	Uses          int      `json:"uses"`
}

// SkillNotVerifiedError is returned when admitting a skill that has not passed the test gate.
type SkillNotVerifiedError struct{ Message string }

func (e *SkillNotVerifiedError) Error() string { return e.Message }

// UnverifiedSkill is the error for storing skill id unverified (python's message).
func UnverifiedSkill(action, id string) error {
	return &SkillNotVerifiedError{Message: fmt.Sprintf(
		"refusing to %s unverified skill %s (must pass the test gate first)", action, PyRepr(id))}
}
