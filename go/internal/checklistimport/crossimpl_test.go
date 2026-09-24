package checklistimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCrossImplMatchesPython loads the same files with the Python reference implementation
// (lha.state.checklist_import) and with this package, and requires identical results: the same
// checklist (compared as parsed JSON), title, description and references, or the same error
// class and exact message.

const pyLoadAll = `
import json, sys
from lha.state.checklist_import import ChecklistImportError, load_checklist, witnesses_from_manifest
out = []
for arg in sys.argv[1:]:
    mode, include, path = arg.split("|", 2)
    try:
        if mode == "manifest":
            out.append({"ok": True, "manifest": witnesses_from_manifest(path)})
        else:
            r = load_checklist(path, include_done=include == "1")
            out.append({"ok": True, "checklist": r.checklist.model_dump(), "title": r.title,
                        "description": r.description, "references": r.references})
    except ChecklistImportError as e:
        out.append({"ok": False, "kind": "import", "error": str(e)})
    except Exception as e:
        out.append({"ok": False, "kind": type(e).__name__, "error": str(e)})
print(json.dumps(out))
`

type fixture struct {
	name, content string
	includeDone   bool
	manifest      bool
	path          string // overrides the path passed to both loaders (relative to the dir)
}

func crossFixtures() []fixture {
	md := func(name, content string) fixture { return fixture{name: name + ".md", content: content} }
	js := func(name, content string) fixture { return fixture{name: name + ".json", content: content} }
	item := func(extra string) string { return `{"items": [{"description": "d"` + extra + `}]}` }
	fx := []fixture{
		md("roadmap", roadmap),
		{name: "roadmap-done.md", content: roadmap, includeDone: true},
		md("title-hashes", "# Title ##  \n\nIntro one\nstill one\n\n\nIntro two\n## P\n- [ ] a\n"),
		md("title-only-hash", "# #\n- [ ] a\n"),
		md("bom", "\ufeff# T\n- [ ] x\n"),
		md("crlf", "# T\r\n\r\nintro\r\n\r\n- [ ] one\r\n  more\r\n## P\r\n- [ ] two\r\n"),
		md("cr-only", "# T\r- [ ] one\r- [ ] two\r"),
		md("unicode-lines", "# T - [ ] one\x0c- [ ] two\x1c- [ ] three\u0085  cont\n"),
		md("unicode-indent", "- [ ] top\n　　- [ ] nested ideographic\n  cont\n"),
		md("numbered", "- [ ] a\n  1. nested numbered\n     under it\n  back\n- [ ] b\n  ١) arabic digit bullet\n"),
		md("witness-case", "- [ ] x (wİtness: go:TestA) (WITNEſS: go:TestB) (witnesses: , ,)\n"),
		md("fences", "- [ ] a\n~~~\n- [ ] fenced\n```\n- [ ] still fenced? no\n```\n- [ ] after\n"),
		md("headings", "# T\n####### seven\n- [ ] a\n### sub\n  not a continuation\n##\tTabbed phase\n- [x] done\n- [ ] b\n"),
		md("markers", "* [X] star done\n+ [ ] plus open\n- [ ]\n-  [ ]   spaced   out   \n- [x] lower x\n"),
		{name: "markers-done.md", content: "* [X] star done\n+ [ ] plus open\n- [x] lower x\n", includeDone: true},
		md("skip-indent", "- [ ] a\n  - nested\n\n    deeper\n  back to a?\n- plain\n  plain continuation\n- [ ] b\n"),
		md("intro-after-heading", "intro before title\n# Title\n\npara\n### h\nmore intro\n- bullet\n  x\n\n## P\n- [ ] a\n"),
		md("bad-witness", "# T\n\n- [ ] fine\n- [ ] bad (witness: go:not-a-test)\n"),
		md("only-witness", "## P\n\n- [ ] (witness: go:TestX)\n"),
		md("no-open", "# T\n\n- [x] already done\n- plain bullet\n"),
		md("empty", ""),
		{name: "upper.MARKDOWN", content: "- [ ] upper suffix\n"},
		{name: "x.JSON", content: `[{"description": "upper json"}]`},
		{name: "plan.yaml", content: "items: []"},
		{name: "noext", content: "- [ ] x\n"},
		{name: "dot.", content: "- [ ] x\n"},
		{name: "A.MΣ", content: "- [ ] x\n"},
		{name: "invalid-utf8.md", content: "- [ ] ok\n\xe2\x82x\n"},
		{name: "invalid-utf8.json", content: "\xff"},
		{name: "slashes.json", content: `[{"description": "s"}]`, path: "./sub//slashes.json/"},
		{name: "missing.json", path: "absent.json"},

		js("mission", `{"title": " 　Livy\x1c ", "description": "\tReal.\n", "references": ["docs/livy.md"],
			"items": [{"description": "sessions API", "witnesses": ["go:TestLivySessions"]},
			{"description": "batches API", "depends_on": ["01"], "allow_harness_edits": true},
			{"id": "e2e", "description": "real spark", "depends_on": ["02"], "witnesses": ["ci:sail"]}]}`),
		js("toplevel-list", `[{"description": "a"}, {"description": "b", "id": "x"}]`),
		js("dup-keys", `{"items": [{"description": "first", "status": "done", "description": "second"}], "title": "a", "title": "b"}`),
		js("coercions", `{"items": [{"description": "d", "attempts": " 1_0.00 ", "consecutive_failures": 2.0,
			"schema_version": true, "allow_harness_edits": "YES", "status": "blocked", "verified_by": [], "notes": "n"}]}`),
		js("bool-num", item(`, "allow_harness_edits": 1.0, "attempts": -0.0`)),
		js("no-items", `{"title": "x"}`),
		js("string", `"just a string"`),
		js("empty-items", `{"items": []}`),
		js("bare-string", `{"items": ["bare string"]}`),
		js("unknown", `{"items": [{"description": "x", "witness": [], "zé": 1, "Z": 2, "witness": 3}]}`),
		js("status", item(`, "status": "finished"`)),
		js("missing-desc", `{"items": [{"status": "todo", "notes": "`+strings.Repeat("é", 40)+`"}]}`),
		js("many-errors", `{"items": [{"id": null, "description": 3, "status": 1.5e300, "verified_by": "ab",
			"depends_on": ["a", 1, null, [1], {"k": true}], "attempts": "1.5", "consecutive_failures": NaN,
			"last_failure": -0.0, "allow_harness_edits": 2, "witnesses": {"a": 1}, "notes": [1.0, "a'b", null, true],
			"schema_version": 9.3e18}]}`),
		js("int-errors", item(`, "attempts": 2.5, "consecutive_failures": " 1__0 ", "schema_version": Infinity`)),
		js("bool-errors", item(`, "allow_harness_edits": " true"`)),
		js("bool-errors2", item(`, "allow_harness_edits": 0.5`)),
		js("bool-errors3", item(`, "allow_harness_edits": null`)),
		js("long-repr", item(`, "status": "`+strings.Repeat("😀", 3)+strings.Repeat("x", 40)+`"`)),
		js("long-repr2", item(`, "status": "a`+strings.Repeat("é", 30)+`"`)),
		js("blank-desc", item(`, "description": "　\u0085 "`)),
		js("blank-id", item(`, "id": " "`)),
		js("title-type", `{"title": 3, "items": [{"description": "x"}]}`),
		js("title-nan", `{"title": NaN, "items": [{"description": "x"}]}`),
		js("title-null", `{"title": null, "items": [{"description": "x"}]}`),
		js("refs", `{"references": "docs", "items": [{"description": "x"}]}`),
		js("refs2", `{"references": ["a", 1], "items": [{"description": "x"}]}`),
		js("dup-id", `{"items": [{"id": "01", "description": "x"}, {"description": "y", "id": "01"}]}`),
		js("unknown-dep", item(`, "depends_on": ["zz", "01"]`)),
		js("cycle", `{"items": [{"id": "a", "description": "x", "depends_on": ["b"]}, {"id": "b", "description": "y", "depends_on": ["a"]}]}`),
		js("bad-witness", item(`, "witnesses": ["sdk:TestX"]`)),
		js("bad-witness-quote", `{"items": [{"id": "it's", "description": "x", "witnesses": ["go:Test\"x"]}]}`),
		js("bom", "\ufeff[]"),
		js("big-int", `[{"description": "x", "attempts": `+strings.Repeat("9", 5000)+`}]`),
		js("escapes", `[{"description": "tab\t\u00e9\ud83d\ude00\/\b\"x"}]`),
	}
	for i, bad := range []string{
		"", " ", "{", "{ ", `{"a"`, `{"a" 1}`, `{"a":`, `{"a":}`, `{"a":1`, `{"a":1,`, `{"a":1,}`,
		`{"a":1 "b"}`, "[", "[1,]", "[1 2]", "[1", `"abc`, `"a\`, `"a\x"`, `"a\u12"`, `"a\u12`,
		`"\u12G4"`, "\"a\tb\"", "\"\x00\"", "-", "-x", "-Inf", "nul", "tru", "1.", "1.e5", "1e", "1e+",
		"01", "1 2", "[]\n x", "{\"a\":\n\n  x}", `"\ud800\u12"`, `"\ud800\uZZ00"`, `"\u1234`, "é x",
		"nan", `{"a":1}}`, " \n [}", `[{"description": "x"}] trailing`,
	} {
		fx = append(fx, fixture{name: "syntax" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".json", content: bad})
	}
	for i, m := range []string{
		`{"row": {"claim": "RLS", "witnesses": ["ci:x", "go:TestY"]}, "blob": {"witnesses": []}, "_gated": 1}`,
		"{nope", "[1, 2]", `{"a": {"claim": "x"}}`, `{"a": ["go:TestX"]}`, `{"a": {"witnesses": [1]}}`,
		`{"ok": {"witnesses": ["x"]}, "it's": null}`, "\xfe", "\ufeff{}",
	} {
		fx = append(fx, fixture{name: "manifest" + string(rune('a'+i)) + ".json", content: m, manifest: true})
	}
	fx = append(fx, fixture{name: "manifest-missing.json", path: "nope/manifest.json", manifest: true})
	return fx
}

type crossResult struct {
	OK          bool            `json:"ok"`
	Kind        string          `json:"kind,omitempty"`
	Error       string          `json:"error,omitempty"`
	Checklist   json.RawMessage `json:"checklist,omitempty"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	References  []string        `json:"references"`
	Manifest    json.RawMessage `json:"manifest,omitempty"`
}

func goResult(t *testing.T, f fixture, path string) crossResult {
	t.Helper()
	fail := func(err error) crossResult {
		var ie *ImportError
		var de *DecodeError
		switch {
		case errors.As(err, &ie):
			return crossResult{Kind: "import", Error: err.Error()}
		case errors.As(err, &de):
			return crossResult{Kind: "UnicodeDecodeError", Error: err.Error()}
		}
		return crossResult{Kind: "ValueError", Error: err.Error()}
	}
	if f.manifest {
		m, err := WitnessesFromManifest(path)
		if err != nil {
			return fail(err)
		}
		raw, _ := json.Marshal(m)
		return crossResult{OK: true, Manifest: raw}
	}
	r, err := Load(path, f.includeDone)
	if err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(r.Checklist)
	if err != nil {
		t.Fatal(err)
	}
	return crossResult{OK: true, Checklist: raw, Title: r.Title, Description: r.Description, References: r.References}
}

func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func TestCrossImplMatchesPython(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; skipping the cross-implementation check")
	}
	if testing.Short() {
		t.Skip("cross-implementation check is slow")
	}
	_, file, _, _ := runtime.Caller(0)
	project := filepath.Join(filepath.Dir(file), "..", "..", "..", "python")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "adir.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixtures := append(crossFixtures(), fixture{name: "adir.json", path: "adir.json"})
	var args, paths []string
	for _, f := range fixtures {
		if f.content != "" || f.path == "" {
			target := filepath.Join(dir, f.name)
			if strings.HasPrefix(f.path, "./sub/") {
				target = filepath.Join(dir, "sub", f.name)
			}
			if err := os.WriteFile(target, []byte(f.content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(dir, f.name)
		if f.path != "" {
			path = dir + "/" + f.path
		}
		mode, include := "load", "0"
		if f.manifest {
			mode = "manifest"
		}
		if f.includeDone {
			include = "1"
		}
		args = append(args, mode+"|"+include+"|"+path)
		paths = append(paths, path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "uv", append([]string{"run", "--quiet", "--project", project, "python", "-c", pyLoadAll}, args...)...)
	cmd.Dir = project
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python failed: %v\n%s", err, stderr.String())
	}
	var want []crossResult
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatalf("%v in %s", err, out)
	}
	if len(want) != len(fixtures) {
		t.Fatalf("python returned %d results for %d fixtures", len(want), len(fixtures))
	}
	for i, f := range fixtures {
		got, w := goResult(t, f, paths[i]), want[i]
		if w.References == nil && w.OK && !f.manifest {
			w.References = []string{}
		}
		same := got.OK == w.OK && got.Kind == w.Kind && got.Error == w.Error &&
			got.Title == w.Title && got.Description == w.Description &&
			reflect.DeepEqual(got.References, w.References) &&
			jsonEqual(got.Checklist, w.Checklist) && jsonEqual(got.Manifest, w.Manifest)
		if !same {
			g, _ := json.Marshal(got)
			p, _ := json.Marshal(w)
			t.Errorf("%s differs:\n go:     %s\n python: %s", f.name, g, p)
		}
	}
}
