package checklistimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Ported from python/tests/unit/test_checklist_import.py.

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSON(t *testing.T, dir string, data any, name string) string {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return write(t, dir, name, string(raw))
}

func mustLoad(t *testing.T, path string, includeDone bool) ImportedChecklist {
	t.Helper()
	imported, err := Load(path, includeDone)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	return imported
}

func importError(t *testing.T, err error) string {
	t.Helper()
	var ie *ImportError
	if !errors.As(err, &ie) {
		t.Fatalf("want *ImportError, got %T: %v", err, err)
	}
	return ie.Message
}

func ids(items []contracts.ChecklistItem) []string {
	out := []string{}
	for _, i := range items {
		out = append(out, i.ID)
	}
	return out
}

// --- JSON -----------------------------------------------------------------------------------

func TestJSONMissionFile(t *testing.T) {
	path := writeJSON(t, t.TempDir(), map[string]any{
		"title":       " Livy ",
		"description": "Real Spark behind Livy.",
		"references":  []string{"docs/livy.md"},
		"items": []any{
			map[string]any{"description": "sessions API", "witnesses": []string{"go:TestLivySessions"}},
			map[string]any{"description": "batches API", "depends_on": []string{"01"}, "allow_harness_edits": true},
			map[string]any{"id": "e2e", "description": "real spark", "depends_on": []string{"02"}, "witnesses": []string{"ci:sail"}},
		},
	}, "plan.json")
	imported := mustLoad(t, path, false)
	if imported.Title != "Livy" || imported.Description != "Real Spark behind Livy." {
		t.Fatalf("title/description = %q / %q", imported.Title, imported.Description)
	}
	if !reflect.DeepEqual(imported.References, []string{"docs/livy.md"}) {
		t.Fatalf("references = %v", imported.References)
	}
	items := imported.Checklist.Items
	if got := ids(items); !reflect.DeepEqual(got, []string{"01", "02", "e2e"}) {
		t.Fatalf("ids = %v", got)
	}
	if !reflect.DeepEqual(items[0].Witnesses, []string{"go:TestLivySessions"}) {
		t.Fatalf("witnesses = %v", items[0].Witnesses)
	}
	if !items[1].AllowHarnessEdits || !reflect.DeepEqual(items[1].DependsOn, []string{"01"}) {
		t.Fatalf("item 02 = %+v", items[1])
	}
	for _, i := range items {
		if i.Status != contracts.StatusTodo {
			t.Fatalf("status = %q", i.Status)
		}
	}
}

func TestJSONChecklistDumpRoundTrips(t *testing.T) {
	a := contracts.NewChecklistItem("a", "one")
	a.Status, a.VerifiedBy = contracts.StatusDone, []string{"pytest"}
	b := contracts.NewChecklistItem("b", "two", "a")
	b.Witnesses = []string{"cmd:true"}
	checklist := contracts.Checklist{Items: []contracts.ChecklistItem{a, b}, SchemaVersion: 1}
	raw, err := json.Marshal(checklist)
	if err != nil {
		t.Fatal(err)
	}
	imported := mustLoad(t, write(t, t.TempDir(), "checklist.json", string(raw)), false)
	if !reflect.DeepEqual(imported.Checklist, checklist) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", imported.Checklist, checklist)
	}
	if imported.Title != "" || len(imported.References) != 0 || imported.References == nil {
		t.Fatalf("title %q references %#v", imported.Title, imported.References)
	}
}

func TestJSONTopLevelListAndIDWidth(t *testing.T) {
	data := []any{}
	for n := range 100 {
		data = append(data, map[string]any{"description": fmt.Sprintf("step %d", n)})
	}
	got := ids(mustLoad(t, writeJSON(t, t.TempDir(), data, "plan.json"), false).Checklist.Items)
	if got[0] != "001" || got[len(got)-1] != "100" {
		t.Fatalf("ids %s .. %s", got[0], got[len(got)-1])
	}
}

func TestJSONErrors(t *testing.T) {
	cases := []struct {
		data     any
		fragment string
	}{
		{map[string]any{"title": "x"}, `an object with an "items" list`},
		{"just a string", `an object with an "items" list`},
		{map[string]any{"items": []any{}}, "checklist has no items"},
		{map[string]any{"items": []any{"bare string"}}, "items[0] must be an object"},
		{map[string]any{"items": []any{map[string]any{"description": "x", "witness": []string{"go:TestX"}}}},
			"unknown field(s): ['witness']"},
		{map[string]any{"items": []any{map[string]any{"description": "x", "status": "finished"}}}, "items[0] is invalid"},
		{map[string]any{"items": []any{map[string]any{"description": "  "}}}, "non-empty id and description"},
		{map[string]any{"title": 3, "items": []any{map[string]any{"description": "x"}}}, "must be strings"},
		{map[string]any{"references": "docs", "items": []any{map[string]any{"description": "x"}}}, `"references" must be a list`},
		{map[string]any{"items": []any{map[string]any{"id": "01", "description": "x"}, map[string]any{"description": "y", "id": "01"}}},
			"duplicate item id '01'"},
		{map[string]any{"items": []any{map[string]any{"description": "x", "depends_on": []string{"zz"}}}}, "unknown item 'zz'"},
		{map[string]any{"items": []any{
			map[string]any{"id": "a", "description": "x", "depends_on": []string{"b"}},
			map[string]any{"id": "b", "description": "y", "depends_on": []string{"a"}},
		}}, "dependency cycle"},
		{map[string]any{"items": []any{map[string]any{"description": "x", "witnesses": []string{"sdk:TestX"}}}}, "item '01': witness"},
	}
	for _, tc := range cases {
		t.Run(tc.fragment, func(t *testing.T) {
			path := writeJSON(t, t.TempDir(), tc.data, "plan.json")
			_, err := Load(path, false)
			msg := importError(t, err)
			if !strings.Contains(msg, tc.fragment) || !strings.Contains(msg, path) {
				t.Fatalf("message %q lacks %q or the path", msg, tc.fragment)
			}
		})
	}
}

func TestInvalidJSON(t *testing.T) {
	_, err := Load(write(t, t.TempDir(), "bad.json", "{nope"), false)
	if msg := importError(t, err); !strings.Contains(msg, "invalid JSON") {
		t.Fatal(msg)
	}
}

func TestUnreadableAndUnsupportedFiles(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(filepath.Join(dir, "missing.json"), false)
	if msg := importError(t, err); !strings.Contains(msg, "cannot read checklist") {
		t.Fatal(msg)
	}
	_, err = Load(write(t, dir, "plan.yaml", "items: []"), false)
	if msg := importError(t, err); !strings.Contains(msg, "unsupported checklist format '.yaml'") {
		t.Fatal(msg)
	}
}

// --- Markdown -------------------------------------------------------------------------------

const roadmap = "# 13 — Roadmap\n" +
	"\n" +
	"Scope chosen: **full** — control plane through the OneLake data plane,\n" +
	"composed via docker-compose.\n" +
	"\n" +
	"Each phase is independently useful and CI-verified.\n" +
	"\n" +
	"## P0 — the spine (token acceptance + workspaces)\n" +
	"\n" +
	"The minimum that lets someone test automation.\n" +
	"\n" +
	"- [x] Token acceptance: validate Bearer against entra-emulator JWKS/issuer;\n" +
	"      audience set. (`--entra-issuer`)\n" +
	"- [x] Store + migrations (`workspace`, `item`).\n" +
	"\n" +
	"## P1 — CI/CD (the primary draw)\n" +
	"\n" +
	"- [x] Item **definitions**: `getDefinition` (200) / `updateDefinition` (202 LRO).\n" +
	"- [ ] **Deployment pipelines** — the third item in this phase's own header,\n" +
	"      shipped end to end (witnesses: go:TestDeployStage, ci:fabric-cli).\n" +
	"      - D0 pipeline/stage model — a nested bullet, not a checkbox\n" +
	"        with its own continuation\n" +
	"      Designed in 23-deployment-pipelines.md.\n" +
	"- [ ] Jobs: trigger + cancel (witness: go:TestJobLifecycle@./internal/jobs/...)\n" +
	"\n" +
	"## P2 — the identity handshake\n" +
	"\n" +
	"Its dependency has already shipped.\n" +
	"\n" +
	"- [ ] Workspace-identity lifecycle (witness: go:TestProvisionIdentity)\n" +
	"  - [ ] nested checkbox is its own item\n" +
	"- [ ] Trusted workspace access\n" +
	"\n" +
	"```\n" +
	"- [ ] not an item: inside a code fence\n" +
	"```\n" +
	"\n" +
	"### Notes\n" +
	"\n" +
	"- a plain bullet is ignored\n" +
	"\n" +
	"## Sequencing note\n" +
	"\n" +
	"Prose only, no items.\n" +
	"\n" +
	"## R — Real compute\n" +
	"\n" +
	"- [ ] Livy on real Spark (witness: pytest:tests/e2e/test_livy.py::test_real_spark)\n"

func TestFabricStyleRoadmap(t *testing.T) {
	imported := mustLoad(t, write(t, t.TempDir(), "13-roadmap.md", roadmap), false)
	if imported.Title != "13 — Roadmap" {
		t.Fatalf("title %q", imported.Title)
	}
	wantDesc := "Scope chosen: **full** — control plane through the OneLake data plane, " +
		"composed via docker-compose.\n\nEach phase is independently useful and CI-verified."
	if imported.Description != wantDesc {
		t.Fatalf("description %q", imported.Description)
	}
	if imported.References == nil || len(imported.References) != 0 {
		t.Fatalf("references %#v", imported.References)
	}
	all := imported.Checklist.Items
	if got := ids(all); !reflect.DeepEqual(got, []string{"01", "02", "03", "04", "05", "06"}) {
		t.Fatalf("ids %v", got)
	}
	items := map[string]contracts.ChecklistItem{}
	for _, i := range all {
		items[i.ID] = i
	}
	pipelines := items["01"]
	if want := "**Deployment pipelines** — the third item in this phase's own header, " +
		"shipped end to end. Designed in 23-deployment-pipelines.md."; pipelines.Description != want {
		t.Fatalf("01 description %q", pipelines.Description)
	}
	eq := func(label string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s = %#v, want %#v", label, got, want)
		}
	}
	eq("01 witnesses", pipelines.Witnesses, []string{"go:TestDeployStage", "ci:fabric-cli"})
	eq("02 witnesses", items["02"].Witnesses, []string{"go:TestJobLifecycle@./internal/jobs/..."})
	if items["02"].Description != "Jobs: trigger + cancel" {
		t.Fatalf("02 description %q", items["02"].Description)
	}
	// P0 has only done items, so the first open phase has no dependencies.
	eq("01 deps", pipelines.DependsOn, []string{})
	eq("02 deps", items["02"].DependsOn, []string{})
	// P2 depends on every item of P1; items inside P2 are independent.
	for _, id := range []string{"03", "04", "05"} {
		eq(id+" deps", items[id].DependsOn, []string{"01", "02"})
	}
	if items["04"].Description != "nested checkbox is its own item" {
		t.Fatalf("04 description %q", items["04"].Description)
	}
	// R skips the item-less "Sequencing note" phase and depends on all of P2.
	eq("06 deps", items["06"].DependsOn, []string{"03", "04", "05"})
	eq("06 witnesses", items["06"].Witnesses, []string{"pytest:tests/e2e/test_livy.py::test_real_spark"})
	for _, i := range all {
		if strings.Contains(i.Description, "not an item") {
			t.Fatalf("fenced line imported: %q", i.Description)
		}
	}
}

func TestIncludeDoneImportsCheckedItemsAsTodo(t *testing.T) {
	items := mustLoad(t, write(t, t.TempDir(), "roadmap.md", roadmap), true).Checklist.Items
	if len(items) != 9 {
		t.Fatalf("%d items", len(items))
	}
	if want := "Token acceptance: validate Bearer against entra-emulator JWKS/issuer; " +
		"audience set. (`--entra-issuer`)"; items[0].Description != want {
		t.Fatalf("description %q", items[0].Description)
	}
	for _, i := range items {
		if i.Status != contracts.StatusTodo {
			t.Fatalf("status %q", i.Status)
		}
	}
	p0 := []string{"01", "02"}
	if !reflect.DeepEqual(items[2].DependsOn, p0) || !reflect.DeepEqual(items[3].DependsOn, p0) {
		t.Fatalf("deps %v %v", items[2].DependsOn, items[3].DependsOn)
	}
}

func TestMarkdownWithoutTitleUsesFileStem(t *testing.T) {
	imported := mustLoad(t, write(t, t.TempDir(), "plan.md", "- [ ] one\n- [ ] two\n"), false)
	if imported.Title != "plan" || imported.Description != "" {
		t.Fatalf("title %q description %q", imported.Title, imported.Description)
	}
	for _, i := range imported.Checklist.Items {
		if len(i.DependsOn) != 0 {
			t.Fatalf("deps %v", i.DependsOn)
		}
	}
}

func TestMarkdownItemsBeforeFirstPhaseAreAPhase(t *testing.T) {
	items := mustLoad(t, write(t, t.TempDir(), "p.md", "# T\n\n- [ ] setup\n\n## Next\n\n- [ ] build\n"), false).Checklist.Items
	if len(items[0].DependsOn) != 0 || !reflect.DeepEqual(items[1].DependsOn, []string{"01"}) {
		t.Fatalf("deps %v %v", items[0].DependsOn, items[1].DependsOn)
	}
}

func TestMarkdownMultipleWitnessGroupsAndCase(t *testing.T) {
	text := "- [ ] thing (Witness: go:TestA) and (WITNESSES: cmd:make x, trusted:e2e,)\n"
	item := mustLoad(t, write(t, t.TempDir(), "p.md", text), false).Checklist.Items[0]
	if !reflect.DeepEqual(item.Witnesses, []string{"go:TestA", "cmd:make x", "trusted:e2e"}) {
		t.Fatalf("witnesses %v", item.Witnesses)
	}
	if item.Description != "thing and" {
		t.Fatalf("description %q", item.Description)
	}
}

func TestMarkdownManyItemsGetWideIDs(t *testing.T) {
	var b strings.Builder
	for n := range 120 {
		fmt.Fprintf(&b, "- [ ] item %d\n", n)
	}
	items := mustLoad(t, write(t, t.TempDir(), "p.md", b.String()), false).Checklist.Items
	if items[0].ID != "001" || items[len(items)-1].ID != "120" {
		t.Fatalf("ids %s .. %s", items[0].ID, items[len(items)-1].ID)
	}
}

func TestMarkdownErrorsCarryLineNumbers(t *testing.T) {
	path := write(t, t.TempDir(), "p.md", "# T\n\n- [ ] fine\n- [ ] bad (witness: go:not-a-test)\n")
	_, err := Load(path, false)
	msg := importError(t, err)
	if want := path + ":4: item '02': witness 'go:not-a-test'"; !strings.HasPrefix(msg, want) {
		t.Fatalf("message %q, want prefix %q", msg, want)
	}
}

func TestMarkdownItemWithOnlyAWitness(t *testing.T) {
	_, err := Load(write(t, t.TempDir(), "p.md", "## P\n\n- [ ] (witness: go:TestX)\n"), false)
	if msg := importError(t, err); !strings.Contains(msg, "p.md:3: checklist item has no description") {
		t.Fatal(msg)
	}
}

func TestMarkdownWithoutOpenItems(t *testing.T) {
	_, err := Load(write(t, t.TempDir(), "p.md", "# T\n\n- [x] already done\n- plain bullet\n"), false)
	if msg := importError(t, err); !strings.Contains(msg, "checklist has no items") {
		t.Fatal(msg)
	}
}

// --- witnesses.json manifests ---------------------------------------------------------------

func TestWitnessesFromManifest(t *testing.T) {
	path := writeJSON(t, t.TempDir(), map[string]any{
		"row-level-security": map[string]any{
			"section":   "Data Warehouse",
			"claim":     "RLS",
			"witnesses": []string{"ci:warehouse-tds", "go:TestRelayEnforcesRowLevelSecurity"},
		},
		"blob-surface": map[string]any{"witnesses": []string{"ci:adls-sdk"}},
		"_gated":       map[string]any{"go:TestRelayEnforcesRowLevelSecurity": "needs SQL Server"},
	}, "witnesses.json")
	got, err := WitnessesFromManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"row-level-security": {"ci:warehouse-tds", "go:TestRelayEnforcesRowLevelSecurity"},
		"blob-surface":       {"ci:adls-sdk"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestWitnessManifestErrors(t *testing.T) {
	cases := []struct{ content, fragment string }{
		{"{nope", "cannot read witness manifest"},
		{"[1, 2]", "must be a JSON object"},
		{`{"a": {"claim": "x"}}`, "'a'.witnesses must be a list of strings"},
		{`{"a": ["go:TestX"]}`, "'a'.witnesses must be a list of strings"},
		{`{"a": {"witnesses": [1]}}`, "'a'.witnesses must be a list of strings"},
	}
	for _, tc := range cases {
		t.Run(tc.content, func(t *testing.T) {
			_, err := WitnessesFromManifest(write(t, t.TempDir(), "w.json", tc.content))
			if msg := importError(t, err); !strings.Contains(msg, tc.fragment) {
				t.Fatalf("message %q lacks %q", msg, tc.fragment)
			}
		})
	}
}

func TestWitnessManifestMissingFile(t *testing.T) {
	_, err := WitnessesFromManifest(filepath.Join(t.TempDir(), "absent.json"))
	if msg := importError(t, err); !strings.Contains(msg, "cannot read witness manifest") {
		t.Fatal(msg)
	}
}

// --- Go-only checks of the Python-semantics helpers -------------------------------------------

func TestPyPathAndSuffix(t *testing.T) {
	cases := []struct{ in, str, suffix, stem string }{
		{"a.", "a.", "", "a."},
		{"a..b", "a..b", ".b", "a."},
		{".x", ".x", "", ".x"},
		{"..", "..", "", ".."},
		{"x.tar.gz", "x.tar.gz", ".gz", "x.tar"},
		{"a//b/./c/", "a/b/c", "", "c"},
		{"", ".", "", ""},
		{".", ".", "", ""},
		{"//x", "//x", "", "x"},
		{"///x", "/x", "", "x"},
	}
	for _, c := range cases {
		p := pyPath(c.in)
		if p != c.str || pySuffix(p) != c.suffix || pyStem(p) != c.stem {
			t.Errorf("%q: got %q %q %q", c.in, p, pySuffix(p), pyStem(p))
		}
	}
	if got := pyLower(".AΣ"); got != ".aς" {
		t.Errorf("final sigma: %q", got)
	}
	if got := pyLower(".İ"); got != ".i̇" {
		t.Errorf("dotted I: %q", got)
	}
}

func TestDecodeErrorsMatchCPython(t *testing.T) {
	cases := map[string]string{
		"\xff":         "'utf-8' codec can't decode byte 0xff in position 0: invalid start byte",
		"ab\xe2\x82":   "'utf-8' codec can't decode bytes in position 2-3: unexpected end of data",
		"\xe2\x82x":    "'utf-8' codec can't decode bytes in position 0-1: invalid continuation byte",
		"\xe2x":        "'utf-8' codec can't decode byte 0xe2 in position 0: invalid continuation byte",
		"\xed\xa0\x80": "'utf-8' codec can't decode byte 0xed in position 0: invalid continuation byte",
	}
	for in, want := range cases {
		if _, err := decodeUTF8([]byte(in)); err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %s", in, err, want)
		}
	}
}
