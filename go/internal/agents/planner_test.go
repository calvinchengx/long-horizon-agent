package agents

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// Ported from python/tests/unit/test_planner.py, plus ownership and replanner cases.

func ids(items []contracts.ChecklistItem) []string {
	out := []string{}
	for _, i := range items {
		out = append(out, i.ID)
	}
	return out
}

func deps(items []contracts.ChecklistItem) [][]string {
	out := [][]string{}
	for _, i := range items {
		out = append(out, i.DependsOn)
	}
	return out
}

func TestParseChecklistFromJSONArray(t *testing.T) {
	items := ParseChecklist(`[{"description": "add model", "depends_on": []}, {"description": "add tests", "depends_on": ["01"]}]`)
	if !reflect.DeepEqual(ids(items), []string{"01", "02"}) || !reflect.DeepEqual(items[1].DependsOn, []string{"01"}) {
		t.Fatalf("%+v", items)
	}
}

func TestParseChecklistToleratesProseWrapping(t *testing.T) {
	items := ParseChecklist("Sure! Here is the plan:\n[{\"description\": \"do x\"}]\nHope that helps.")
	if len(items) != 1 || items[0].Description != "do x" {
		t.Fatalf("%+v", items)
	}
}

func TestParseChecklistEmptyOnGarbage(t *testing.T) {
	if items := ParseChecklist("no json here"); len(items) != 0 {
		t.Fatalf("%+v", items)
	}
}

func TestPlannerUsesModelOutput(t *testing.T) {
	m := model.NewStub([]contracts.TurnResult{{Text: `[{"description": "step one"}, {"description": "step two"}]`}})
	cl, err := NewPlanner(m).Plan(context.Background(), "T", "D", "")
	if err != nil || len(cl.Items) != 2 || cl.Items[0].Description != "step one" || cl.Items[1].Description != "step two" {
		t.Fatalf("%+v %v", cl, err)
	}
}

func TestPlannerFallsBackToSingleItem(t *testing.T) {
	m := model.NewStub([]contracts.TurnResult{{Text: "I cannot plan this."}})
	cl, err := NewPlanner(m).Plan(context.Background(), "T", "build the thing", "")
	if err != nil || len(cl.Items) != 1 || cl.Items[0].Description != "build the thing" || cl.Items[0].ID != "01" {
		t.Fatalf("%+v %v", cl, err)
	}
}

func TestParseChecklistNormalizesDependencyIDs(t *testing.T) {
	items := ParseChecklist(`[{"description": "a"}, {"description": "b", "depends_on": ["1"]},` +
		` {"description": "c", "depends_on": [1, "step 2", "#2"]}]`)
	if !reflect.DeepEqual(deps(items), [][]string{{}, {"01"}, {"01", "02"}}) {
		t.Fatalf("%v", deps(items))
	}
	for _, i := range items {
		if i.Notes != "" {
			t.Fatalf("notes: %q", i.Notes)
		}
	}
}

func TestParseChecklistDropsForwardSelfAndUnknownDeps(t *testing.T) {
	items := ParseChecklist(`[{"description": "a", "depends_on": ["2"]}, {"description": "b", "depends_on": ["2"]},` +
		` {"description": "c", "depends_on": ["99", "setup"]}]`)
	if !reflect.DeepEqual(deps(items), [][]string{{}, {}, {}}) {
		t.Fatalf("%v", deps(items))
	}
	if items[2].Notes != "planner dropped invalid depends_on: ['99', 'setup']" {
		t.Fatalf("notes: %q", items[2].Notes)
	}
	cl := contracts.Checklist{Items: items}
	if errs := cl.DependencyErrors(); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
}

func TestParseChecklistMapsIndicesAcrossSkippedEntries(t *testing.T) {
	items := ParseChecklist(`[{"description": "a"}, {"description": ""}, {"description": "c", "depends_on": ["1", "2"]}]`)
	if !reflect.DeepEqual(ids(items), []string{"01", "02"}) || !reflect.DeepEqual(items[1].DependsOn, []string{"01"}) {
		t.Fatalf("%+v", items)
	}
}

func TestParseChecklistReadsHarnessEditOptIn(t *testing.T) {
	if !ParseChecklist(`[{"description": "fix the flaky test", "allow_harness_edits": true}]`)[0].AllowHarnessEdits {
		t.Fatal("opt-in lost")
	}
}

func TestParsePlanPythonValueRendering(t *testing.T) {
	// Non-dict entries are str()'d; "step" is a fallback key; odd deps are dropped as str(raw).
	items, _ := ParsePlan(`["plain step", {"step": "via step", "depends_on": [1.0, true, null, "", 7, 1.5]}, 42]`)
	if !reflect.DeepEqual(ids(items), []string{"01", "02", "03"}) || items[0].Description != "plain step" ||
		items[1].Description != "via step" || items[2].Description != "42" {
		t.Fatalf("%+v", items)
	}
	if !reflect.DeepEqual(items[1].DependsOn, []string{"01"}) || items[1].Notes != "planner dropped invalid depends_on: ['True', '7', '1.5']" {
		t.Fatalf("%+v", items[1])
	}
}

func TestPlanWidthGrowsPastNinetyNine(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 100; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"s"`)
	}
	b.WriteString("]")
	items := ParseChecklist(b.String())
	if items[0].ID != "001" || items[99].ID != "100" {
		t.Fatalf("%s %s", items[0].ID, items[99].ID)
	}
}

func TestAssignOwnership(t *testing.T) {
	items, files := ParsePlan(`[
		{"description": "a", "files": ["src/a.py", "src/A2.py"]},
		{"description": "b", "files": ["src/b.py", "pyproject.toml"]},
		{"description": "c", "files": ["SRC/a.py"]},
		{"description": "d", "files": ["../escape.py"]},
		{"description": "e", "files": [".lha/checklist.json"]},
		{"description": "f"}
	]`)
	own := AssignOwnership(items, files)
	want := map[string]string{"src/a.py": "implementer-01", "src/a2.py": "implementer-01"}
	if !reflect.DeepEqual(own.Owners, want) {
		t.Fatalf("owners %v", own.Owners)
	}
	notes := []string{}
	for _, i := range items {
		notes = append(notes, i.Notes)
	}
	wantNotes := []string{"", "serial: touches shared files ['pyproject.toml']", "serial after 01: overlapping files",
		"serial: invalid file paths ['../escape.py']", "serial: invalid file paths ['.lha/checklist.json']", ""}
	if !reflect.DeepEqual(notes, wantNotes) {
		t.Fatalf("notes %q", notes)
	}
	if !reflect.DeepEqual(items[2].DependsOn, []string{"01"}) {
		t.Fatalf("overlap dep %v", items[2].DependsOn)
	}
}

func TestReplannerSplitsAndBounds(t *testing.T) {
	it := contracts.NewChecklistItem("01", "coarse")
	it.Witnesses = []string{"go:TestX"}
	it.LastFailure = "boom"
	it.ConsecutiveFailures = 3
	msgs := ReplannerMessages("Mission: M", it)
	if !strings.Contains(msgs[1].Content, "Blocked item [01]: coarse\nIts acceptance checks (they will gate the LAST step): go:TestX\n\nIt failed 3 times in a row.") {
		t.Fatalf("%s", msgs[1].Content)
	}
	m := model.NewStub([]contracts.TurnResult{{Text: `[{"description":"1"},{"description":"2"},{"description":"3"},{"description":"4"},{"description":"5"},{"description":"6"},{"description":"7"}]`}})
	drafts, err := NewReplanner(m).Split(context.Background(), "M", it)
	if err != nil || len(drafts) != MaxChildren || drafts[0].ID != "draft-1" {
		t.Fatalf("%+v %v", drafts, err)
	}
	one := model.NewStub([]contracts.TurnResult{{Text: `[{"description":"only"}]`}})
	if drafts, _ := NewReplanner(one).Split(context.Background(), "M", it); drafts != nil {
		t.Fatalf("a single step is no split: %+v", drafts)
	}
}

func TestNormalizePathAndIsShared(t *testing.T) {
	cases := map[string]string{"a//b/./c": "a/b/c", `a\b`: "a/b", "a/../b": "b", ".env": ".env"}
	for in, want := range cases {
		if got, err := NormalizePath(in); err != nil || got != want {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"/abs", "..", "", ".", "a/../.."} {
		if _, err := NormalizePath(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
