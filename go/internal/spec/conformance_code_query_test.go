package spec

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
)

// TestExecutionCodeQuery runs spec/execution/code_query.json: the code_query tool's spec, the
// ripwire command for each kind of question, which questions are refused (with Python's
// messages), and how answers are clipped.
func TestExecutionCodeQuery(t *testing.T) {
	var s struct {
		Kinds          map[string]string `json:"kinds"`
		MaxSymbolChars int               `json:"max_symbol_chars"`
		MaxFindChars   int               `json:"max_find_chars"`
		MaxAnswerChars int               `json:"max_answer_chars"`
		Spec           json.RawMessage   `json:"spec"`
		Questions      []struct {
			Kind   string   `json:"kind"`
			Target string   `json:"target"`
			Budget int      `json:"budget"`
			Argv   []string `json:"argv"`
			Error  *string  `json:"error"`
		} `json:"questions"`
		Clip []struct {
			Text    string `json:"text"`
			Clipped string `json:"clipped"`
		} `json:"clip"`
	}
	Load(t, "execution/code_query.json", &s)
	kinds := map[string]string{}
	for _, k := range tools.CodeQueryKinds {
		kinds[k.Kind] = k.Flag
	}
	if !reflect.DeepEqual(kinds, s.Kinds) || s.MaxSymbolChars != tools.MaxSymbolChars ||
		s.MaxFindChars != tools.MaxFindChars || s.MaxAnswerChars != tools.MaxAnswerChars {
		t.Fatalf("constants differ from python")
	}
	JSONEqual(t, "spec", tools.CodeQueryTool{}.Spec(), s.Spec)
	if len(s.Questions) == 0 || len(s.Clip) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Questions {
		argv, err := tools.CodeQueryArgv(c.Kind, c.Target, c.Budget)
		switch {
		case c.Error != nil && (err == nil || err.Error() != *c.Error):
			t.Errorf("CodeQueryArgv(%q, %.20q): err %v, want %q", c.Kind, c.Target, err, *c.Error)
		case c.Error == nil && (err != nil || !reflect.DeepEqual(argv, c.Argv)):
			t.Errorf("CodeQueryArgv(%q, %.20q) = %q, %v; want %q", c.Kind, c.Target, argv, err, c.Argv)
		}
	}
	for i, c := range s.Clip {
		if got := tools.ClipAnswer(c.Text); got != c.Clipped {
			t.Errorf("clip %d differs", i)
		}
	}
}
