package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// A model that keeps timing out ends `lha run-local` / `lha mission` with the normal summary (or
// a one-line error before the mission started) and exit 1, never a crash
// (python/tests/unit/test_model_unavailable.py).

type timeoutTransport struct{}

func (timeoutTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, os.ErrDeadlineExceeded
}

func timingOutOllama(t *testing.T) contracts.ModelProvider {
	t.Helper()
	zero := 0.0
	m, err := model.NewOpenAICompat(model.OpenAICompatOptions{
		BaseURL: "http://ollama.test/v1", ModelName: "qwen3:8b", Label: "ollama",
		PriceInPerMTok: &zero, PriceOutPerMTok: &zero, Client: &http.Client{Transport: timeoutTransport{}},
		Sleep: func(context.Context, float64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRunLocalModelOutagePrintsTheSummaryAndExitsOne(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	linkFakeToolbox(t)
	r := runCLI(t, timingOutOllama(t), "run-local", "--item", "x", "--workdir", filepath.Join(dir, "ws"),
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true")
	m := reportRE.FindStringSubmatch(r.stdout)
	if r.code != 1 || m == nil || m[2] != "model unavailable: ReadTimeout after 4 attempts" || m[3] != "0" || m[4] != "1" {
		t.Fatalf("%+v", r)
	}
}

func TestMissionPlannerOutageIsACleanError(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	linkFakeToolbox(t)
	r := runCLI(t, timingOutOllama(t), "mission", "--task", "build it", "--workdir", filepath.Join(dir, "ws"),
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true")
	if r.code != 1 || !strings.HasPrefix(r.stderr, "error: model unavailable: ReadTimeout after 4 attempts") {
		t.Fatalf("%+v", r)
	}
}
