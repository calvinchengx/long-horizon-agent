package obs

import "testing"

// Ported from python/tests/unit/test_bounded_memory.py: the recorder keeps the newest events.
func TestTheTraceRecorderKeepsTheNewestEvents(t *testing.T) {
	defer func(n int) { MaxTraceEvents = n }(MaxTraceEvents)
	MaxTraceEvents = 10
	r := NewTraceRecorder(nil)
	for n := 0; n < 100; n++ {
		r.Record("test_tick", "m1", "", F("n", n))
	}
	events := r.Events()
	last, _ := events[len(events)-1].Data.Get("n")
	if len(events) < 10 || len(events) > 11 || last != 99 || r.Dropped()+len(events) != 100 {
		t.Fatal(len(events), last, r.Dropped())
	}
}
