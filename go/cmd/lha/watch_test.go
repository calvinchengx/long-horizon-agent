package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// One fixed mission + events payload, shared by the render test and the parity test: a durable
// mission waiting on a human, with an open gate, every event kind's summary and an unknown-cost
// call. The expected render is the exact bytes both implementations print.
const watchMissionJSON = `{"mission_id":"mission_abc123","title":"Ship the fabric emulator",` +
	`"status":"WAITING_ON_HUMAN","durable":true,` +
	`"items":{"total":4,"todo":1,"in_progress":1,"blocked":1,"done":1,"split":0},` +
	`"spend":{"calls":12,"known_usd":3.5,"unknown_cost_calls":0,"input_tokens":100,"output_tokens":200},` +
	`"live":{"status":"WAITING_ON_HUMAN","cycles":2,` +
	`"gate":{"gate_id":"g1","question":"Approve ` + "`git push`" + `?"},` +
	`"open_question":null,"resume_at":null,"steer_notes":[],"pending_edits":0}}`

const watchEventsJSON = `{"events":[` +
	`{"id":1,"cycle_id":"c1","ts":"2026-10-04T00:00:01+00:00","kind":"tool_call","payload":{"tool":"run_command","ok":true}},` +
	`{"id":2,"cycle_id":"c1","ts":"2026-10-04T00:00:02+00:00","kind":"tool_call","payload":{"tool":"run_command","ok":false}},` +
	`{"id":3,"cycle_id":"","ts":"2026-10-04T00:00:03+00:00","kind":"cycle_started","payload":{"item_id":"01"}},` +
	`{"id":4,"cycle_id":"c1","ts":"2026-10-04T00:00:04+00:00","kind":"verify","payload":{"verdict":"passed"}},` +
	`{"id":5,"cycle_id":"c1","ts":"2026-10-04T00:00:05+00:00","kind":"session_progress","payload":{"turns":3}},` +
	`{"id":6,"cycle_id":"c1","ts":"2026-10-04T00:00:06+00:00","kind":"parallel_wave","payload":{"items":["01","02"]}},` +
	`{"id":7,"cycle_id":"","ts":"2026-10-04T00:00:07+00:00","kind":"tool_approval","payload":{"decision":"reject"}},` +
	`{"id":8,"cycle_id":"c1","ts":"2026-10-04T00:00:08+00:00","kind":"llm_turn","payload":{}}` +
	`],"next_after":8}`

const watchExpected = "mission_abc123  WAITING_ON_HUMAN  durable\n" +
	"Ship the fabric emulator\n" +
	"items 1/4 done, 1 in progress, 1 blocked, 0 split\n" +
	"spend $3.5000 over 12 calls\n" +
	"gate g1: Approve `git push`?\n" +
	"\n" +
	"events:\n" +
	"  2026-10-04T00:00:08+00:00  c1  llm_turn\n" +
	"  2026-10-04T00:00:07+00:00  -  tool_approval  reject\n" +
	"  2026-10-04T00:00:06+00:00  c1  parallel_wave  01,02\n" +
	"  2026-10-04T00:00:05+00:00  c1  session_progress  turns 3\n" +
	"  2026-10-04T00:00:04+00:00  c1  verify  passed\n" +
	"  2026-10-04T00:00:03+00:00  -  cycle_started  01\n" +
	"  2026-10-04T00:00:02+00:00  c1  tool_call  run_command failed\n" +
	"  2026-10-04T00:00:01+00:00  c1  tool_call  run_command\n"

func watchFixture(t *testing.T) (watchMission, []watchEvent) {
	t.Helper()
	var mission watchMission
	if err := decodeWatchJSON(strings.NewReader(watchMissionJSON), &mission); err != nil {
		t.Fatalf("mission json: %v", err)
	}
	var page struct {
		Events []watchEvent `json:"events"`
	}
	if err := decodeWatchJSON(strings.NewReader(watchEventsJSON), &page); err != nil {
		t.Fatalf("events json: %v", err)
	}
	return mission, page.Events
}

func TestWatchRenderExact(t *testing.T) {
	mission, events := watchFixture(t)
	if got := watchRender(mission, events); got != watchExpected {
		t.Errorf("watchRender differs:\ngot:\n%s\nwant:\n%s", got, watchExpected)
	}
}

func TestWatchRenderLocalNoGateUnknownCost(t *testing.T) {
	mission := watchMission{
		MissionID: "mission_local",
		Title:     "Local mission",
		Status:    "RUNNING",
		Spend:     watchSpend{Calls: 3, UnknownCostCalls: 2},
	}
	want := "mission_local  RUNNING\nLocal mission\nitems -\n" +
		"spend $0.0000 over 3 calls (2 unpriced)\n\nevents:\n"
	if got := watchRender(mission, nil); got != want {
		t.Errorf("watchRender differs:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestResolveWatchTargetTakesTokenFromServeURL(t *testing.T) {
	base, token := resolveWatchTarget("http://127.0.0.1:8765/?token=abc", "")
	if base != "http://127.0.0.1:8765" || token != "abc" {
		t.Errorf("resolveWatchTarget = (%q, %q)", base, token)
	}
	base, token = resolveWatchTarget("http://127.0.0.1:8765/?token=abc", "xyz")
	if base != "http://127.0.0.1:8765" || token != "xyz" {
		t.Errorf("resolveWatchTarget explicit token = (%q, %q)", base, token)
	}
}

// TestWatchFetchAsksForTheNewestEvents: the events endpoint pages forward from `after`
// (exclusive), so lha watch asks for those after last_event.id - limit.
func TestWatchFetchAsksForTheNewestEvents(t *testing.T) {
	seen := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events") {
			seen = r.URL.Query().Get("after")
			_, _ = io.WriteString(w, `{"events":[],"next_after":30}`)
			return
		}
		_, _ = io.WriteString(w, `{"mission_id":"m","title":"t","status":"RUNNING","durable":false,`+
			`"items":null,"spend":{"calls":0,"known_usd":0,"unknown_cost_calls":0},`+
			`"last_event":{"id":30,"kind":"llm_turn","ts":"x"}}`)
	}))
	defer server.Close()
	if _, _, err := watchFetch(&http.Client{}, server.URL, "", "m", 10); err != nil {
		t.Fatal(err)
	}
	if seen != "20" {
		t.Errorf("after = %q, want 20", seen)
	}
}

// TestWatchOnceParity runs the built lha watch --once against a fixed mission + events server and
// (when uv is available) the Python lha watch --once against the same server, and compares stdout
// byte for byte.
func TestWatchOnceParity(t *testing.T) {
	bin := lhaBinary(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events") {
			_, _ = io.WriteString(w, watchEventsJSON)
			return
		}
		_, _ = io.WriteString(w, watchMissionJSON)
	}))
	defer server.Close()

	args := []string{"watch", "mission_abc123", "--once", "--url", server.URL, "--token", "x"}
	goRun := runProcess(t, t.TempDir(), processEnv(), bin, args...)
	if goRun.code != 0 || goRun.stdout != watchExpected {
		t.Fatalf("go: code %d\nstdout:\n%s\nstderr:\n%s", goRun.code, goRun.stdout, goRun.stderr)
	}
	if !pythonAvailable(t) {
		return
	}
	pyRun := runPythonLHA(t, t.TempDir(), processEnv(), args...)
	if pyRun.code != goRun.code || pyRun.stdout != goRun.stdout {
		t.Errorf("stdout differs:\ngo (%d):\n%s\npy (%d):\n%s\npy stderr:\n%s",
			goRun.code, goRun.stdout, pyRun.code, pyRun.stdout, pyRun.stderr)
	}
}
