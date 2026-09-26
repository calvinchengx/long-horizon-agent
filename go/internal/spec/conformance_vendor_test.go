package spec

import (
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/state/vendor"
)

// TestVendorPaths runs spec/state/vendor_paths.json: where `lha vendor` stores a fetched URL.
func TestVendorPaths(t *testing.T) {
	var s struct {
		Cases []struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
			Path        string `json:"path"`
		} `json:"cases"`
	}
	Load(t, "state/vendor_paths.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no vendor path cases")
	}
	for _, c := range s.Cases {
		got, err := vendor.TargetPath(c.URL, c.ContentType)
		if err != nil || got != c.Path {
			t.Errorf("TargetPath(%q, %q) = %q, %v; want %q", c.URL, c.ContentType, got, err, c.Path)
		}
	}
}
