package spec

import (
	"log/slog"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler)) // memory events are asserted, not read
	os.Exit(m.Run())
}
