package spec

import (
	"encoding/json"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
)

// TestExecutionEditFile runs spec/execution/edit_file.json: the edit_file tool's spec and which
// edits apply or are refused (with Python's messages).
func TestExecutionEditFile(t *testing.T) {
	var s struct {
		Spec  json.RawMessage `json:"spec"`
		Edits []struct {
			Content string  `json:"content"`
			OldText string  `json:"old_text"`
			NewText string  `json:"new_text"`
			Result  *string `json:"result"`
			Error   *string `json:"error"`
		} `json:"edits"`
	}
	Load(t, "execution/edit_file.json", &s)
	JSONEqual(t, "spec", tools.EditFileTool{}.Spec(), s.Spec)
	if len(s.Edits) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Edits {
		got, err := tools.ApplyEdit(c.Content, c.OldText, c.NewText)
		switch {
		case c.Error != nil && (err == nil || err.Error() != *c.Error):
			t.Errorf("ApplyEdit(%q, %q): err %v, want %q", c.Content, c.OldText, err, *c.Error)
		case c.Error == nil && (err != nil || got != *c.Result):
			t.Errorf("ApplyEdit(%q, %q) = %q, %v; want %q", c.Content, c.OldText, got, err, *c.Result)
		}
	}
}
