package agent

import (
	"log/slog"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler)) // trace events are asserted, not read
	os.Exit(m.Run())
}
