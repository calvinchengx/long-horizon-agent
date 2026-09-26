package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/state/vendor"
)

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVendorCommand(t *testing.T) {
	dir := cleanEnv(t)
	saved := vendorOptions
	t.Cleanup(func() { vendorOptions = saved })
	vendorOptions = vendor.Options{
		Resolver: func(context.Context, string, int) ([]string, error) { return []string{"93.184.216.34"}, nil },
		Client: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Status: "200 OK", Request: r,
				Header: http.Header{"Content-Type": {"text/html"}},
				Body:   io.NopCloser(strings.NewReader("<p>Items API</p>"))}, nil
		})},
	}
	// Options may follow the URLs (click's interspersed options).
	r := runCLI(t, nil, "vendor", "https://docs.example.com/items", "--into", "ref")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "https://docs.example.com/items -> ref/docs.example.com/items.html (16 bytes, sha256 ") ||
		!strings.HasSuffix(r.stdout, ")\nmanifest: ref/MANIFEST.json\n") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "ref", "docs.example.com", "items.html.txt")); err != nil {
		t.Fatal(err)
	}
	if r := runCLI(t, nil, "vendor"); r.code != 2 || !strings.Contains(r.stderr, "Missing argument 'URL...'") {
		t.Fatalf("no url: %+v", r)
	}
	if r := runCLI(t, nil, "vendor", "--bogus", "x"); r.code != 2 {
		t.Fatalf("unknown option: %+v", r)
	}
	if r := runCLI(t, nil, "vendor", "--help"); r.code != 0 {
		t.Fatalf("help: %+v", r)
	}
}

// The refusals that need no network print the same error and exit code as Python.
func TestE2EVendorRefusalsMatchPython(t *testing.T) {
	bin := lhaBinary(t)
	withPython := pythonAvailable(t)
	for _, args := range [][]string{
		{"vendor", "ftp://x.example/a"},
		{"vendor", "https://user:pw@docs.example.com/a"},
		{"vendor", "https://docs.example.com:8443/a", "--into", "refs"},
		{"vendor", "--into", "refs", "http://127.0.0.1/x"},
		{"vendor", "http://10.1.2.3/private", "ftp://x.example/a"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			env := processEnv()
			goRun := runProcess(t, t.TempDir(), env, bin, args...)
			if goRun.code != 2 || !strings.HasPrefix(goRun.stderr, "error: ") || goRun.stdout != "" {
				t.Fatalf("go: %+v", goRun)
			}
			if !withPython {
				return
			}
			pyRun := runPythonLHA(t, t.TempDir(), env, args...)
			if pyRun.code != goRun.code || pyRun.stderr != goRun.stderr || pyRun.stdout != goRun.stdout {
				t.Errorf("go %d %q %q\npy %d %q %q", goRun.code, goRun.stdout, goRun.stderr, pyRun.code, pyRun.stdout, pyRun.stderr)
			}
		})
	}
}
