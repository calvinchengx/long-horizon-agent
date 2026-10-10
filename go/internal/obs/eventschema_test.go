package obs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type warnings []string

func (w *warnings) Warn(msg string, args ...any) {
	*w = append(*w, fmt.Sprint(append([]any{msg}, args...)...))
}

// withAudit runs appendAudit's lazy setup afresh under env, restoring the process's audit file
// (CI runs the suite with LHA_TRACE_AUDIT_DIR set) afterwards.
func withAudit(t *testing.T, dir string) {
	t.Helper()
	saved := auditPath
	t.Cleanup(func() {
		auditPath = saved
		auditOnce = sync.Once{}
		auditOnce.Do(func() {})
	})
	t.Setenv(AuditDirEnv, dir)
	auditOnce = sync.Once{}
	auditPath = ""
}

func TestAppendAuditWritesEachEventWhenTheDirIsSet(t *testing.T) {
	dir := t.TempDir()
	withAudit(t, dir)
	var w warnings
	appendAudit(TraceEvent{Kind: "test_audit", MissionID: "m", Data: Fields{F("n", 1)}}, &w)
	appendAudit(TraceEvent{Kind: "test_audit", CycleID: "c"}, &w)
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("go-%d.ndjson", os.Getpid())))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"test_audit","mission_id":"m","cycle_id":"","data":{"n":1}}` + "\n" +
		`{"kind":"test_audit","mission_id":"","cycle_id":"c","data":{}}` + "\n"
	if string(data) != want || len(w) != 0 {
		t.Fatalf("audit file %q, warnings %v", data, w)
	}
}

func TestAppendAuditIsOffWithoutTheDir(t *testing.T) {
	withAudit(t, "")
	var w warnings
	appendAudit(TraceEvent{Kind: "test_audit"}, &w)
	if auditPath != "" || len(w) != 0 {
		t.Fatalf("auditPath %q, warnings %v", auditPath, w)
	}
}

func TestAppendAuditWarnsWhenItCannotWrite(t *testing.T) {
	withAudit(t, filepath.Join(t.TempDir(), "missing"))
	var w warnings
	appendAudit(TraceEvent{Kind: "test_audit"}, &w)
	if len(w) != 1 || !strings.HasPrefix(w[0], "trace_audit_failed") {
		t.Fatalf("warnings %v", w)
	}
}

func TestAppendAuditReportsAFailedWriteEvenWhenCloseSucceeds(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	withAudit(t, "")
	auditOnce.Do(func() {})
	auditPath = "/dev/full"
	var w warnings
	appendAudit(TraceEvent{Kind: "test_audit"}, &w)
	if len(w) != 1 {
		t.Fatalf("warnings %v", w)
	}
}

func decode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSchemaErrors(t *testing.T) {
	schema := decode(t, `{
		"type": "object",
		"required": ["kind", "n"],
		"properties": {
			"kind": {"enum": ["a", 1]},
			"n": {"type": "integer"},
			"x": {"type": "number"},
			"maybe": {"type": ["string", "null"]},
			"list": {"type": "array", "items": {"type": "boolean"}},
			"inner": {"type": "object", "properties": {"s": {"type": "string"}}, "additionalProperties": false}
		},
		"additionalProperties": {"type": "string"}
	}`).(map[string]any)
	cases := []struct {
		value string
		want  []string
	}{
		{`{"kind": "a", "n": 3, "x": 1.5, "maybe": null, "list": [true], "inner": {"s": "t"}, "extra": "e"}`, nil},
		{`{"kind": 1, "n": 3}`, nil},
		{`{"kind": "1", "n": 3}`, []string{`$.kind: "1" is not one of [a 1]`}},
		{`{"kind": "b", "n": 3.0}`, []string{`$.kind: "b" is not one of [a 1]`, `$.n: expected integer, got number`}},
		{`{"n": 1e2, "x": "y"}`, []string{`$: missing "kind"`, `$.n: expected integer, got number`, `$.x: expected number, got string`}},
		{`{"kind": "a", "n": 1, "maybe": 2, "list": [true, 0]}`, []string{`$.list[1]: expected boolean, got integer`, `$.maybe: expected string or null, got integer`}},
		{`{"kind": "a", "n": 1, "inner": {"s": 1, "t": "u"}, "extra": 2}`, []string{`$.extra: expected string, got integer`, `$.inner.s: expected string, got integer`, `$.inner: unexpected "t"`}},
		{`[1]`, []string{`$: expected object, got array`}},
	}
	for _, tc := range cases {
		got := SchemaErrors(schema, decode(t, tc.value), "$")
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.value, got, tc.want)
		}
	}
	if got := EventErrors(map[string]map[string]any{}, "nope", nil); !reflect.DeepEqual(got, []string{`unknown event kind "nope"`}) {
		t.Errorf("unknown kind: %q", got)
	}
}

func TestAuditCountsEventsKindsAndViolations(t *testing.T) {
	kinds := map[string]map[string]any{
		"a": decode(t, `{"type": "object", "required": ["n"]}`).(map[string]any),
		"b": decode(t, `{"type": "object"}`).(map[string]any),
		"c": decode(t, `{"type": "object"}`).(map[string]any),
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go-1.ndjson", `{"kind":"a","data":{"n":1}}`+"\n\n"+`{"kind":"a","data":{}}`+"\n"+`{"kind":"test_x","data":1}`+"\n")
	write("go-2.ndjson", `{"kind":"a","data":{}}`+"\n"+`{"kind":"b","data":[]}`+"\n")
	write("notes.txt", "not an event\n")
	report, err := Audit(dir, kinds)
	if err != nil {
		t.Fatal(err)
	}
	if report.Events != 4 {
		t.Errorf("events %d", report.Events)
	}
	if !reflect.DeepEqual(report.Seen, map[string]bool{"a": true, "b": true}) {
		t.Errorf("seen %v", report.Seen)
	}
	want := map[string]int{`a: $: missing "n"`: 2, "b: $: expected object, got array": 1}
	if !reflect.DeepEqual(report.Violations, want) {
		t.Errorf("violations %v", report.Violations)
	}
	if got := report.Unseen(kinds); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("unseen %v", got)
	}

	write("go-3.ndjson", "{not json\n")
	if _, err := Audit(dir, kinds); err == nil || !strings.Contains(err.Error(), "go-3.ndjson") {
		t.Errorf("bad line: %v", err)
	}
}

func TestAuditFailsOnUnreadableFilesAndBadPatterns(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "x.ndjson"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Audit(dir, nil); err == nil {
		t.Error("a directory named *.ndjson was read")
	}
	if _, err := Audit("[", nil); err == nil {
		t.Error("a bad glob pattern was accepted")
	}
}
