package agent

import (
	"log/slog"
	"os"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/model/claudecodetest"
)

func TestMain(m *testing.M) {
	claudecodetest.Main()                          // this binary doubles as the fake claude (claude_code engine tests)
	slog.SetDefault(slog.New(slog.DiscardHandler)) // trace events are asserted, not read
	// Runs persist to the mission store: never the developer's per-user one.
	dir, err := os.MkdirTemp("", "lha-agent-store-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_DATA_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
