package spec

import (
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

func TestHarnessFiles(t *testing.T) {
	var s struct {
		Cases []struct {
			Path    string `json:"path"`
			Harness bool   `json:"harness"`
		} `json:"cases"`
	}
	Load(t, "verify/harness_files.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Cases {
		if got := verify.IsHarnessFile(c.Path); got != c.Harness {
			t.Errorf("IsHarnessFile(%q) = %v, want %v", c.Path, got, c.Harness)
		}
	}
}
