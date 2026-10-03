package spec

import (
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// TestGold is python's test_system_one_gold: gold evaluation rows (lha eval), their parse and
// check errors, and each built-in judge's scorecards and report.
func TestGold(t *testing.T) {
	type card struct {
		Source        string   `json:"source"`
		Rows          int      `json:"rows"`
		Judged        int      `json:"judged"`
		Agree         int      `json:"agree"`
		TP            int      `json:"tp"`
		FP            int      `json:"fp"`
		FN            int      `json:"fn"`
		TN            int      `json:"tn"`
		Disagreements []string `json:"disagreements"`
	}
	var s struct {
		ParseErrors []struct {
			Name  string  `json:"name"`
			Text  string  `json:"text"`
			Error *string `json:"error"`
		} `json:"parse_errors"`
		Checks []struct {
			Name   string   `json:"name"`
			Text   string   `json:"text"`
			Rows   int      `json:"rows"`
			Errors []string `json:"errors"`
			JSONL  string   `json:"jsonl"`
		} `json:"checks"`
		Scored struct {
			Text   string `json:"text"`
			Judges []struct {
				Judge  string `json:"judge"`
				Cards  []card `json:"cards"`
				Report string `json:"report"`
			} `json:"judges"`
		} `json:"scored"`
	}
	Load(t, "systemone/gold.json", &s)
	for _, c := range s.ParseErrors {
		_, err := systemone.ParseGold(c.Text, "gold.jsonl")
		switch {
		case c.Error == nil && err != nil:
			t.Errorf("%s: unexpected error %v", c.Name, err)
		case c.Error != nil && (err == nil || err.Error() != *c.Error):
			t.Errorf("%s: error %v, want %q", c.Name, err, *c.Error)
		}
	}
	for _, c := range s.Checks {
		rows, err := systemone.ParseGold(c.Text, "gold.jsonl")
		if err != nil || len(rows) != c.Rows {
			t.Fatalf("%s: %d rows, %v", c.Name, len(rows), err)
		}
		if got := systemone.CheckGold(rows); !reflect.DeepEqual(got, c.Errors) {
			t.Errorf("%s: errors %q, want %q", c.Name, got, c.Errors)
		}
		if got := systemone.GoldToJSONL(rows); got != c.JSONL {
			t.Errorf("%s: jsonl\n%s\nwant\n%s", c.Name, got, c.JSONL)
		}
	}
	rows, err := systemone.ParseGold(s.Scored.Text, "gold.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range s.Scored.Judges {
		judge, err := systemone.JudgeNamed(j.Judge)
		if err != nil {
			t.Fatal(err)
		}
		cards := systemone.ScoreGold(rows, judge)
		got := make([]card, len(cards))
		for i, c := range cards {
			d := c.Disagreements
			if d == nil {
				d = []string{}
			}
			got[i] = card{c.Source, c.Rows, c.Judged, c.Agree, c.TP, c.FP, c.FN, c.TN, d}
		}
		if !reflect.DeepEqual(got, j.Cards) {
			t.Errorf("%s: cards %+v, want %+v", j.Judge, got, j.Cards)
		}
		if report := systemone.RenderScorecards(cards, j.Judge); report != j.Report {
			t.Errorf("%s: report\n%s\nwant\n%s", j.Judge, report, j.Report)
		}
	}
}
