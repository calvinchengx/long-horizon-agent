package obs

import "testing"

func TestAFailingListenerNeverBreaksRecording(t *testing.T) {
	r := NewTraceRecorder(nil)
	r.AddListener(func(TraceEvent) { panic("listener down") })
	got := []string{}
	r.AddListener(func(e TraceEvent) { got = append(got, e.Kind) }) // later listeners still run
	e := r.Record("test_x", "m", "")
	if e.Kind != "test_x" || len(r.Events()) != 1 || len(got) != 1 {
		t.Fatal(e, r.Events(), got)
	}
}
