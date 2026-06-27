package spec

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

func TestClassifyCommand(t *testing.T) {
	var s struct {
		Cases []struct {
			Argv   []string `json:"argv"`
			Reason *string  `json:"reason"`
		} `json:"cases"`
	}
	Load(t, "safety/classify_command.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Cases {
		reason, gated := safety.ClassifyCommand(c.Argv)
		switch {
		case c.Reason == nil && gated:
			t.Errorf("ClassifyCommand(%q) = %q, want allowed", c.Argv, reason)
		case c.Reason != nil && (!gated || reason != *c.Reason):
			t.Errorf("ClassifyCommand(%q) = %q (gated=%v), want %q", c.Argv, reason, gated, *c.Reason)
		}
	}
}

func TestEgress(t *testing.T) {
	var s struct {
		NormalizeHost []struct {
			Host       string `json:"host"`
			Normalized string `json:"normalized"`
		} `json:"normalize_host"`
		ParseURL []struct {
			URL    string      `json:"url"`
			OK     bool        `json:"ok"`
			Scheme string      `json:"scheme"`
			Host   string      `json:"host"`
			Port   json.Number `json:"port"`
		} `json:"parse_url"`
		IsPublicAddress []struct {
			Address string `json:"address"`
			Public  bool   `json:"public"`
		} `json:"is_public_address"`
		Permits []struct {
			AllowHosts []string `json:"allow_hosts"`
			URL        string   `json:"url"`
			Permitted  bool     `json:"permitted"`
		} `json:"permits"`
	}
	Load(t, "safety/egress.json", &s)
	if len(s.NormalizeHost) == 0 || len(s.ParseURL) == 0 || len(s.IsPublicAddress) == 0 || len(s.Permits) == 0 {
		t.Fatal("missing egress sections")
	}
	for _, c := range s.NormalizeHost {
		if got := safety.NormalizeHost(c.Host); got != c.Normalized {
			t.Errorf("NormalizeHost(%q) = %q, want %q", c.Host, got, c.Normalized)
		}
	}
	for _, c := range s.ParseURL {
		got, err := safety.ParseURL(c.URL)
		if !c.OK {
			if err == nil {
				t.Errorf("ParseURL(%q) = %+v, want an error", c.URL, got)
			}
			continue
		}
		port, _ := c.Port.Int64()
		if err != nil {
			t.Errorf("ParseURL(%q): %v", c.URL, err)
		} else if got.Scheme != c.Scheme || got.Host != c.Host || int64(got.Port) != port {
			t.Errorf("ParseURL(%q) = %+v, want %s %s %d", c.URL, got, c.Scheme, c.Host, port)
		}
	}
	for _, c := range s.IsPublicAddress {
		if got := safety.IsPublicAddress(c.Address); got != c.Public {
			t.Errorf("IsPublicAddress(%q) = %v, want %v", c.Address, got, c.Public)
		}
	}
	for _, c := range s.Permits {
		policy := safety.NewEgressPolicy(c.AllowHosts...)
		if got := policy.Permits(c.URL); got != c.Permitted {
			t.Errorf("Permits(%v, %q) = %v, want %v", c.AllowHosts, c.URL, got, c.Permitted)
		}
	}
	// The resolved-address check agrees with IsPublicAddress on IP literals.
	for _, c := range s.IsPublicAddress {
		_, err := safety.CheckResolvedAddresses(context.Background(), c.Address, 443,
			func(context.Context, string, int) ([]string, error) { return []string{"93.184.216.34"}, nil })
		if c.Public && err != nil {
			t.Errorf("CheckResolvedAddresses(%q): %v", c.Address, err)
		}
	}
}
