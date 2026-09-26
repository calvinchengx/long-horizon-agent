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
	os.Exit(m.Run())
}
