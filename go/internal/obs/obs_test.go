package obs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Ported from python/tests/unit/test_obs_redact.py, plus the CPython regex semantics the
// hand-written matchers reproduce.

func TestSecretKeysRedactedButTokenCountersKept(t *testing.T) {
	out := RedactMapping(map[string]any{
		"api_key":       "abc",
		"Authorization": "Bearer xyz",
		"db_password":   "hunter2",
		"postgres_dsn":  "postgresql://u:p@h/db",
		"token":         "t0k",
		"input_tokens":  120,
		"max_tokens":    4096,
		"nested":        map[string]any{"anthropic_api_key": "k", "ok": "fine"},
		"wrapped":       Secret("s"),
		"empty_secret":  "",
		"nil_token":     nil,
	})
	for _, k := range []string{"api_key", "Authorization", "db_password", "postgres_dsn", "token", "wrapped"} {
		if out[k] != Redacted {
			t.Errorf("%s = %v, want %q", k, out[k], Redacted)
		}
	}
	if out["input_tokens"] != 120 || out["max_tokens"] != 4096 {
		t.Errorf("counters changed: %v %v", out["input_tokens"], out["max_tokens"])
	}
	if !reflect.DeepEqual(out["nested"], map[string]any{"anthropic_api_key": Redacted, "ok": "fine"}) {
		t.Errorf("nested = %v", out["nested"])
	}
	if out["empty_secret"] != "" || out["nil_token"] != nil {
		t.Errorf("None / empty secret-keyed values must pass through: %v %v", out["empty_secret"], out["nil_token"])
	}
}

func TestSecretLookingValuesRedactedInFreeText(t *testing.T) {
	text := RedactText("called with sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV and header Bearer eyJhbGciOi.x.y " +
		"db postgresql://admin:s3cret@db:5432/lha")
	for _, leaked := range []string{"ABCDEFGHIJ", "eyJhbGciOi", "s3cret"} {
		if strings.Contains(text, leaked) {
			t.Errorf("%q leaked: %s", leaked, text)
		}
	}
	if !strings.Contains(text, "postgresql://admin:***@db:5432/lha") {
		t.Errorf("dsn: %s", text)
	}
}

func TestRedactTextEdgeCases(t *testing.T) {
	// Expected values are the Python reference's output.
	for in, want := range map[string]string{
		"xsk-abcdefghijklmnopqrstu_sk-bbbbbbbbbbbbbbbbbbbb": "xsk-abcdefghijklmnopqrstu_***",
		"sk-short":                        "sk-short",
		"AKIAABCDEFGHIJKLMNOPQ":           "AKIAABCDEFGHIJKLMNOPQ", // \b fails
		"AKIAABCDEFGHIJKLMNOP-x":          "***-x",
		"AKIAABCDEFGHIJKLMNOP\u00e9":      "AKIAABCDEFGHIJKLMNOP\u00e9", // é is a word char
		"authorization: bearer":           "authorization: ***",
		"Authorization:  'Basic  abc,def": "Authorization:  'Basic  ***,def",
		"AUTHOR\u0130ZATION=tok":          "AUTHOR\u0130ZATION=***", // re IGNORECASE: İ ~ i
		"x-authorization: v":              "x-authorization: ***",
		"xauthorization: v":               "xauthorization: v",
		"\u00e9bearer abc":                "\u00e9bearer abc",
		"BEARER a.b/c+d~e_f-g==":          "BEARER ***",
		"bearer \u017fx":                  "bearer ***", // ſ matches [A-Za-z] under IGNORECASE
		"bearer\u2028abc":                 "bearer ***",
		"redis://:pw@h/x?y@z":             "redis://:***@h/x?y@z",
		"a://u:p@q@r@host/":               "a://u:***@host/",
		"HTTP://u:p@h":                    "HTTP://u:p@h",
		"xhttp://u:p@h":                   "xhttp://u:***@h",
		"\u00e9http://u:p@h":              "\u00e9http://u:p@h",
		"ghp_abcdefghijklmnopqrst ghx_abcdefghijklmnopqrst": "*** ghx_abcdefghijklmnopqrst",
		"xoxs-0123456789 xoxq-0123456789":                   "*** xoxq-0123456789",
		"AIza" + strings.Repeat("a", 29):                    "AIza" + strings.Repeat("a", 29),
		"AIza" + strings.Repeat("a", 30):                    "***",
	} {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%+q) = %+q, want %+q", in, got, want)
		}
	}
}

// Matches that end exactly at the end of the text, and the bounds of each character class
// (mutation audit). Expected values are the Python reference's output.
func TestRedactTextBoundaries(t *testing.T) {
	for in, want := range map[string]string{
		"gh":                   "gh",
		"AKIAABCDEFGHIJKLMNOP": "***",
		"AKIA0123":             "AKIA0123",
		"AKIAabcdefghijklmnop": "AKIAabcdefghijklmnop",
		"AKIA0000000000000000 AKIA9999999999999999 AKIAZZZZZZZZZZZZZZZZ":                      "*** *** ***",
		"AKIA/AAAAAAAAAAAAAAA AKIA:AAAAAAAAAAAAAAA AKIA@AAAAAAAAAAAAAAA AKIA[AAAAAAAAAAAAAAA": "AKIA/AAAAAAAAAAAAAAA AKIA:AAAAAAAAAAAAAAA AKIA@AAAAAAAAAAAAAAA AKIA[AAAAAAAAAAAAAAA",
		"Zsk-abcdefghijklmnopq 9sk-abcdefghijklmnopq zsk-abcdefghijklmnopq":                   "Zsk-abcdefghijklmnopq 9sk-abcdefghijklmnopq zsk-abcdefghijklmnopq",
		"authorization":            "authorization",
		"authorization: ":          "authorization: ",
		"Authorization: Bearer  ;": "Authorization: ***  ;",
		"z://u:p@h":                "z://u:***@h",
		"h2://u:p@h":               "h2://u:***@h",
		"az://u:p@h":               "az://u:***@h",
		"ba://u:p@h":               "ba://u:***@h",
		"h9://u:p@h":               "h9://u:***@h",
		"h0://u:p@h":               "h0://u:***@h",
		"a://user":                 "a://user",
		"a://u:@h":                 "a://u:***@h",
	} {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%+q) = %+q, want %+q", in, got, want)
		}
	}
	for _, r := range "09AZ" {
		if !classDigitUpper(r) {
			t.Errorf("classDigitUpper(%q) = false", r)
		}
	}
	for _, r := range "/:@[a" {
		if classDigitUpper(r) {
			t.Errorf("classDigitUpper(%q) = true", r)
		}
	}
	for _, r := range "09azAZ" {
		if !isASCIIAlnum(r) {
			t.Errorf("isASCIIAlnum(%q) = false", r)
		}
	}
	for _, r := range "/:`{@[" {
		if isASCIIAlnum(r) {
			t.Errorf("isASCIIAlnum(%q) = true", r)
		}
	}
}

func TestRedactOrderedMap(t *testing.T) {
	m := contracts.NewOrderedMap("api_key", "k", "note", "use sk-abcdefghijklmnopqrstu", "n", 1)
	out, ok := RedactValue(m).(*contracts.OrderedMap)
	if !ok || out == m {
		t.Fatalf("RedactValue(OrderedMap) = %#v", out)
	}
	var keys []string
	out.Range(func(k string, _ any) bool { keys = append(keys, k); return true })
	if !reflect.DeepEqual(keys, []string{"api_key", "note", "n"}) || out.Value("api_key") != Redacted ||
		out.Value("note") != "use ***" || out.Value("n") != 1 {
		t.Errorf("RedactValue(OrderedMap) = %v %v %v %v", keys, out.Value("api_key"), out.Value("note"), out.Value("n"))
	}
	if m.Value("api_key") != "k" {
		t.Error("RedactValue changed its input")
	}
	var none *contracts.OrderedMap
	if got, ok := RedactValue(none).(*contracts.OrderedMap); !ok || got != nil {
		t.Errorf("RedactValue(nil OrderedMap) = %#v", got)
	}
}

func TestCamelBoundary(t *testing.T) {
	for in, want := range map[string]string{
		"aAzZ0A9Z":      "a_Az_Z0_A9_Z",
		"A@A[A`A{A/A:A": "A@A[A`A{A/A:A",
		"a@a[":          "a@a[",
	} {
		if got := camelBoundary(in); got != want {
			t.Errorf("camelBoundary(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSecretKey(t *testing.T) {
	for key, want := range map[string]bool{
		"api-key": true, "APIKEY": true, "passwd": true, "PassWord": true, "pasword": false,
		"private_key": true, "privatekey": true, "session_cookie": true, "x_auth": true,
		"x_auth\n": true, "x_auth\n\n": false, "author": false, "tokens_used": false,
		"myToken": true, "my2Auth": true, "MYTOKEN": false, "-token": true, "token-x": false,
		"\u0131d_token": true, "api\u212aey": true, "\u017fecret": true, "": false,
		"credentials": true, "DSN": true,
		"api_version": false, "private_note": false, "my_api": false, "apiary": false,
		"aToken": true, "zToken": true, "0Token": true, "9Token": true, "@Token": false,
		"[Token": false, "`Token": false, "{Token": false, "/Token": false, ":Token": false,
		"aAuth": true,
	} {
		if got := IsSecretKey(key); got != want {
			t.Errorf("IsSecretKey(%+q) = %v, want %v", key, got, want)
		}
	}
}

type named string

func TestRedactValue(t *testing.T) {
	in := map[string]any{
		"list":    []any{"sk-abcdefghijklmnopqrst", 1, nil, Secret("x")},
		"strs":    []string{"Bearer abc"},
		"arr":     [2]string{"ok", "bearer x"},
		"bytes":   []byte("sk-abcdefghijklmnopqrst"),
		"intmap":  map[int]string{1: "bearer y"},
		"typed":   map[string]string{"password": "p", "note": "bearer z"},
		"named":   named("bearer q"),
		"number":  3.5,
		"boolean": true,
	}
	got := RedactValue(in)
	want := map[string]any{
		"list":    []any{Redacted, 1, nil, Redacted},
		"strs":    []any{"Bearer ***"},
		"arr":     []any{"ok", "bearer ***"},
		"bytes":   []byte("sk-abcdefghijklmnopqrst"),
		"intmap":  map[string]any{"1": "bearer ***"},
		"typed":   map[string]any{"password": Redacted, "note": "bearer ***"},
		"named":   "bearer ***",
		"number":  3.5,
		"boolean": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RedactValue =\n %#v\nwant\n %#v", got, want)
	}
	if RedactValue(nil) != nil {
		t.Error("nil")
	}
	if fmt.Sprint(Secret("hunter2")) != Redacted || fmt.Sprintf("%#v", Secret("x")) != Redacted ||
		Secret("v").Value() != "v" {
		t.Error("Secret formatting")
	}
}

func TestTraceRecorderRedactsEventData(t *testing.T) {
	var logs bytes.Buffer
	rec := NewTraceRecorder(slog.New(slog.NewJSONHandler(&logs, nil)))
	event := rec.Record("tool_call", "m", "",
		F("api_key", "sk-live"), F("output", "key=sk-ABCDEFGHIJKLMNOPQRST"), F("n", 3))
	if v, _ := event.Data.Get("api_key"); v != Redacted {
		t.Errorf("api_key = %v", v)
	}
	if v, _ := event.Data.Get("output"); strings.Contains(fmt.Sprint(v), "ABCDEFGHIJ") {
		t.Errorf("output leaked: %v", v)
	}
	if v, _ := event.Data.Get("n"); v != 3 {
		t.Errorf("n = %v", v)
	}
	jsonl, err := rec.ToJSONL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonl, "sk-live") || strings.Contains(logs.String(), "sk-live") {
		t.Error("secret reached the trace or the log")
	}
	want := `{"kind":"tool_call","mission_id":"m","cycle_id":"","data":{"api_key":"***","output":"key=***","n":3}}`
	if jsonl != want {
		t.Errorf("ToJSONL =\n %s\nwant\n %s", jsonl, want)
	}
	if !strings.Contains(logs.String(), `"msg":"tool_call"`) || !strings.Contains(logs.String(), `"mission_id":"m"`) {
		t.Errorf("log line: %s", logs.String())
	}
}

func TestTraceRecorderOrderingAndJSON(t *testing.T) {
	rec := &TraceRecorder{} // the zero value logs to the default logger
	slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	rec.Record("a", "m", "c1", F("z", 1), F("a", "<b>&"), F("z", 2))
	rec.Record("b", "m", "c2")
	jsonl, err := rec.ToJSONL()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"a","mission_id":"m","cycle_id":"c1","data":{"z":2,"a":"<b>&"}}` + "\n" +
		`{"kind":"b","mission_id":"m","cycle_id":"c2","data":{}}`
	if jsonl != want {
		t.Errorf("ToJSONL =\n%s\nwant\n%s", jsonl, want)
	}
	events := rec.Events()
	if len(events) != 2 || events[0].Data.Map()["z"] != 2 {
		t.Errorf("events = %+v", events)
	}
	var back TraceEvent
	if err := json.Unmarshal([]byte(strings.Split(jsonl, "\n")[0]), &back); err != nil {
		t.Fatal(err)
	}
	if back.Data[0].Key != "z" || back.Data[1].Key != "a" || back.CycleID != "c1" {
		t.Errorf("round trip = %+v", back)
	}
	var null Fields
	if err := json.Unmarshal([]byte("null"), &null); err != nil || null == nil {
		t.Errorf("null data: %v %v", null, err)
	}
	if _, ok := (Fields{}).Get("x"); ok {
		t.Error("Get on empty")
	}
	b, _ := json.Marshal(TraceEvent{Kind: "k"})
	if string(b) != `{"kind":"k","mission_id":"","cycle_id":"","data":{}}` {
		t.Errorf("zero event: %s", b)
	}
	if _, err := json.Marshal(Fields{F("bad", func() {})}); err == nil {
		t.Error("unmarshalable value accepted")
	}
	if err := json.Unmarshal([]byte(`{"a":`), &null); err == nil {
		t.Error("truncated JSON accepted")
	}
}

func TestConfigureLogging(t *testing.T) {
	defer slog.SetDefault(slog.Default())
	var buf bytes.Buffer
	ConfigureLogging(&buf, true)
	Logger("").Info("hello", "k", "v")
	if !strings.Contains(buf.String(), `"msg":"hello"`) || !strings.Contains(buf.String(), `"logger":"lha"`) {
		t.Errorf("json log: %s", buf.String())
	}
	buf.Reset()
	ConfigureLogging(&buf, false)
	Logger("x").Info("hi")
	if !strings.Contains(buf.String(), "msg=hi") || !strings.Contains(buf.String(), "logger=x") {
		t.Errorf("text log: %s", buf.String())
	}
	ConfigureLogging(nil, false)
}
