package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

func tctx(t *testing.T, dir string) contracts.ToolContext {
	t.Helper()
	s, err := execution.NewLocalSandbox().Open(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return contracts.ToolContext{MissionID: "m1", Session: s}
}

func forTools(t *testing.T, tools []contracts.Tool, opts execution.DispatcherOptions) *execution.AllowListDispatcher {
	t.Helper()
	d, err := execution.ForTools(tools, opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func call(id, name string, args map[string]any) contracts.ToolCall {
	return contracts.ToolCall{ID: id, Name: name, Arguments: args}
}

func dispatch(d contracts.ToolDispatcher, c contracts.ToolCall, tc contracts.ToolContext) contracts.ToolResult {
	return d.Dispatch(context.Background(), c, tc)
}

func TestWriteThenReadRoundTrips(t *testing.T) {
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	tc := tctx(t, t.TempDir())
	wrote := dispatch(d, call("1", "write_file", map[string]any{"path": "a/b.txt", "content": "héllo"}), tc)
	if !wrote.OK || wrote.Content != "wrote 5 bytes to a/b.txt" {
		t.Fatal(wrote)
	}
	read := dispatch(d, call("2", "read_file", map[string]any{"path": "a/b.txt"}), tc)
	if !read.OK || read.Content != "héllo" {
		t.Fatal(read)
	}
	missing := dispatch(d, call("3", "read_file", map[string]any{"path": "nope"}), tc)
	root := execution.Realpath(tc.Session.Workdir())
	if missing.ErrorText() != "cannot read 'nope': [Errno 2] No such file or directory: '"+root+"/nope'" {
		t.Fatal(missing.ErrorText())
	}
}

func TestEgressIsDefaultDenied(t *testing.T) {
	search, _ := NewWebSearchTool(WebSearchOptions{APIKey: "unused"})
	tools := append(DefaultLocalTools(), search)
	d := forTools(t, tools, execution.DispatcherOptions{AllowMutating: true})
	res := dispatch(d, call("3", "web_search", map[string]any{"query": "anything"}), tctx(t, t.TempDir()))
	if res.OK || res.ErrorText() != "egress is disabled (default-deny): 'web_search'" {
		t.Fatal(res)
	}
	enabled := forTools(t, tools, execution.DispatcherOptions{AllowMutating: true, AllowEgress: true})
	if !slices.ContainsFunc(enabled.Specs(), func(s contracts.ToolSpec) bool { return s.Name == "web_search" }) {
		t.Fatal("web_search not advertised")
	}
}

func TestUnknownToolAndMissingArgs(t *testing.T) {
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	tc := tctx(t, t.TempDir())
	if r := dispatch(d, call("4", "nope", map[string]any{}), tc); r.ErrorText() != "unknown tool: 'nope'" {
		t.Fatal(r)
	}
	if r := dispatch(d, call("5", "read_file", map[string]any{}), tc); r.ErrorText() != "missing required args for 'read_file': ['path']" {
		t.Fatal(r)
	}
	if r := dispatch(d, call("5", "write_file", nil), tc); r.ErrorText() != "missing required args for 'write_file': ['content', 'path']" {
		t.Fatal(r)
	}
}

func TestAllowListAndMutatingPolicy(t *testing.T) {
	tc := tctx(t, t.TempDir())
	d, err := execution.NewAllowListDispatcher(DefaultLocalTools(), execution.DispatcherOptions{Allow: []string{"read_file"}, AllowMutating: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := dispatch(d, call("6", "write_file", map[string]any{"path": "x", "content": "y"}), tc); r.ErrorText() != "tool not allowed: 'write_file'" {
		t.Fatal(r)
	}
	noMutate := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{})
	if r := dispatch(noMutate, call("7", "write_file", map[string]any{"path": "x", "content": "y"}), tc); r.ErrorText() != "mutating tools are disabled: 'write_file'" {
		t.Fatal(r)
	}
}

func TestArgumentTypesAreValidated(t *testing.T) {
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	tc := tctx(t, t.TempDir())
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"read_file", map[string]any{"path": 123.0}, "invalid args for 'read_file': path must be of type string, got int"},
		{"write_file", map[string]any{"path": "a", "content": []any{"x"}}, "invalid args for 'write_file': content must be of type string, got list"},
		{"run_command", map[string]any{"argv": "rm -rf /"}, "invalid args for 'run_command': argv must be of type array, got str"},
		{"run_command", map[string]any{"argv": []any{"ls", 3.0}}, "invalid args for 'run_command': argv[1] must be of type string, got int"},
		{"run_command", map[string]any{"argv": []any{"ls"}, "timeout_s": true}, "invalid args for 'run_command': timeout_s must be of type integer, got bool"},
		{"run_command", map[string]any{"argv": []any{"ls"}, "timeout_s": 0.0}, "invalid args for 'run_command': timeout_s must be >= 1"},
		{"run_command", map[string]any{"argv": []any{"ls"}, "timeout_s": 1.5}, "invalid args for 'run_command': timeout_s must be of type integer, got float"},
		{"grep", map[string]any{"pattern": "x", "regex": "yes"}, "invalid args for 'grep': regex must be of type boolean, got str"},
	}
	for _, c := range cases {
		if r := dispatch(d, call("1", c.name, c.args), tc); r.OK || r.ErrorText() != c.want {
			t.Errorf("%s %v: %q", c.name, c.args, r.ErrorText())
		}
	}
}

func TestWritesToHarnessDirsAreDenied(t *testing.T) {
	tmp := t.TempDir()
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	tc := tctx(t, tmp)
	for _, p := range []string{".lha/checklist.json", ".git/hooks/pre-commit", "./.LHA/x", "a/../.git/config"} {
		r := dispatch(d, call("w", "write_file", map[string]any{"path": p, "content": "x"}), tc)
		want := "'write_file' may not modify harness-owned path '" + p + "' (.lha/ or .git/)"
		if r.OK || r.ErrorText() != want {
			t.Errorf("%s: %q", p, r.ErrorText())
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, ".lha")); err == nil {
		t.Fatal(".lha created")
	}
	os.Mkdir(filepath.Join(tmp, ".lha"), 0o755)
	os.WriteFile(filepath.Join(tmp, ".lha", "n.txt"), []byte("hi"), 0o644)
	if r := dispatch(d, call("r", "read_file", map[string]any{"path": ".lha/n.txt"}), tc); !r.OK {
		t.Fatal(r)
	}
	if r := dispatch(d, call("r", "read_file", map[string]any{"path": "../x"}), tc); r.ErrorText() != "path not allowed for 'read_file': path escapes the workspace: '../x'" {
		t.Fatal(r)
	}
}

func TestWritesThroughSymlinkToHarnessDirAreDenied(t *testing.T) {
	tmp := t.TempDir()
	os.Mkdir(filepath.Join(tmp, ".git"), 0o755)
	os.Symlink(filepath.Join(tmp, ".git"), filepath.Join(tmp, "innocent"))
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	r := dispatch(d, call("w", "write_file", map[string]any{"path": "innocent/config", "content": "x"}), tctx(t, tmp))
	if r.OK {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".git", "config")); err == nil {
		t.Fatal("wrote through the link")
	}
}

type callbackGate struct {
	asked    []contracts.GateRequest
	decision contracts.GateDecision
	by       string
	pending  bool
	err      error
}

func (g *callbackGate) Request(_ context.Context, req contracts.GateRequest) (contracts.GateResolution, error) {
	g.asked = append(g.asked, req)
	if g.err != nil {
		return contracts.GateResolution{}, g.err
	}
	if g.pending {
		return contracts.GateResolution{GateID: req.GateID, Decision: contracts.GateReject, ResolvedBy: contracts.PendingApproval}, nil
	}
	return contracts.GateResolution{GateID: req.GateID, Decision: g.decision, ResolvedBy: g.by}, nil
}

func TestIrreversibleCommandsDeniedWithoutGate(t *testing.T) {
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	r := dispatch(d, call("p", "run_command", map[string]any{"argv": []any{"git", "push", "origin"}}), tctx(t, t.TempDir()))
	if r.OK || !strings.HasPrefix(r.ErrorText(), "irreversible action denied (no human gate configured): ") {
		t.Fatal(r)
	}
	events := d.DrainEvents()
	if len(events) != 1 || events[0].Kind != "tool_approval" {
		t.Fatal(events)
	}
	p := events[0].Payload.Plain()
	if p["tool"] != "run_command" || p["arguments"] != "{'argv': ['git', 'push', 'origin']}" ||
		p["decision"] != "reject" || p["approved"] != false || p["resolved_by"] != "no human gate configured" ||
		p["defaulted"] != true || p["fingerprint"] != "0c6efe57c6d81fa336b6a6459b37de23" || // python's
		p["reason"] != "git push (outward-facing / rewrites history)" ||
		r.ErrorText() != "irreversible action denied (no human gate configured): git push (outward-facing / rewrites history)" {
		t.Fatal(p)
	}
	if len(d.DrainEvents()) != 0 {
		t.Fatal("not drained")
	}
}

func TestIrreversibleCommandsRouteThroughGate(t *testing.T) {
	tc := tctx(t, t.TempDir())
	argv := []any{"twine", "upload"}
	c := call("p", "run_command", map[string]any{"argv": argv})
	gate := &callbackGate{decision: contracts.GateApprove, by: "alice"}
	approved := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true, Gate: gate})
	r := dispatch(approved, c, tc)
	if len(gate.asked) != 1 {
		t.Fatal(gate.asked)
	}
	req := gate.asked[0]
	if req.Risk != contracts.RiskIrreversible || req.DefaultAction != contracts.GateReject ||
		req.GateID != "m1:tool:p" || !strings.HasPrefix(req.Question, "Allow 'run_command'? ") ||
		req.Context["argv"] != `["twine", "upload"]` || req.Context["arguments"] != "{'argv': ['twine', 'upload']}" {
		t.Fatal(req)
	}
	if strings.Contains(r.ErrorText(), "denied") { // it ran (twine may be absent)
		t.Fatal(r)
	}
	ev := approved.DrainEvents()
	if len(ev) != 1 || ev[0].Payload.Plain()["decision"] != "approve" || ev[0].Payload.Plain()["approved"] != true || ev[0].Payload.Plain()["resolved_by"] != "alice" {
		t.Fatal(ev)
	}

	rejecting := &callbackGate{decision: contracts.GateReject, by: "policy"}
	auto := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true, Gate: rejecting})
	r = dispatch(auto, c, tc)
	reason, _ := safety.ClassifyCommand([]string{"twine", "upload"})
	if r.OK || r.ErrorText() != "irreversible action denied (reject by policy): "+reason {
		t.Fatal(r)
	}
	pending := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true, Gate: &callbackGate{pending: true}})
	r = dispatch(pending, c, tc)
	if !strings.HasPrefix(r.ErrorText(), "queued for human approval: "+reason+". It is NOT done.") {
		t.Fatal(r)
	}
	if ev := pending.DrainEvents(); ev[0].Payload.Plain()["decision"] != "pending" {
		t.Fatal(ev)
	}
	broken := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true, Gate: &callbackGate{err: context.DeadlineExceeded}})
	if r := dispatch(broken, c, tc); r.ErrorText() != "irreversible action denied (gate error: TimeoutError): "+reason {
		t.Fatal(r)
	}

	// Benign commands never hit the gate.
	gate.asked = nil
	ok := dispatch(approved, call("b", "run_command", map[string]any{"argv": []any{"sh", "-c", "echo hi"}}), tc)
	if !ok.OK || len(gate.asked) != 0 || ok.Content != "exit_code=0\n--- stdout ---\nhi\n\n--- stderr ---\n" {
		t.Fatalf("%q %v", ok.Content, gate.asked)
	}
	fail := dispatch(approved, call("b", "run_command", map[string]any{"argv": []any{"sh", "-c", "exit 3"}, "timeout_s": 5.0}), tc)
	if fail.OK || fail.ErrorText() != "exit 3" || !strings.HasPrefix(fail.Content, "exit_code=3\n") {
		t.Fatal(fail)
	}
}

func TestRuleOfTwo(t *testing.T) {
	tools := append(DefaultLocalTools(), NewFetchURLTool(FetchURLOptions{Policy: &safety.EgressPolicy{}}))
	d := forTools(t, tools, execution.DispatcherOptions{AllowMutating: true, AllowEgress: true})
	if caps := d.Capabilities(); !slices.Equal(caps, []safety.Capability{safety.ExternalComms, safety.UntrustedContent}) {
		t.Fatal(caps)
	}
	_, err := execution.ForTools(tools, execution.DispatcherOptions{AllowMutating: true, AllowEgress: true,
		Capabilities: []safety.Capability{safety.PrivateData}})
	if !errors.Is(err, safety.ErrRuleOfTwoViolation) {
		t.Fatal(err)
	}
	gated := forTools(t, tools, execution.DispatcherOptions{AllowMutating: true, AllowEgress: true,
		Capabilities: []safety.Capability{safety.PrivateData}, Gate: &callbackGate{decision: contracts.GateReject, by: "policy"}})
	if len(gated.Capabilities()) != 3 {
		t.Fatal(gated.Capabilities())
	}
	forTools(t, tools, execution.DispatcherOptions{AllowMutating: true, Capabilities: []safety.Capability{safety.PrivateData}})
	// The trifecta with a gate gates every egress call.
	r := dispatch(gated, call("f", "fetch_url", map[string]any{"url": "https://example.com"}), tctx(t, t.TempDir()))
	if r.OK || r.ErrorText() != "irreversible action denied (reject by policy): egress while holding untrusted input + private data + external comms" {
		t.Fatal(r)
	}
}

func TestGrepIsLiteralByDefaultAndGuardsRegex(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("a.b\naxb\n"), 0o644)
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	tc := tctx(t, tmp)
	if r := dispatch(d, call("g", "grep", map[string]any{"pattern": "a.b"}), tc); r.Content != "f.txt:1: a.b" {
		t.Fatal(r)
	}
	if r := dispatch(d, call("g", "grep", map[string]any{"pattern": "a.b", "regex": true}), tc); r.Content != "f.txt:1: a.b\nf.txt:2: axb" {
		t.Fatal(r)
	}
	for _, evil := range []string{"(a+)+$", "(a|aa)*b", "(x*)*y", `(a)\1`, strings.Repeat("a", 300)} {
		if r := dispatch(d, call("g", "grep", map[string]any{"pattern": evil, "regex": true}), tc); r.OK {
			t.Errorf("%q accepted", evil)
		}
	}
	if r := dispatch(d, call("g", "grep", map[string]any{"pattern": "zzz"}), tc); r.Content != "(no matches)" {
		t.Fatal(r)
	}
	if r := dispatch(d, call("g", "grep", map[string]any{"pattern": ""}), tc); r.ErrorText() != "pattern must not be empty" {
		t.Fatal(r)
	}
}

func TestDispatcherIsFailClosedByDefault(t *testing.T) {
	tmp := t.TempDir()
	tc := tctx(t, tmp)
	os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("x"), 0o644)
	closed, _ := execution.NewAllowListDispatcher(DefaultLocalTools(), execution.DispatcherOptions{})
	if len(closed.Specs()) != 0 {
		t.Fatal(closed.Specs())
	}
	if r := dispatch(closed, call("z1", "read_file", map[string]any{"path": "f.txt"}), tc); r.ErrorText() != "tool not allowed: 'read_file'" {
		t.Fatal(r)
	}
	named, _ := execution.NewAllowListDispatcher(DefaultLocalTools(), execution.DispatcherOptions{Allow: []string{"read_file", "write_file"}})
	if r := dispatch(named, call("z2", "read_file", map[string]any{"path": "f.txt"}), tc); !r.OK {
		t.Fatal(r)
	}
	if r := dispatch(named, call("z3", "write_file", map[string]any{"path": "g.txt", "content": "y"}), tc); r.OK {
		t.Fatal(r)
	}
	if _, err := os.Stat(filepath.Join(tmp, "g.txt")); err == nil {
		t.Fatal("wrote")
	}
}

func TestFSToolsDoNotLeakOutsideViaSymlinks(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	outside := filepath.Join(tmp, "outside")
	os.Mkdir(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOKEN=abc"), 0o644)
	tc := tctx(t, work)
	os.WriteFile(filepath.Join(work, "ok.txt"), []byte("TOKEN=inside"), 0o644)
	os.Symlink(outside, filepath.Join(work, "leak"))
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(work, "leak_file"))
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true})
	listing := dispatch(d, call("1", "list_files", map[string]any{}), tc)
	if !listing.OK || listing.Content != "ok.txt" {
		t.Fatal(listing)
	}
	grep := dispatch(d, call("2", "grep", map[string]any{"pattern": "TOKEN"}), tc)
	if !grep.OK || grep.Content != "ok.txt:1: TOKEN=inside" {
		t.Fatal(grep)
	}
	if sub := dispatch(d, call("3", "grep", map[string]any{"pattern": "TOKEN", "subdir": "leak"}), tc); sub.OK {
		t.Fatal(sub)
	}
	for _, c := range []contracts.ToolCall{
		call("1", "read_file", map[string]any{"path": "../x"}),
		call("2", "write_file", map[string]any{"path": "/tmp/x", "content": "y"}),
		call("3", "list_files", map[string]any{"subdir": "../"}),
		call("4", "grep", map[string]any{"pattern": "a", "subdir": ".."}),
	} {
		if r := dispatch(d, c, tc); r.OK {
			t.Errorf("%v: %v", c, r)
		}
	}
}

func TestListFilesOrderAndIgnores(t *testing.T) {
	tmp := t.TempDir()
	for _, p := range []string{"a/b", "a-c", "real/f", "node_modules/x.js", ".git/config", "z/__pycache__/m.pyc", ".hidden"} {
		os.MkdirAll(filepath.Join(tmp, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(tmp, p), []byte("x\r\ny\rz"), 0o644)
	}
	os.Symlink(filepath.Join(tmp, "real"), filepath.Join(tmp, "al"))
	os.Symlink(filepath.Join(tmp, "a-c"), filepath.Join(tmp, "link-in"))
	d := forTools(t, DefaultLocalTools(), execution.DispatcherOptions{})
	tc := tctx(t, tmp)
	r := dispatch(d, call("1", "list_files", map[string]any{}), tc)
	if r.Content != ".hidden\na/b\na-c\nlink-in\nreal/f" {
		t.Fatalf("%q", r.Content)
	}
	if r := dispatch(d, call("1", "list_files", map[string]any{"subdir": "real"}), tc); r.Content != "real/f" {
		t.Fatal(r)
	}
	if r := dispatch(d, call("1", "list_files", map[string]any{"subdir": "missing"}), tc); r.Content != "(no files)" {
		t.Fatal(r)
	}
	if r := dispatch(d, call("1", "grep", map[string]any{"pattern": "z", "subdir": "real"}), tc); r.Content != "real/f:3: z" {
		t.Fatalf("%q", r.Content)
	}
}
