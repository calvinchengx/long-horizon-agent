package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/agenttest"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from python/tests/unit/test_sandbox_egress.py (the loop half): the sandbox session's
// egress proxy requests are committed with the cycle's checkpoint as sandbox_egress events.

type egressSession struct {
	*agenttest.Session
	drained int
}

func (s *egressSession) DrainEgressEvents(context.Context) []contracts.EventRecord {
	s.drained++
	return []contracts.EventRecord{{Kind: "sandbox_egress", Payload: contracts.Payload(
		"decision", "allow", "method", "CONNECT", "host", "pypi.org", "port", 443,
		"detail", "151.101.0.223:443", "count", 3,
	)}}
}

type egressToolbox struct {
	*agenttest.Toolbox
	sess *egressSession
}

func (t egressToolbox) Session() contracts.SandboxSession { return t.sess }

func TestTheLoopCommitsTheSessionsEgressEvents(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "ws")
	var sess *egressSession
	o := runOpts(t, workdir, runnerSettings(t), model.NewStub([]contracts.TurnResult{done}), passCheck)
	o.OpenToolbox = func(_ context.Context, req ToolboxRequest) (Toolbox, error) {
		box := agenttest.NewToolbox(req.Workdir)
		sess = &egressSession{Session: box.Sess}
		return egressToolbox{Toolbox: box, sess: sess}, nil
	}
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || !s.Completed {
		t.Fatalf("%+v %v", s, err)
	}
	raw, err := state.RunGit(context.Background(), workdir, "show", "HEAD:.lha/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var egress *contracts.EventRecord
	for _, line := range strings.Split(raw, "\n") {
		var e contracts.EventRecord
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(line, err)
		}
		if e.Kind == "sandbox_egress" {
			egress = &e
		}
	}
	if egress == nil || egress.CycleID == "" || egress.Payload.Plain()["host"] != "pypi.org" ||
		egress.Payload.Plain()["count"] != 3.0 || sess.drained == 0 {
		t.Fatalf("egress event: %+v\n%s", egress, raw)
	}
	if !strings.Contains(s.TraceJSONL, `"kind":"sandbox_egress"`) {
		t.Fatalf("trace: %s", s.TraceJSONL)
	}
}
