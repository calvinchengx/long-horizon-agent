package coordination

import (
	"reflect"
	"testing"
)

// Ported from python/tests/unit/test_bounded_memory.py: the blackboard keeps the newest posts and
// counts them all.
func TestTheBlackboardKeepsTheNewestPostsAndCountsThemAll(t *testing.T) {
	defer func(n int) { MaxBoardEntries = n }(MaxBoardEntries)
	MaxBoardEntries = 5
	var b Blackboard
	for _, p := range []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7"} {
		b.Post("a", p)
	}
	b.Respond("b", "r")
	b.CommitRound()
	got := []string{}
	for _, e := range b.Read() {
		got = append(got, e.Content)
	}
	if !reflect.DeepEqual(got, []string{"p4", "p5", "p6", "p7", "r"}) || b.Posted() != 9 {
		t.Fatal(got, b.Posted())
	}
}
