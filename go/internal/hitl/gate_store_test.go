package hitl

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// Ported from python/tests/unit/test_hitl_ladder.py (the terminal approver's hitl_gates rows).

func scripted(answers []*string, escalation []float64, tty bool) *TerminalApprover {
	now := 0.0
	return NewTerminalApprover(TerminalOptions{
		TimeoutSeconds: 60, EscalationSeconds: escalation, Out: &strings.Builder{},
		IsTTY: func() bool { return tty },
		ReadLine: func(_ context.Context, timeout float64) (string, bool) {
			if len(answers) == 0 {
				now += timeout
				return "", false
			}
			a := answers[0]
			answers = answers[1:]
			if a == nil {
				now += timeout
				return "", false
			}
			return *a, true
		},
		Clock: func() float64 { return now },
	})
}

func TestTerminalApproverRecordsEachGateInTheStore(t *testing.T) {
	t.Setenv("LOGNAME", "alice")
	ctx := context.Background()
	store, err := persistence.OpenSQLite(ctx, filepath.Join(t.TempDir(), "s.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	approved := scripted([]*string{nil, s("y\n")}, []float64{5}, true)
	approved.BindStore(store)
	secret := `["curl", "-H", "Authorization: Bearer sk-ant-abcdefghijklmnopqrstu", "x"]`
	if _, err := approved.Request(ctx, contracts.GateRequest{GateID: "m:tool:c1", Question: "Allow?", Risk: contracts.RiskIrreversible,
		Context: map[string]string{"mission_id": "m1", "tool": "run_command", "argv": secret, "fingerprint": "c1"}}); err != nil {
		t.Fatal(err)
	}
	timedOut := scripted(nil, []float64{10, 30}, true)
	timedOut.BindStore(store)
	if _, err := timedOut.Request(ctx, contracts.GateRequest{GateID: "m2:tool:c9", Question: "Allow?"}); err != nil {
		t.Fatal(err)
	}
	noTTY := scripted(nil, nil, false)
	noTTY.BindStore(store)
	if _, err := noTTY.Request(ctx, contracts.GateRequest{GateID: "m3:tool:c1", Question: "Allow?"}); err != nil {
		t.Fatal(err)
	}
	gates, err := store.ListGates(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]persistence.GateRow{}
	for _, g := range gates {
		rows[g.MissionID] = g
	}
	one := rows["m1"]
	if one.Status != "RESOLVED" || *one.Decision != "approve" || *one.ResolvedBy != "terminal:alice" || one.Reminders != 1 ||
		one.GateID != "m:tool:c1" || strings.Join(one.Options, ",") != "approve,reject" || one.Request["tool"] != "run_command" ||
		strings.Contains(one.Request["argv"], "sk-ant") || one.Risk != "irreversible" {
		t.Fatalf("%+v", one)
	}
	two := rows["m2"] // the mission id comes from the gate id when the context has none
	if two.Status != "DEFAULTED" || *two.Decision != "reject" || *two.ResolvedBy != "timeout" || two.Reminders != 2 {
		t.Fatalf("%+v", two)
	}
	if three := rows["m3"]; three.Status != "DEFAULTED" || !strings.Contains(*three.ResolvedBy, "not a TTY") {
		t.Fatalf("%+v", three)
	}
}

type brokenRecorder struct{}

func (brokenRecorder) RecordGateEvent(context.Context, persistence.GateEvent) error {
	return errors.New("store down")
}

func TestTerminalApproverNeverFailsOnAStoreError(t *testing.T) {
	approver := scripted([]*string{s("y\n")}, nil, true)
	approver.BindStore(brokenRecorder{})
	res, err := approver.Request(context.Background(), contracts.GateRequest{GateID: "m:tool:x", Question: "q"})
	if err != nil || res.Decision != contracts.GateApprove || approver.StoreFailures() != 2 {
		t.Fatal(res, err, approver.StoreFailures()) // opened + resolved; the answer is unaffected
	}
}
