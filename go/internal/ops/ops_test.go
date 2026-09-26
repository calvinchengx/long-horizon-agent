package ops

import (
	"reflect"
	"testing"
)

func TestShouldDeclareImpossible(t *testing.T) {
	if ShouldDeclareImpossible(2, 3) || !ShouldDeclareImpossible(3, 3) || !ShouldDeclareImpossible(4, 3) {
		t.Fatal("threshold is inclusive")
	}
}

func TestDecideSafePark(t *testing.T) {
	d := DecideSafePark([]DependencyStatus{
		{Name: "sandbox", Health: HealthDown}, {Name: "git", Health: HealthDown},
		{Name: "model", Health: HealthOK}, {Name: "postgres", Health: HealthDegraded},
	})
	if !d.Park || d.Reason != "critical dependency down: ['git', 'sandbox']" || !reflect.DeepEqual(d.Degraded, []string{"postgres"}) {
		t.Fatalf("got %+v", d)
	}
	d = DecideSafePark([]DependencyStatus{{Name: "git", Health: HealthOK}, {Name: "langfuse", Health: HealthDown}})
	if d.Park || d.Reason != "operational (some optionals degraded)" || !reflect.DeepEqual(d.Degraded, []string{"langfuse"}) {
		t.Fatalf("got %+v", d)
	}
	// An optional dependency DOWN never parks; an unknown one is ignored.
	if DecideSafePark([]DependencyStatus{{Name: "whatever", Health: HealthDown}}).Park {
		t.Fatal("unknown dependency parked")
	}
}

func TestDecideMemoryMode(t *testing.T) {
	m := DecideMemoryMode([]DependencyStatus{{Name: "postgres", Health: HealthOK}, {Name: "embeddings", Health: HealthOK}})
	if !m.Dense || m.GitGrep || m.Label() != "hybrid" {
		t.Fatalf("got %+v", m)
	}
	m = DecideMemoryMode([]DependencyStatus{
		{Name: "pgvector", Health: HealthDown, Detail: "no extension"}, {Name: "embeddings", Health: HealthDegraded},
	})
	if m.Dense || !m.GitGrep || m.Label() != "lexical" ||
		m.Reason != "lexical-only retrieval (BM25 + git grep): embeddings: degraded; pgvector: no extension" ||
		!reflect.DeepEqual(m.Degraded, []string{"embeddings", "pgvector"}) {
		t.Fatalf("got %+v", m)
	}
}
