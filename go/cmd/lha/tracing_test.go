package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestTracingComponent(t *testing.T) {
	for args, want := range map[string]string{"worker": "worker", "version": "cli", "": "cli"} {
		var argv []string
		if args != "" {
			argv = []string{args}
		}
		if got := tracingComponent(argv); got != want {
			t.Fatalf("%q: %s", args, got)
		}
	}
}

// With an OTLP endpoint configured, a run-local exports its spans to the collector at process
// end (python: the CLI callback's configure_tracing + the SDK's exit-time flush).
func TestCLIExportsSpansToTheConfiguredCollector(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
	}))
	defer srv.Close()
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub", "LHA_MAX_TURNS_PER_CYCLE=2", "LHA_OTEL_EXPORTER_OTLP_ENDPOINT="+srv.URL)
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	linkFakeToolbox(t)
	args := []string{"run-local", "--item", "say hello", "--workdir", filepath.Join(dir, "ws"),
		"--sandbox", "local", "--unsafe-local", "--no-default-checks", "--check", "true"}
	shutdown := startTracing(args)
	r := runCLI(t, nil, args...)
	shutdown()
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || paths[0] != "/v1/traces" {
		t.Fatalf("collector saw %v", paths)
	}
}

func TestNoEndpointNoTracing(t *testing.T) {
	cleanEnv(t)
	startTracing([]string{"version"})() // a no-op shutdown
}
