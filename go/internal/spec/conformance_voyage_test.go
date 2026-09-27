package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/memory"
)

// roundTrip adapts a function to http.RoundTripper.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestMemoryVoyage runs spec/memory/voyage.json: the Voyage embedder's batching, the request
// bodies it puts on the wire for the same inputs as Python (captured from a mocked API), how it
// reads or rejects a response, and its probe's HTTP-status messages.
func TestMemoryVoyage(t *testing.T) {
	var s struct {
		DefaultModel string `json:"default_model"`
		BatchTexts   int    `json:"batch_texts"`
		BatchChars   int    `json:"batch_chars"`
		Batches      []struct {
			Texts    []string   `json:"texts"`
			MaxTexts int        `json:"max_texts"`
			MaxChars int        `json:"max_chars"`
			Batches  [][]string `json:"batches"`
		} `json:"batches"`
		Requests []struct {
			Model     string          `json:"model"`
			InputType string          `json:"input_type"`
			Texts     []string        `json:"texts"`
			Bodies    json.RawMessage `json:"bodies"`
		} `json:"requests"`
		Responses []struct {
			Count    int             `json:"count"`
			Response json.RawMessage `json:"response"`
			Vectors  [][]float64     `json:"vectors"`
			Error    *string         `json:"error"`
		} `json:"responses"`
		Statuses []struct {
			Model   string `json:"model"`
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"statuses"`
	}
	Load(t, "memory/voyage.json", &s)
	if s.DefaultModel != memory.VoyageDefaultModel || s.BatchTexts != memory.VoyageBatchTexts || s.BatchChars != memory.VoyageBatchChars {
		t.Fatalf("constants differ from python: %q %d %d", s.DefaultModel, s.BatchTexts, s.BatchChars)
	}
	if len(s.Batches) == 0 || len(s.Requests) == 0 || len(s.Responses) == 0 || len(s.Statuses) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Batches {
		got := memory.VoyageBatches(c.Texts, c.MaxTexts, c.MaxChars)
		if !reflect.DeepEqual(got, c.Batches) {
			t.Errorf("VoyageBatches(%q, %d, %d) = %q, want %q", c.Texts, c.MaxTexts, c.MaxChars, got, c.Batches)
		}
	}
	public := func(context.Context, string, int) ([]string, error) { return []string{"93.184.216.34"}, nil }
	for _, c := range s.Requests {
		sent := []any{}
		api := roundTrip(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var generic any
			if err := json.Unmarshal(raw, &generic); err != nil {
				t.Fatalf("body is not JSON: %s", raw)
			}
			sent = append(sent, generic)
			var body struct {
				Input []string `json:"input"`
			}
			_ = json.Unmarshal(raw, &body)
			data := []map[string]any{}
			for i := range body.Input {
				data = append(data, map[string]any{"index": i, "embedding": []float64{1, 0}})
			}
			out, _ := json.Marshal(map[string]any{"data": data})
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{},
				Body: io.NopCloser(bytes.NewReader(out)), Request: r}, nil
		})
		e, err := memory.NewVoyageEmbedder(memory.VoyageOptions{APIKey: "pa-test", Model: c.Model, Resolver: public, Transport: api}, 2)
		if err != nil {
			t.Fatal(err)
		}
		if c.InputType == "query" {
			_, err = e.EmbedQuery(context.Background(), c.Texts)
		} else {
			_, err = e.Embed(context.Background(), c.Texts)
		}
		if err != nil {
			t.Fatalf("%s: %v", c.Model, err)
		}
		JSONEqual(t, "voyage request bodies "+c.Model, sent, c.Bodies)
	}
	for _, c := range s.Responses {
		got, err := memory.ParseVoyageResponse(c.Response, c.Count)
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error {
				t.Errorf("ParseVoyageResponse(%s) error = %v, want %q", c.Response, err, *c.Error)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.Vectors) {
			t.Errorf("ParseVoyageResponse(%s) = %v, %v; want %v", c.Response, got, err, c.Vectors)
		}
	}
	for _, c := range s.Statuses {
		if got := memory.VoyageStatusMessage(c.Model, c.Status); got != c.Message {
			t.Errorf("VoyageStatusMessage(%q, %d) = %q, want %q", c.Model, c.Status, got, c.Message)
		}
	}
}
