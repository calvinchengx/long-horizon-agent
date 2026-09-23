package spec

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

func TestDecisionChain(t *testing.T) {
	var s struct {
		Genesis string `json:"genesis"`
		Chain   []struct {
			Record    any    `json:"record"`
			Canonical string `json:"canonical"`
			Prev      string `json:"prev"`
			Hash      string `json:"hash"`
		} `json:"chain"`
		Log struct {
			Lines []string `json:"lines"`
			Links []struct {
				Kind        string `json:"kind"`
				RunningHash string `json:"running_hash"`
			} `json:"links"`
			LastHash string `json:"last_hash"`
			Verify   []struct {
				Name    string      `json:"name"`
				Lines   []string    `json:"lines"`
				OK      bool        `json:"ok"`
				Checked json.Number `json:"checked"`
				Legacy  json.Number `json:"legacy"`
				Problem string      `json:"problem"`
			} `json:"verify"`
		} `json:"log"`
	}
	Load(t, "coordination/decision_chain.json", &s)
	if s.Genesis != state.GenesisHash || len(s.Chain) == 0 || len(s.Log.Lines) == 0 {
		t.Fatal("unexpected spec shape")
	}

	// Canonical JSON bytes and the hash chain.
	prev := s.Genesis
	for _, link := range s.Chain {
		canon, err := state.CanonicalJSON(link.Record)
		if err != nil {
			t.Fatal(err)
		}
		if canon != link.Canonical {
			t.Errorf("canonical:\n go:   %q\n spec: %q", canon, link.Canonical)
		}
		if link.Prev != prev {
			t.Errorf("prev %s, want %s", link.Prev, prev)
		}
		hash, err := state.ChainHash(prev, link.Record)
		if err != nil || hash != link.Hash {
			t.Errorf("hash %s (%v), want %s", hash, err, link.Hash)
		}
		prev = link.Hash
	}

	// The .lha/decisions.ndjson format: a legacy prefix folded into the chain, then envelopes.
	var data []byte
	for i, line := range s.Log.Lines {
		data = append(data, line+"\n"...)
		chain, err := state.ParseDecisionChain(data)
		if err != nil {
			t.Fatal(err)
		}
		link := s.Log.Links[i]
		if chain.LastHash != link.RunningHash {
			t.Errorf("line %d: running hash %s, want %s", i+1, chain.LastHash, link.RunningHash)
		}
		if strings.Contains(line, `"prev"`) != (link.Kind == "chained") {
			t.Errorf("line %d: kind %s", i+1, link.Kind)
		}
		if link.Kind == "chained" { // Go writes the exact bytes Python wrote
			prevHash := s.Genesis
			if i > 0 {
				prevHash = s.Log.Links[i-1].RunningHash
			}
			encoded, _, err := state.EncodeDecisionLink(prevHash, chain.Records[i])
			if err != nil || encoded != line {
				t.Errorf("line %d encoding:\n go:   %q\n spec: %q (%v)", i+1, encoded, line, err)
			}
		}
	}
	if chain, err := state.ParseDecisionChain(data); err != nil || chain.LastHash != s.Log.LastHash {
		t.Errorf("last hash %s (%v), want %s", chain.LastHash, err, s.Log.LastHash)
	}
	for _, c := range s.Log.Verify {
		var body bytes.Buffer
		for _, line := range c.Lines {
			body.WriteString(line + "\n")
		}
		got := state.VerifyDecisionChain(body.Bytes())
		if got.OK != c.OK || c.Checked.String() != strconv.Itoa(got.Checked) ||
			c.Legacy.String() != strconv.Itoa(got.Legacy) || got.Problem != c.Problem {
			t.Errorf("%s: got %+v, want ok=%v checked=%s legacy=%s problem=%q",
				c.Name, got, c.OK, c.Checked, c.Legacy, c.Problem)
		}
	}
}
