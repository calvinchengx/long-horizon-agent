package spec

import (
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

func TestRedact(t *testing.T) {
	var s struct {
		RedactText []struct {
			Text     string `json:"text"`
			Redacted string `json:"redacted"`
		} `json:"redact_text"`
		IsSecretKey []struct {
			Key    string `json:"key"`
			Secret bool   `json:"secret"`
		} `json:"is_secret_key"`
	}
	Load(t, "obs/redact.json", &s)
	if len(s.RedactText) == 0 || len(s.IsSecretKey) == 0 {
		t.Fatal("missing redact sections")
	}
	for _, c := range s.RedactText {
		if got := obs.RedactText(c.Text); got != c.Redacted {
			t.Errorf("RedactText(%q) = %q, want %q", c.Text, got, c.Redacted)
		}
	}
	for _, c := range s.IsSecretKey {
		if got := obs.IsSecretKey(c.Key); got != c.Secret {
			t.Errorf("IsSecretKey(%q) = %v, want %v", c.Key, got, c.Secret)
		}
	}
}
