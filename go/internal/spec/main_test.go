package spec

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler)) // memory events are asserted, not read
	code := m.Run()
	// Contract coverage: after a full run, every spec file and every top-level group must have
	// been checked. Skipped when -run / -skip select a subset.
	if code == 0 && flagValue("test.run") == "" && flagValue("test.skip") == "" {
		gaps, err := Uncovered()
		if err != nil || len(gaps) > 0 {
			fmt.Fprintf(os.Stderr, "spec coverage: %v\n  %s\n", err, strings.Join(gaps, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

func flagValue(name string) string {
	if f := flag.Lookup(name); f != nil {
		return f.Value.String()
	}
	return ""
}
