package agent

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// A model that stays unreachable after its retries and fallbacks ends a local run cleanly
// (python/tests/unit/test_model_unavailable.py): "model unavailable: ..." stop, ABORTED row, the
// anchor keeps the last committed cycle.

type timeoutTransport struct{}

func (timeoutTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, os.ErrDeadlineExceeded
}

func noSleep(context.Context, float64) error { return nil }

// timingOutOllama is an Ollama provider whose every request times out (the machine under load).
func timingOutOllama(t *testing.T) contracts.ModelProvider {
	t.Helper()
	zero := 0.0
	m, err := model.NewOpenAICompat(model.OpenAICompatOptions{
		BaseURL: "http://ollama.test/v1", ModelName: "qwen3:8b", Label: "ollama",
		PriceInPerMTok: &zero, PriceOutPerMTok: &zero,
		Client: &http.Client{Transport: timeoutTransport{}}, Sleep: noSleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// diesAfter serves ok turns, then every call times out.
type diesAfter struct {
	*model.StubModel
	ok int
}

func (d *diesAfter) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	if d.ok <= 0 {
		return contracts.TurnResult{}, &url.Error{Op: "Post", URL: "http://ollama.test/v1/chat/completions", Err: os.ErrDeadlineExceeded}
	}
	d.ok--
	return d.StubModel.Complete(ctx, messages, tools, maxTokens)
}

func TestLocalRunStopsCleanlyWhenTheModelKeepsTimingOut(t *testing.T) {
	settings, path := storeSettings(t)
	dir := t.TempDir()
	o := runOpts(t, dir, settings, timingOutOllama(t), passCheck)
	o.Checklist = items(2)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || s.StoppedReason != "model unavailable: ReadTimeout after 4 attempts" ||
		s.Completed || s.Cycles != 0 || s.ItemsDone != 0 || s.ItemsTotal != 2 {
		t.Fatalf("%+v %v", s, err)
	}
	if !strings.Contains(s.TraceJSONL, `"kind":"model_unavailable"`) {
		t.Fatalf("trace: %s", s.TraceJSONL)
	}
	row, err := openRunStore(t, path).GetMission(context.Background(), s.MissionID)
	if err != nil || row.Status != "ABORTED" {
		t.Fatalf("%+v %v", row, err)
	}
	if out := (&fixture{dir: dir}).git(t, "status", "--porcelain", "--", ".lha"); out != "" {
		t.Fatalf("anchor half-committed: %q", out)
	}
}

func TestCommittedCyclesSurviveALaterOutage(t *testing.T) {
	settings, _ := storeSettings(t, "LHA_MAX_TURNS_PER_CYCLE=2")
	dir := t.TempDir()
	o := runOpts(t, dir, settings, &diesAfter{StubModel: model.NewStub([]contracts.TurnResult{done}), ok: 1}, passCheck)
	o.Checklist = items(2)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || s.StoppedReason != "model unavailable: ReadTimeout after 1 attempt" ||
		s.Cycles != 1 || s.ItemsDone != 1 || s.HeadSHA == "" || s.HeadSHA != strings.TrimSpace((&fixture{dir: dir}).git(t, "rev-parse", "HEAD")) {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestNonTransientModelErrorsStillFail(t *testing.T) {
	settings, _ := storeSettings(t)
	boom := errors.New("malformed response")
	o := runOpts(t, t.TempDir(), settings, &failing{StubModel: model.NewStub(nil), err: boom}, passCheck)
	if _, err := RunMissionLocal(context.Background(), o); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestPlannerOutageIsAnUnavailableError(t *testing.T) {
	o := PlanOptions{RunOptions: RunOptions{Workdir: t.TempDir(), Title: "t", Settings: runnerSettings(t), OpenToolbox: fakeOpener(nil)},
		Task: "do it", PlannerModel: timingOutOllama(t)}
	_, err := PlanAndRunLocal(context.Background(), o)
	var down *model.UnavailableError
	if !errors.As(err, &down) || err.Error() != "model unavailable: ReadTimeout after 4 attempts" {
		t.Fatalf("err = %v", err)
	}
}

type failing struct {
	*model.StubModel
	err error
}

func (f *failing) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, f.err
}
