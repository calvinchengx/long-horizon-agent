package spec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/egressproxy"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// wireSpec is spec/state/wire_bytes.json: the exact bytes Python writes for representative
// events, the anchor's events.ndjson, lease and egress events, .lha/ownership.json and gate
// webhook bodies. Every comparison here is of raw bytes, never of parsed JSON: key order, float
// formatting and escaping all count.
type wireSpec struct {
	Events           []string `json:"events"`
	AnchorEventsFile string   `json:"anchor_events_file"`
	LeaseEvents      []struct {
		Decision struct {
			Writer        string  `json:"writer"`
			Path          string  `json:"path"`
			Reason        string  `json:"reason"`
			Granted       bool    `json:"granted"`
			PreviousOwner *string `json:"previous_owner"`
			Why           string  `json:"why"`
		} `json:"decision"`
		Line string `json:"line"`
	} `json:"lease_events"`
	EgressEvents []string `json:"egress_events"`
	Ownership    []struct {
		Items    []string            `json:"items"`
		Files    map[string][]string `json:"files"`
		Release  []string            `json:"release"`
		Reassign [][2]string         `json:"reassign"`
		JSON     string              `json:"json"`
	} `json:"ownership"`
	Gates []struct {
		Notice json.RawMessage `json:"notice"`
		Body   string          `json:"body"`
		Event  string          `json:"event"`
	} `json:"gates"`
}

func loadWire(t *testing.T) wireSpec {
	t.Helper()
	var s wireSpec
	Load(t, "state/wire_bytes.json", &s)
	if len(s.Events) == 0 || len(s.LeaseEvents) == 0 || len(s.EgressEvents) == 0 || len(s.Ownership) == 0 || len(s.Gates) == 0 {
		t.Fatal("empty wire spec")
	}
	return s
}

func eventLine(t *testing.T, e contracts.EventRecord) string {
	t.Helper()
	data, err := state.PydanticJSON(e, false)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sameBytes(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s bytes differ\n go:     %q\n python: %q", what, got, want)
	}
}

// TestWireEventLines: an event read from Python's events.ndjson is written back byte for byte.
func TestWireEventLines(t *testing.T) {
	for _, line := range loadWire(t).Events {
		var e contracts.EventRecord
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		sameBytes(t, "event", eventLine(t, e), line)
	}
}

// TestWireAnchorEventsFile: the anchor's checkpoint writes Python's events.ndjson bytes.
func TestWireAnchorEventsFile(t *testing.T) {
	s := loadWire(t)
	ctx := context.Background()
	dir := t.TempDir()
	anchor := state.NewGitMissionAnchor(dir)
	items := contracts.Checklist{Items: []contracts.ChecklistItem{{ID: "01", Description: "one", Status: contracts.StatusTodo}}}
	if _, err := anchor.Initialize(ctx, "Wire", "bytes", items); err != nil {
		t.Fatal(err)
	}
	events := []contracts.EventRecord{}
	for _, line := range s.Events {
		var e contracts.EventRecord
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c1", ProgressSummary: "- c1",
		Checklist: items, Events: events}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, state.AnchorDir, state.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	sameBytes(t, ".lha/events.ndjson", string(data), s.AnchorEventsFile)
}

// TestWireBuiltEvents: events Go builds itself (lease decisions, egress proxy logs) come out in
// Python's key order.
func TestWireBuiltEvents(t *testing.T) {
	s := loadWire(t)
	for _, c := range s.LeaseEvents {
		d := coordination.LeaseDecision{Writer: c.Decision.Writer, Path: c.Decision.Path, Reason: c.Decision.Reason,
			Granted: c.Decision.Granted, Why: c.Decision.Why}
		if c.Decision.PreviousOwner != nil {
			d.PreviousOwner, d.HasPrevious = *c.Decision.PreviousOwner, true
		}
		sameBytes(t, "lease event", eventLine(t, coordination.NewLeaseEvent(d, "c9")), c.Line)
	}
	var egress struct {
		ProxyLog struct {
			Lines  []string        `json:"lines"`
			Events json.RawMessage `json:"events"` // checked by TestSandboxEgress
		} `json:"proxy_log"`
	}
	Load(t, "execution/sandbox_egress.json", &egress)
	got := egressproxy.ParseProxyLog(egress.ProxyLog.Lines)
	if len(got) != len(s.EgressEvents) {
		t.Fatalf("%d egress events, want %d", len(got), len(s.EgressEvents))
	}
	for i, e := range got {
		sameBytes(t, "egress event", eventLine(t, e), s.EgressEvents[i])
	}
}

// TestWireOwnershipJSON: .lha/ownership.json keeps Python's assignment order through release and
// reassign.
func TestWireOwnershipJSON(t *testing.T) {
	for _, c := range loadWire(t).Ownership {
		items := make([]contracts.ChecklistItem, len(c.Items))
		for i, id := range c.Items {
			items[i] = contracts.ChecklistItem{ID: id, Description: "item " + id, Status: contracts.StatusTodo}
		}
		m := agents.AssignOwnership(items, c.Files)
		for _, w := range c.Release {
			m.Release(w)
		}
		for _, r := range c.Reassign {
			if _, err := m.Reassign(r[0], r[1]); err != nil {
				t.Fatal(err)
			}
		}
		data, err := coordination.OwnershipJSON(m)
		if err != nil {
			t.Fatal(err)
		}
		sameBytes(t, "ownership.json", string(data), c.JSON)
		back, err := coordination.OwnershipJSON(mustReadOwnership(t, data))
		if err != nil {
			t.Fatal(err)
		}
		sameBytes(t, "ownership.json (read back)", string(back), c.JSON)
	}
}

func mustReadOwnership(t *testing.T, data []byte) *coordination.FileOwnershipMap {
	t.Helper()
	m := coordination.NewFileOwnershipMap()
	if err := json.Unmarshal(data, m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestWireGateNotice: a durable gate event's webhook body and anchor event.
func TestWireGateNotice(t *testing.T) {
	for _, c := range loadWire(t).Gates {
		var n durable.GateNotice
		if err := json.Unmarshal(c.Notice, &n); err != nil {
			t.Fatal(err)
		}
		payload := durable.GateNoticePayload(n)
		sameBytes(t, "gate webhook body", string(payload.JSON()), c.Body)
		e := contracts.EventRecord{Kind: "gate_" + n.Event, CycleID: "gate:" + n.GateID, Payload: durable.GatePayloadMap(payload)}
		sameBytes(t, "gate event", eventLine(t, e), c.Event)
	}
}
