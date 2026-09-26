package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// Postgres + pgvector semantic store (python: lha.memory.semantic_pg.PgSemanticIndex), the
// production SemanticIndex. Queries are gated by (embedding_model, embedding_version) so vectors
// are never compared across embedder versions. Vectors are sent as pgvector text literals and
// cast explicitly (no client-side adapter is needed). Rows are keyed by MemoryRecord.ID and
// re-adding an id upserts. The embedder's dim must equal the vector(N) column width.

// DefaultColumnDim is the width of semantic_memory.embedding (vector(1024)).
const DefaultColumnDim = 1024

// EmbeddingDimensionError is the embedder's width not matching the pgvector column.
type EmbeddingDimensionError struct{ Message string }

func (e *EmbeddingDimensionError) Error() string { return e.Message }

// PgSemanticIndex is a SemanticIndex on Postgres + pgvector (HNSW cosine). With MissionID set
// the index is scoped to one mission.
type PgSemanticIndex struct {
	pool      *pgxpool.Pool
	embedder  contracts.Embedder
	columnDim int
	MissionID string
}

// NewPgSemanticIndex checks the embedder width against columnDim (<= 0 => 1024).
func NewPgSemanticIndex(pool *pgxpool.Pool, embedder contracts.Embedder, columnDim int, missionID string) (*PgSemanticIndex, error) {
	if columnDim <= 0 {
		columnDim = DefaultColumnDim
	}
	if embedder.Dim() != columnDim {
		return nil, &EmbeddingDimensionError{Message: fmt.Sprintf(
			"embedder %s produces dim=%d vectors but the semantic_memory.embedding column is vector(%d)",
			contracts.PyRepr(embedder.Name()), embedder.Dim(), columnDim)}
	}
	return &PgSemanticIndex{pool: pool, embedder: embedder, columnDim: columnDim, MissionID: missionID}, nil
}

func (x *PgSemanticIndex) checkVector(v []float64) error {
	if len(v) != x.columnDim {
		return &EmbeddingDimensionError{Message: fmt.Sprintf("embedder %s returned a dim=%d vector; expected %d",
			contracts.PyRepr(x.embedder.Name()), len(v), x.columnDim)}
	}
	return nil
}

// vectorLiteral is pgvector's text form "[x,y,...]".
func vectorLiteral(v []float64) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = persistence.PyDumps(f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Add embeds and upserts records.
func (x *PgSemanticIndex) Add(ctx context.Context, records []contracts.MemoryRecord) error {
	if len(records) == 0 {
		return nil
	}
	texts := make([]string, len(records))
	for i, r := range records {
		texts[i] = r.Text
	}
	vectors, err := x.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	for _, v := range vectors {
		if err := x.checkVector(v); err != nil {
			return err
		}
	}
	for i, r := range records {
		var mission any
		if x.MissionID != "" {
			mission = x.MissionID
		} else if m, ok := r.Metadata["mission_id"]; ok {
			mission = m
		}
		md := r.Metadata
		if md == nil {
			md = map[string]string{}
		}
		if _, err := x.pool.Exec(ctx,
			"INSERT INTO semantic_memory (id, mission_id, kind, text, metadata, "+
				"embedding, embedding_model, embedding_version, valid) "+
				"VALUES ($1, $2, $3, $4, $5::text::jsonb, $6::text::vector, $7, $8, $9) "+
				"ON CONFLICT (id) DO UPDATE SET mission_id = EXCLUDED.mission_id, "+
				"kind = EXCLUDED.kind, text = EXCLUDED.text, metadata = EXCLUDED.metadata, "+
				"embedding = EXCLUDED.embedding, "+
				"embedding_model = EXCLUDED.embedding_model, "+
				"embedding_version = EXCLUDED.embedding_version, valid = EXCLUDED.valid",
			r.ID, mission, r.Kind, r.Text, persistence.PyDumps(md), vectorLiteral(vectors[i]),
			x.embedder.Name(), x.embedder.Version(), r.Valid); err != nil {
			return err
		}
	}
	return nil
}

// Query is the k nearest valid rows of this embedder's model+version (score = 1 - cosine distance).
func (x *PgSemanticIndex) Query(ctx context.Context, text string, k int) ([]contracts.RetrievalHit, error) {
	qv, err := x.embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if err := x.checkVector(qv[0]); err != nil {
		return nil, err
	}
	lit := vectorLiteral(qv[0])
	params := []any{lit, x.embedder.Name(), x.embedder.Version()}
	scope := ""
	if x.MissionID != "" {
		params = append(params, x.MissionID)
		scope = " AND mission_id = $4"
	}
	params = append(params, k)
	rows, err := x.pool.Query(ctx,
		"SELECT id, text, embedding_model, embedding_version, valid, "+
			"1 - (embedding <=> $1::text::vector) AS score, kind, metadata::text "+
			"FROM semantic_memory "+
			"WHERE valid AND embedding IS NOT NULL "+
			"AND embedding_model = $2 AND embedding_version = $3"+
			scope+fmt.Sprintf(" ORDER BY embedding <=> $1::text::vector LIMIT $%d", len(params)), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := []contracts.RetrievalHit{}
	for rows.Next() {
		var r contracts.MemoryRecord
		var score float64
		var kind *string
		var metadata string
		if err := rows.Scan(&r.ID, &r.Text, &r.EmbeddingModel, &r.EmbeddingVersion, &r.Valid, &score, &kind, &metadata); err != nil {
			return nil, err
		}
		r.Kind = "semantic"
		if kind != nil && *kind != "" {
			r.Kind = *kind
		}
		if r.Metadata, err = persistence.DecodeStringMap(metadata); err != nil {
			return nil, err
		}
		hits = append(hits, contracts.RetrievalHit{Record: r, Score: score})
	}
	return hits, rows.Err()
}
