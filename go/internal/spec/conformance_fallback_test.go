package spec

import (
	"math"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// TestFallbackModels runs spec/model/fallback_models.json: LHA_FALLBACK_MODELS entries parse to
// the same backend, model and price, or fail with python's exact ValueError message.
func TestFallbackModels(t *testing.T) {
	var s struct {
		Cases []struct {
			Entry   string  `json:"entry"`
			Error   *string `json:"error"`
			Backend string  `json:"backend"`
			Model   string  `json:"model"`
			Price   *struct {
				In  float64 `json:"input_per_mtok"`
				Out float64 `json:"output_per_mtok"`
			} `json:"price"`
		} `json:"cases"`
	}
	Load(t, "model/fallback_models.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no fallback cases")
	}
	for _, c := range s.Cases {
		got, err := model.ParseFallbackEntry(c.Entry)
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error {
				t.Errorf("%q: error %v, want %q", c.Entry, err, *c.Error)
			}
			continue
		}
		if err != nil || got.Backend != c.Backend || got.Model != c.Model || (got.Price == nil) != (c.Price == nil) {
			t.Errorf("%q: %+v %v", c.Entry, got, err)
			continue
		}
		if c.Price != nil && (got.Price.InputPerMTok != c.Price.In || got.Price.OutputPerMTok != c.Price.Out ||
			math.Signbit(got.Price.OutputPerMTok) != math.Signbit(c.Price.Out)) {
			t.Errorf("%q: price %+v", c.Entry, *got.Price)
		}
	}
}
