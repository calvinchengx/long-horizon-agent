// Package serve is lha serve: the UI API (spec/serve/openapi.json) over the mission store,
// anchors and Temporal (python: lha.serve.app).
//
// Every route reads shared contracts only: the store schema (missions, cost_ledger, hitl_gates,
// mission_events), the .lha/ anchor at a mission's recorded workdir, and the workflow's queries
// and signals (the wire contract). So this server shows any implementation's missions, and any
// implementation's server shows this one's (docs/27-mission-ui.md).
//
// Performance: one shared reader (hub) follows mission_events and the mission rows and fans out
// to every stream; Temporal queries are cached per mission for a few seconds; nothing polls per
// client.
//
// Security: the server binds to loopback, refuses a Host that is not its own loopback address (DNS
// rebinding), and needs the start-up token on every request: the X-LHA-Token header, or for reads
// the lha_token cookie the start-up URL sets (SameSite=Strict, HttpOnly).
package serve

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/calvinchengx/long-horizon-agent/go/internal/checklistedit"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// APIVersion is spec/serve/openapi.json's info.version.
const APIVersion = "1.1.0"

// Implementation names this implementation in /api/v1/health.
const Implementation = "go"

const (
	liveTTL                = 3 * time.Second // a mission's live state is reused this long
	temporalRetry          = 5 * time.Second // a failed connection is not retried sooner
	temporalConnectTimeout = 3 * time.Second
	keepaliveDefault       = "15" // seconds a quiet stream waits before a comment (LHA_SERVE_KEEPALIVE_S)
	pollEvents             = 500 * time.Millisecond
	pollMissions           = 2 * time.Second
	streamQueue            = 1000
	maxSnoozeSeconds       = 31_536_000
	maxWhoChars            = 200
	eventPage              = 500
	missionsWatched        = 1000
	diffCacheSize          = 128     // attempt diffs one process caches (keyed by workdir+head)
	maxDiffBytes           = 100_000 // an attempt diff longer than this is cut and flagged truncated
)

var decisions = []string{"approve", "reject", "retry", "abort", "impossible"}

// editFields are each checklist edit op's fields and their kinds (spec/state/checklist_edit.json).
var editFields = map[string]map[string]string{
	"add": {"description": "str", "id": "str", "witnesses": "list", "depends_on": "list",
		"after": "str", "allow_harness_edits": "bool", "notes": "str"},
	"remove": {"id": "str"},
	"edit": {"id": "str", "description": "str", "witnesses": "list", "depends_on": "list",
		"allow_harness_edits": "bool", "notes": "str"},
	"reopen":  {"id": "str"},
	"block":   {"id": "str"},
	"unblock": {"id": "str"},
}

// apiError is an error response.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func fail(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// Server is everything the routes share.
type Server struct {
	Settings *config.Settings
	Store    persistence.Store
	Token    string
	Port     int
	Version  string // the lha version /api/v1/health reports

	temporal  temporalState
	hub       *hub
	keepalive time.Duration

	diffMu    sync.Mutex
	diffCache map[string]string // (workdir, head) -> the attempt's diff (bounded; git never reruns)
}

// Route is one API route (a test checks they are exactly spec/serve/openapi.json's operations).
type Route struct {
	Method, Path string
	handle       func(*Server, http.ResponseWriter, *http.Request) error
	write        bool
}

// Routes are every API route.
var Routes = []Route{
	{"GET", "/api/v1/health", (*Server).health, false},
	{"GET", "/api/v1/missions", (*Server).listMissions, false},
	{"GET", "/api/v1/missions/{mission_id}", (*Server).getMission, false},
	{"GET", "/api/v1/missions/{mission_id}/items", (*Server).listItems, false},
	{"GET", "/api/v1/missions/{mission_id}/items/{item_id}", (*Server).getItem, false},
	{"GET", "/api/v1/missions/{mission_id}/items/{item_id}/diffs", (*Server).listItemDiffs, false},
	{"GET", "/api/v1/missions/{mission_id}/events", (*Server).listEvents, false},
	{"GET", "/api/v1/missions/{mission_id}/costs", (*Server).listCosts, false},
	{"GET", "/api/v1/missions/{mission_id}/gates", (*Server).listGates, false},
	{"GET", "/api/v1/stream", (*Server).stream, false},
	{"POST", "/api/v1/missions/{mission_id}/steer", (*Server).steer, true},
	{"POST", "/api/v1/missions/{mission_id}/snooze", (*Server).snooze, true},
	{"POST", "/api/v1/missions/{mission_id}/checklist-edits", (*Server).editChecklist, true},
	{"POST", "/api/v1/missions/{mission_id}/decision", (*Server).decide, true},
	{"POST", "/api/v1/missions/{mission_id}/abort", (*Server).abort, true},
}

// Start begins following the store (the shared event reader); call it before serving.
func (s *Server) Start(ctx context.Context) error {
	secs, err := strconv.ParseFloat(cmp.Or(os.Getenv("LHA_SERVE_KEEPALIVE_S"), keepaliveDefault), 64)
	if err != nil || secs <= 0 {
		return fmt.Errorf("LHA_SERVE_KEEPALIVE_S must be a positive number of seconds")
	}
	s.keepalive = time.Duration(secs * float64(time.Second))
	s.hub = newHub(s)
	return s.hub.start(ctx)
}

// Handler is the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range Routes {
		r := r
		mux.HandleFunc(r.Method+" "+r.Path, func(w http.ResponseWriter, req *http.Request) {
			err := s.guard(req, r.write)
			if err == nil {
				err = r.handle(s, w, req)
			}
			if err != nil {
				writeError(w, err)
			}
		})
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, req *http.Request) {
		if err := s.guard(req, false); err != nil {
			writeError(w, err)
			return
		}
		writeError(w, fail(404, "not_found", "no route %s", req.URL.Path))
	})
	mux.HandleFunc("GET /{$}", s.appPage)
	mux.HandleFunc("GET /missions/", s.appPage)
	mux.HandleFunc("GET /assets/{name}", s.asset)
	mux.HandleFunc("POST /mcp", s.mcpEndpoint)
	return mux
}

func writeError(w http.ResponseWriter, err error) {
	var e *apiError
	if !errors.As(err, &e) {
		e = fail(500, "internal", "%v", err)
	}
	writeJSON(w, e.status, map[string]any{"error": map[string]any{"code": e.code, "message": e.message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// --- security ----------------------------------------------------------------------------------

func (s *Server) guardHost(r *http.Request) error {
	if r.Host != fmt.Sprintf("127.0.0.1:%d", s.Port) && r.Host != fmt.Sprintf("localhost:%d", s.Port) {
		return fail(403, "forbidden_host", "Host %s is not this server", contracts.PyRepr(r.Host))
	}
	return nil
}

func (s *Server) validToken(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) == 1
}

func (s *Server) guard(r *http.Request, write bool) error {
	if err := s.guardHost(r); err != nil {
		return err
	}
	token, ok := r.Header["X-Lha-Token"]
	value := ""
	if ok && len(token) > 0 {
		value = token[0]
	} else if !write {
		if c, err := r.Cookie("lha_token"); err == nil {
			value, ok = c.Value, true
		}
	}
	if !ok || !s.validToken(value) {
		return fail(401, "unauthorized", "a valid X-LHA-Token header is required")
	}
	return nil
}

// uiFiles is the UI bundle (built from ui/ by ui/bundle.sh; the Python server ships the same files).
//
//go:embed ui
var uiFiles embed.FS

const tokenMeta = `<meta name="lha-token" content="" />`

var assetName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// pageHeaders: the page and its assets come from this server only; nothing may frame it; the
// start-up URL's token never leaves in a Referer.
func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
		"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

const signIn = "<!doctype html><title>LHA</title><p>Open the URL <code>lha serve</code> printed " +
	"(it carries the token) to use this page.</p>"

// appPage is the UI (/, /missions/...): the start-up URL's ?token= sets the cookie; the page carries
// the token for the UI's writes (X-LHA-Token), only to a browser that already has it.
func (s *Server) appPage(w http.ResponseWriter, r *http.Request) {
	if err := s.guardHost(r); err != nil {
		writeError(w, err)
		return
	}
	fromURL := r.URL.Query().Get("token")
	urlOK := fromURL != "" && s.validToken(fromURL)
	cookieOK := false
	if c, err := r.Cookie("lha_token"); err == nil {
		cookieOK = s.validToken(c.Value)
	}
	pageHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !urlOK && !cookieOK {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, signIn)
		return
	}
	page := signIn
	if raw, err := uiFiles.ReadFile("ui/index.html"); err == nil {
		page = string(raw)
	}
	page = strings.Replace(page, tokenMeta, `<meta name="lha-token" content="`+html.EscapeString(s.Token)+`" />`, 1)
	w.Header().Set("Cache-Control", "no-store")
	if urlOK {
		http.SetCookie(w, &http.Cookie{Name: "lha_token", Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	}
	_, _ = io.WriteString(w, page)
}

// asset is a file of the UI bundle; its name carries its content hash, so it is cached for good.
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	if err := s.guardHost(r); err != nil {
		writeError(w, err)
		return
	}
	name := r.PathValue("name")
	raw, err := uiFiles.ReadFile("ui/assets/" + name)
	if !assetName.MatchString(name) || err != nil {
		writeError(w, fail(404, "not_found", "no asset %s", contracts.PyRepr(name)))
		return
	}
	kind := map[string]string{".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml"}[path.Ext(name)]
	pageHeaders(w)
	w.Header().Set("Content-Type", cmp.Or(kind, "application/octet-stream"))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(raw)
}

// --- parameters --------------------------------------------------------------------------------

func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	raw, ok := r.URL.Query()[name]
	if !ok {
		return def, nil
	}
	v, err := strconv.Atoi(raw[0])
	if err != nil {
		return 0, fail(422, "invalid_request", "%s must be an integer", name)
	}
	if v < lo || (hi >= 0 && v > hi) {
		if hi >= 0 {
			return 0, fail(422, "invalid_request", "%s must be between %d and %d", name, lo, hi)
		}
		return 0, fail(422, "invalid_request", "%s must be at least %d", name, lo)
	}
	return v, nil
}

// --- reading the store and anchors -------------------------------------------------------------

func (s *Server) mission(ctx context.Context, id string) (*persistence.MissionRow, error) {
	row, err := s.Store.GetMission(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fail(404, "not_found", "no mission %s", contracts.PyRepr(id))
	}
	return row, nil
}

// items is the mission's checklist from its anchor, or nil when it cannot be read here.
func items(ctx context.Context, row *persistence.MissionRow) []map[string]any {
	if row.Workdir == "" {
		return nil
	}
	if info, err := os.Stat(filepath.Join(row.Workdir, state.AnchorDir)); err != nil || !info.IsDir() {
		return nil
	}
	checklist, err := state.NewGitMissionAnchor(row.Workdir).ReadChecklist(ctx)
	if err != nil {
		return nil
	}
	// Each item as the JSON object it is in the anchor (plain fields: this cannot fail).
	out := make([]map[string]any, 0, len(checklist.Items))
	raw, _ := json.Marshal(checklist.Items)
	_ = json.Unmarshal(raw, &out)
	return out
}

func counts(items []map[string]any) any {
	if items == nil {
		return nil
	}
	c := map[string]int{"total": len(items), "todo": 0, "in_progress": 0, "blocked": 0, "done": 0, "split": 0}
	for _, item := range items {
		if status, _ := item["status"].(string); status != "total" {
			if _, known := c[status]; known {
				c[status]++
			}
		}
	}
	return c
}

func spend(c persistence.CostSummary) map[string]any {
	return map[string]any{
		"calls": c.Calls, "known_usd": c.KnownUSD, "unknown_cost_calls": c.UnknownCostCalls,
		"input_tokens": c.InputTokens, "output_tokens": c.OutputTokens,
	}
}

func event(e persistence.EventRow) map[string]any {
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	return map[string]any{
		"id": e.ID, "mission_id": e.MissionID, "cycle_id": e.CycleID, "ts": e.TS, "kind": e.Kind,
		"payload": payload, "schema_version": e.SchemaVersion,
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullablePtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func gate(g persistence.GateRow) map[string]any {
	options := g.Options
	if options == nil {
		options = []string{}
	}
	var request any
	if g.Request != nil {
		request = g.Request
	}
	return map[string]any{
		"gate_id": g.GateID, "kind": g.Kind, "status": g.Status, "question": g.Question,
		"options": options, "default_action": g.DefaultAction, "risk": g.Risk, "deadline": g.Deadline,
		"decision": nullablePtr(g.Decision), "resolved_by": nullablePtr(g.ResolvedBy),
		"reminders": g.Reminders, "request": request, "opened_at": g.OpenedAt, "resolved_at": g.ResolvedAt,
	}
}

func terminal(status string) bool {
	for _, t := range persistence.TerminalStatuses {
		if status == t {
			return true
		}
	}
	return false
}

func (s *Server) summary(ctx context.Context, row *persistence.MissionRow, its []map[string]any, haveItems bool) (map[string]any, error) {
	cost, err := s.Store.CostSummary(ctx, row.MissionID)
	if err != nil {
		return nil, err
	}
	last, err := s.Store.LastMissionEvent(ctx, row.MissionID)
	if err != nil {
		return nil, err
	}
	if !haveItems {
		its = items(ctx, row)
	}
	var lastEvent any
	if last != nil {
		lastEvent = map[string]any{"id": last.ID, "ts": last.TS, "kind": last.Kind}
	}
	return map[string]any{
		"mission_id": row.MissionID, "title": row.Title, "status": row.Status,
		"durable": row.WorkflowID != "", "workflow_id": nullable(row.WorkflowID),
		"head_sha": nullable(row.HeadSHA), "created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
		"spend": spend(cost), "items": counts(its), "last_event": lastEvent,
	}, nil
}

// --- reads -------------------------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) error {
	backend, degraded := s.Store.Backend(), s.Store.DegradedReason()
	writeJSON(w, 200, map[string]any{
		"api_version": APIVersion, "implementation": Implementation, "lha_version": s.Version,
		"store":    map[string]any{"backend": backend, "degraded_reason": degraded},
		"temporal": s.temporal.status(r.Context(), s.Settings),
	})
	return nil
}

func (s *Server) listMissions(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 50, 1, 500)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListMissions(r.Context(), limit)
	if err != nil {
		return err
	}
	out := make([]map[string]any, len(rows))
	var wg sync.WaitGroup
	errs := make([]error, len(rows))
	for i := range rows {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i], errs[i] = s.summary(r.Context(), &rows[i], nil, false)
		}(i)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"missions": out})
	return nil
}

func (s *Server) getMission(w http.ResponseWriter, r *http.Request) error {
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return err
	}
	detail, err := s.summary(r.Context(), row, items(r.Context(), row), true)
	if err != nil {
		return err
	}
	var live, liveError any
	if row.WorkflowID != "" && !terminal(row.Status) {
		state, why := s.temporal.live(r.Context(), s.Settings, row.WorkflowID)
		if state != nil {
			live = state
		}
		if why != "" {
			liveError = why
		}
	}
	detail["description"] = row.Description
	detail["workdir"] = nullable(row.Workdir)
	detail["live"] = live
	detail["live_error"] = liveError
	writeJSON(w, 200, detail)
	return nil
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) error {
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return err
	}
	its := items(r.Context(), row)
	if its == nil {
		return fail(404, "anchor_unavailable", "the anchor at %s cannot be read here", contracts.PyRepr(row.Workdir))
	}
	writeJSON(w, 200, map[string]any{"items": its})
	return nil
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) error {
	after, err := intParam(r, "after", 0, 0, -1)
	if err != nil {
		return err
	}
	limit, err := intParam(r, "limit", 500, 1, 1000)
	if err != nil {
		return err
	}
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return err
	}
	rows, err := s.Store.ReadMissionEvents(r.Context(), row.MissionID, int64(after), limit)
	if err != nil {
		return err
	}
	events := make([]map[string]any, len(rows))
	next := int64(after)
	for i, e := range rows {
		events[i] = event(e)
		next = e.ID
	}
	writeJSON(w, 200, map[string]any{"events": events, "next_after": next})
	return nil
}

// ledgerAll is "all of a mission's cost ledger" for the routes that sum it.
const ledgerAll = 1_000_000

func (s *Server) getItem(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 200, 1, 1000)
	if err != nil {
		return err
	}
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return err
	}
	its := items(r.Context(), row)
	if its == nil {
		return fail(404, "anchor_unavailable", "the anchor at %s cannot be read here", contracts.PyRepr(row.Workdir))
	}
	itemID := r.PathValue("item_id")
	var item map[string]any
	dependents := []string{}
	for _, it := range its {
		if it["id"] == itemID {
			item = it
		}
		deps, _ := it["depends_on"].([]any)
		if slices.Contains(deps, any(itemID)) {
			dependents = append(dependents, it["id"].(string))
		}
	}
	if item == nil {
		return fail(404, "not_found", "no item %s in %s", contracts.PyRepr(itemID), contracts.PyRepr(row.MissionID))
	}
	events, err := s.allEvents(r.Context(), row.MissionID)
	if err != nil {
		return err
	}
	ledger, err := s.Store.ListCosts(r.Context(), row.MissionID, ledgerAll)
	if err != nil {
		return err
	}
	cycles := cyclesFor(events, itemID)
	var about []persistence.EventRow
	for _, e := range events {
		if slices.Contains(cycles, e.CycleID) || slices.Contains(named(e.Payload, true), itemID) {
			about = append(about, e)
		}
	}
	var spent []persistence.CostRow
	for _, c := range ledger {
		if slices.Contains(cycles, c.CycleID) {
			spent = append(spent, c)
		}
	}
	recent := about[max(0, len(about)-limit):]
	out := make([]map[string]any, len(recent))
	for i, e := range recent {
		out[i] = event(e)
	}
	writeJSON(w, 200, map[string]any{
		"item": item, "witnesses": witnessResults(item, about), "dependents": dependents,
		"cycles": cycles, "spend": sumCosts(spent, ""), "events": out, "events_total": len(about),
	})
	return nil
}

// cyclesFor is the item's cycles: the non-empty cycle_ids of the events whose payload names the
// item, in the order first recorded (the rule getItem documents; python: lha.serve.app._cycles_for).
func cyclesFor(events []persistence.EventRow, itemID string) []string {
	cycles := []string{}
	for _, e := range events {
		if e.CycleID != "" && !slices.Contains(cycles, e.CycleID) && slices.Contains(named(e.Payload, false), itemID) {
			cycles = append(cycles, e.CycleID)
		}
	}
	return cycles
}

// listItemDiffs is each of the item's failed attempts, from refs/lha/attempts/<mission>/<cycle>.
//
// An attempt ref exists only when the cycle changed files outside .lha/; its parent is the
// pre-attempt HEAD, so the diff is <ref>^..<ref> excluding .lha/. The diff is cached by
// workdir+head: git never runs per request (python: lha.serve.app.list_item_diffs).
func (s *Server) listItemDiffs(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 200, 1, 1000)
	if err != nil {
		return err
	}
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return err
	}
	its := items(r.Context(), row)
	if its == nil {
		return fail(404, "anchor_unavailable", "the anchor at %s cannot be read here", contracts.PyRepr(row.Workdir))
	}
	itemID := r.PathValue("item_id")
	found := false
	for _, it := range its {
		if it["id"] == itemID {
			found = true
		}
	}
	if !found {
		return fail(404, "not_found", "no item %s in %s", contracts.PyRepr(itemID), contracts.PyRepr(row.MissionID))
	}
	events, err := s.allEvents(r.Context(), row.MissionID)
	if err != nil {
		return err
	}
	attempts := []map[string]any{}
	for _, cycleID := range cyclesFor(events, itemID) {
		if a := s.attempt(r.Context(), row.Workdir, row.MissionID, cycleID); a != nil {
			attempts = append(attempts, a)
		}
	}
	if len(attempts) > limit {
		attempts = attempts[len(attempts)-limit:]
	}
	writeJSON(w, 200, map[string]any{"attempts": attempts})
	return nil
}

// attempt is the failed attempt kept at refs/lha/attempts/<mission>/<cycle>, or nil when there is
// none. head is the ref's commit, base its parent (the pre-attempt HEAD); the diff excludes .lha/
// and is cut at maxDiffBytes with truncated set when it was longer (python: _attempt).
func (s *Server) attempt(ctx context.Context, workdir, missionID, cycleID string) map[string]any {
	ref := "refs/lha/attempts/" + missionID + "/" + cycleID
	head, err := state.RunGitWith(ctx, workdir, state.RunOptions{NoCheck: true}, "rev-parse", "--verify", "--quiet", ref)
	if err != nil || head == "" {
		return nil
	}
	base, err := state.RunGitWith(ctx, workdir, state.RunOptions{NoCheck: true}, "rev-parse", "--verify", "--quiet", head+"^")
	if err != nil || base == "" {
		return nil
	}
	diff := s.cachedDiff(ctx, workdir, head, base)
	truncated := false
	if len(diff) > maxDiffBytes {
		diff = strings.ToValidUTF8(diff[:maxDiffBytes], "")
		truncated = true
	}
	return map[string]any{"cycle_id": cycleID, "base": base, "head": head, "diff": diff, "truncated": truncated}
}

// cachedDiff is the attempt's diff, from the cache keyed by workdir+head or computed once (python:
// _cached_diff). Bounded: when full, one entry is dropped (an LRU is not worth the bookkeeping).
func (s *Server) cachedDiff(ctx context.Context, workdir, head, base string) string {
	key := workdir + "\x00" + head
	s.diffMu.Lock()
	if diff, ok := s.diffCache[key]; ok {
		s.diffMu.Unlock()
		return diff
	}
	s.diffMu.Unlock()
	diff := state.DiffRange(ctx, workdir, base, head, state.AnchorDir)
	s.diffMu.Lock()
	if s.diffCache == nil {
		s.diffCache = map[string]string{}
	}
	if len(s.diffCache) >= diffCacheSize {
		for k := range s.diffCache {
			delete(s.diffCache, k)
			break
		}
	}
	s.diffCache[key] = diff
	s.diffMu.Unlock()
	return diff
}

// witnessResults is the item's witnesses, in order, each with its latest result from a `verify`
// event in the item's cycles (nil when none ran). about is oldest first, so a later result wins
// (python: lha.serve.app._witness_results).
func witnessResults(item map[string]any, about []persistence.EventRow) []map[string]any {
	var names []string
	if ws, ok := item["witnesses"].([]any); ok {
		for _, w := range ws {
			if s, ok := w.(string); ok {
				names = append(names, s)
			}
		}
	}
	latest := map[string]map[string]any{}
	for _, e := range about {
		if e.Kind != "verify" {
			continue
		}
		checks, _ := e.Payload["checks"].([]any)
		for _, c := range checks {
			check, ok := c.(map[string]any)
			if !ok {
				continue
			}
			name, _ := check["name"].(string)
			if !slices.Contains(names, name) {
				continue
			}
			latest[name] = map[string]any{
				"passed":     check["passed"] == true,
				"exit_code":  int(numOr(check["exit_code"], 0)),
				"gating":     check["gating"] != false,
				"timed_out":  check["timed_out"] == true,
				"duration_s": numOr(check["duration_s"], 0),
			}
		}
	}
	seen := map[string]bool{}
	out := []map[string]any{}
	for _, w := range names {
		if seen[w] {
			continue
		}
		seen[w] = true
		var result any
		if r, ok := latest[w]; ok {
			result = r
		}
		out = append(out, map[string]any{"witness": w, "latest": result})
	}
	return out
}

// numOr is v as a float, or def for a missing/unsupported value (event payload numbers are
// json.Number).
func numOr(v any, def float64) float64 {
	if n, ok := v.(json.Number); ok {
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	return def
}

func (s *Server) allEvents(ctx context.Context, missionID string) ([]persistence.EventRow, error) {
	var events []persistence.EventRow
	for {
		after := int64(0)
		if len(events) > 0 {
			after = events[len(events)-1].ID
		}
		page, err := s.Store.ReadMissionEvents(ctx, missionID, after, 1000)
		if err != nil {
			return nil, err
		}
		events = append(events, page...)
		if len(page) < 1000 {
			return events, nil
		}
	}
}

// named is the item ids an event's payload names (its items too when withWaves).
func named(payload map[string]any, withWaves bool) []string {
	var out []string
	for _, k := range []string{"item_id", "item"} {
		if v, ok := payload[k].(string); ok {
			out = append(out, v)
		}
	}
	if waves, ok := payload["items"].([]any); ok && withWaves {
		for _, v := range waves {
			if id, ok := v.(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// sumCosts sums calls like the store's CostSummary: an unknown cost is counted, never $0.
func sumCosts(rows []persistence.CostRow, key string) map[string]any {
	known, unknown, in, out := 0.0, 0, 0, 0
	for _, c := range rows {
		if c.CostKnown && c.USD != nil {
			known += *c.USD
		} else {
			unknown++
		}
		in += c.InputTokens
		out += c.OutputTokens
	}
	m := map[string]any{
		"calls": len(rows), "known_usd": known, "unknown_cost_calls": unknown,
		"input_tokens": in, "output_tokens": out,
	}
	if key != "" {
		m["key"] = key
	}
	return m
}

func costsBy(rows []persistence.CostRow, field func(persistence.CostRow) string) []map[string]any {
	var keys []string
	groups := map[string][]persistence.CostRow{}
	for _, c := range rows {
		k := field(c)
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], c)
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, sumCosts(groups[k], k))
	}
	slices.SortStableFunc(out, func(a, b map[string]any) int {
		if c := cmp.Compare(b["known_usd"].(float64), a["known_usd"].(float64)); c != 0 {
			return c
		}
		return strings.Compare(a["key"].(string), b["key"].(string))
	})
	return out
}

func (s *Server) listCosts(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if err != nil {
		return err
	}
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return err
	}
	summary, err := s.Store.CostSummary(r.Context(), row.MissionID)
	if err != nil {
		return err
	}
	ledger, err := s.Store.ListCosts(r.Context(), row.MissionID, ledgerAll)
	if err != nil {
		return err
	}
	rows := ledger[max(0, len(ledger)-limit):]
	calls := make([]map[string]any, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- { // most recent first
		c := rows[i]
		var usd any
		if c.CostKnown && c.USD != nil {
			usd = *c.USD
		}
		calls = append(calls, map[string]any{
			"cycle_id": c.CycleID, "role": c.Role, "model": c.Model, "input_tokens": c.InputTokens,
			"output_tokens": c.OutputTokens, "usd": usd, "ts": c.TS,
		})
	}
	writeJSON(w, 200, map[string]any{
		"summary":  spend(summary),
		"by_role":  costsBy(ledger, func(c persistence.CostRow) string { return c.Role }),
		"by_model": costsBy(ledger, func(c persistence.CostRow) string { return c.Model }),
		"calls":    calls,
	})
	return nil
}

func (s *Server) listGates(w http.ResponseWriter, r *http.Request) error {
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if err != nil {
		return err
	}
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return err
	}
	rows, err := s.Store.ListGates(r.Context(), row.MissionID, limit)
	if err != nil {
		return err
	}
	gates := make([]map[string]any, len(rows))
	for i, g := range rows {
		gates[i] = gate(g)
	}
	writeJSON(w, 200, map[string]any{"gates": gates})
	return nil
}

// --- the stream --------------------------------------------------------------------------------

func (s *Server) stream(w http.ResponseWriter, r *http.Request) error {
	missionID := r.URL.Query().Get("mission_id")
	after, err := intParam(r, "after", -1, 0, -1)
	if err != nil {
		return err
	}
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		v, err := strconv.Atoi(last)
		if err != nil || v < 0 {
			return fail(422, "invalid_request", "Last-Event-ID must be an event id")
		}
		after = v
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming is not supported")
	}
	sub := s.hub.subscribe(missionID)
	defer s.hub.unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()
	ctx := r.Context()
	sent := s.hub.cursorNow()
	if after >= 0 { // replay what was recorded after the cursor, then follow
		sent = int64(after)
		for {
			rows, err := s.Store.ReadMissionEvents(ctx, missionID, sent, eventPage)
			if err != nil {
				return nil
			}
			for _, e := range rows {
				if writeSSE(w, "mission_event", event(e), &e.ID) != nil {
					return nil
				}
				sent = e.ID
			}
			flusher.Flush()
			if len(rows) < eventPage {
				break
			}
		}
	}
	ticker := time.NewTicker(s.keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case m, open := <-sub.queue:
			if !open {
				return nil // dropped for falling behind: the client resumes
			}
			if m.kind == "mission_event" {
				id := m.data["id"].(int64)
				if id <= sent {
					continue
				}
				sent = id
				if writeSSE(w, m.kind, m.data, &id) != nil {
					return nil
				}
			} else if writeSSE(w, m.kind, m.data, nil) != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, kind string, data map[string]any, id *int64) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return err
	}
	head := ""
	if id != nil {
		head = fmt.Sprintf("id: %d\n", *id)
	}
	_, err := fmt.Fprintf(w, "%sevent: %s\ndata: %s\n\n", head, kind, bytes.TrimSpace(b.Bytes()))
	return err
}

type message struct {
	kind string
	data map[string]any
}

type subscriber struct {
	missionID string
	queue     chan message
}

// hub is one reader that follows mission_events and the mission rows for every stream.
type hub struct {
	s       *Server
	mu      sync.Mutex
	subs    map[*subscriber]bool
	cursor  int64
	updated map[string]string
}

func newHub(s *Server) *hub {
	return &hub{s: s, subs: map[*subscriber]bool{}, updated: map[string]string{}}
}

func (h *hub) start(ctx context.Context) error {
	// Start at the newest event: a stream replays older ones itself (after).
	for {
		rows, err := h.s.Store.ReadMissionEvents(ctx, "", h.cursor, missionsWatched)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		h.cursor = rows[len(rows)-1].ID
	}
	rows, err := h.s.Store.ListMissions(ctx, missionsWatched)
	if err != nil {
		return err
	}
	for _, row := range rows {
		h.updated[row.MissionID] = row.UpdatedAt
	}
	go h.run(ctx)
	return nil
}

func (h *hub) cursorNow() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cursor
}

func (h *hub) subscribe(missionID string) *subscriber {
	sub := &subscriber{missionID: missionID, queue: make(chan message, streamQueue)}
	h.mu.Lock()
	h.subs[sub] = true
	h.mu.Unlock()
	return sub
}

func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[sub] {
		delete(h.subs, sub)
		close(sub.queue)
	}
}

func (h *hub) publish(missionID string, m message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if sub.missionID != "" && sub.missionID != missionID {
			continue
		}
		select {
		case sub.queue <- m:
		default: // too slow: it reconnects and resumes from Last-Event-ID
			delete(h.subs, sub)
			close(sub.queue)
		}
	}
}

func (h *hub) run(ctx context.Context) {
	var lastMissions time.Time
	for {
		rows, err := h.s.Store.ReadMissionEvents(ctx, "", h.cursorNow(), eventPage)
		if err == nil {
			for _, e := range rows {
				h.publish(e.MissionID, message{"mission_event", event(e)})
				h.mu.Lock()
				h.cursor = e.ID
				h.mu.Unlock()
			}
		}
		if time.Since(lastMissions) >= pollMissions {
			lastMissions = time.Now()
			h.missions(ctx)
		}
		if err != nil || len(rows) < eventPage {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollEvents):
			}
		}
	}
}

func (h *hub) missions(ctx context.Context) {
	h.mu.Lock()
	idle := len(h.subs) == 0
	h.mu.Unlock()
	if idle {
		return
	}
	rows, err := h.s.Store.ListMissions(ctx, missionsWatched)
	if err != nil {
		return
	}
	for i := range rows {
		row := &rows[i]
		if h.updated[row.MissionID] == row.UpdatedAt {
			continue
		}
		h.updated[row.MissionID] = row.UpdatedAt
		if summary, err := h.s.summary(ctx, row, nil, false); err == nil {
			h.publish(row.MissionID, message{"mission", summary})
		}
	}
}

// --- Temporal: one client, live state cached per mission ---------------------------------------

type liveEntry struct {
	at    time.Time
	state map[string]any
	err   string
}

type temporalState struct {
	mu       sync.Mutex
	client   client.Client
	failedAt time.Time
	err      string
	cache    map[string]liveEntry
}

func (t *temporalState) connect(ctx context.Context, settings *config.Settings) (client.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		return t.client, nil
	}
	if time.Since(t.failedAt) < temporalRetry {
		return nil, fail(503, "temporal_unavailable", "%s", t.err)
	}
	dctx, cancel := context.WithTimeout(ctx, temporalConnectTimeout)
	defer cancel()
	cl, err := durable.Dial(dctx, settings, nil)
	if err != nil {
		t.failedAt = time.Now()
		t.err = pyfmt.Head(fmt.Sprintf("Temporal at %s is unreachable: %v", settings.TemporalAddress, err), 500)
		return nil, fail(503, "temporal_unavailable", "%s", t.err)
	}
	t.client = cl
	return cl, nil
}

func (t *temporalState) status(ctx context.Context, settings *config.Settings) string {
	if _, err := t.connect(ctx, settings); err != nil {
		return "unavailable"
	}
	return "connected"
}

func (t *temporalState) live(ctx context.Context, settings *config.Settings, workflowID string) (map[string]any, string) {
	t.mu.Lock()
	if e, ok := t.cache[workflowID]; ok && time.Since(e.at) < liveTTL {
		t.mu.Unlock()
		return e.state, e.err
	}
	t.mu.Unlock()
	var state map[string]any
	why := ""
	cl, err := t.connect(ctx, settings)
	if err == nil {
		state, err = queryLive(ctx, cl, workflowID)
	}
	if err != nil {
		var e *apiError
		if errors.As(err, &e) {
			why = e.message
		} else {
			why = pyfmt.Head(err.Error(), 500)
		}
		state = nil
	}
	t.mu.Lock()
	if t.cache == nil {
		t.cache = map[string]liveEntry{}
	}
	t.cache[workflowID] = liveEntry{time.Now(), state, why}
	t.mu.Unlock()
	return state, why
}

func (t *temporalState) forget(workflowID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.cache, workflowID)
}

// query runs a query into out; optional queries a workflow does not answer leave out unchanged.
func query(ctx context.Context, cl client.Client, workflowID, name string, out any, optional bool) error {
	v, err := cl.QueryWorkflow(ctx, workflowID, "", name)
	if err != nil {
		var failed *serviceerror.QueryFailed
		if optional && errors.As(err, &failed) {
			return nil
		}
		return err
	}
	if !v.HasValue() {
		return nil
	}
	return v.Get(out)
}

func queryLive(ctx context.Context, cl client.Client, workflowID string) (map[string]any, error) {
	var status any
	var cycles int
	if err := query(ctx, cl, workflowID, durable.QueryStatus, &status, false); err != nil {
		return nil, err
	}
	if err := query(ctx, cl, workflowID, durable.QueryCycles, &cycles, false); err != nil {
		return nil, err
	}
	var gateView *durable.GateView
	var question *string
	var resumeAt *float64
	var notes []string
	var pending int
	for _, q := range []struct {
		name string
		out  any
	}{
		{durable.QueryGate, &gateView}, {durable.QueryOpenQuestion, &question}, {durable.QueryResumeAt, &resumeAt},
		{durable.QuerySteerNotes, &notes}, {durable.QueryPendingEdits, &pending},
	} {
		if err := query(ctx, cl, workflowID, q.name, q.out, true); err != nil {
			return nil, err
		}
	}
	if notes == nil {
		notes = []string{}
	}
	var open, resume any
	if question != nil && *question != "" {
		open = *question
	}
	if resumeAt != nil && *resumeAt != 0 {
		resume = isoformat(time.Unix(0, int64(*resumeAt*float64(time.Second))).UTC())
	}
	return map[string]any{
		"status": pyfmt.PyStr(status), "cycles": cycles, "gate": openGate(gateView),
		"open_question": open, "resume_at": resume, "steer_notes": notes, "pending_edits": pending,
	}, nil
}

func openGate(g *durable.GateView) any {
	if g == nil {
		return nil
	}
	options := g.Options
	if options == nil {
		options = []string{}
	}
	var request any
	if g.Request != nil {
		request = map[string]any{"tool": g.Request.Tool, "arguments": g.Request.Arguments,
			"reason": g.Request.Reason, "fingerprint": g.Request.Fingerprint}
	}
	return map[string]any{
		"gate_id": g.GateID, "kind": g.Kind, "question": g.Question, "options": options,
		"default_action": g.DefaultAction, "opened_at": g.OpenedAt, "deadline": g.Deadline,
		"escalations_sent": g.EscalationsSent, "next_escalation_at": g.NextEscalationAt,
		"recommended": g.Recommended, "request": request,
	}
}

// --- controls ----------------------------------------------------------------------------------

func body(r *http.Request, allowed ...string) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fail(422, "invalid_request", "cannot read the body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fail(422, "invalid_request", "the body is not JSON")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fail(422, "invalid_request", "the body must be a JSON object")
	}
	if err := onlyFields(m, allowed...); err != nil {
		return nil, err
	}
	return m, nil
}

func onlyFields(m map[string]any, allowed ...string) error {
	var extra []string
	for k := range m {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
			}
		}
		if !ok {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		return fail(422, "invalid_request", "unexpected field(s): %s", strings.Join(extra, ", "))
	}
	return nil
}

func (s *Server) durableMission(r *http.Request) (*persistence.MissionRow, error) {
	row, err := s.mission(r.Context(), r.PathValue("mission_id"))
	if err != nil {
		return nil, err
	}
	if row.WorkflowID == "" {
		return nil, fail(409, "not_durable", "a local run has no control channel")
	}
	if terminal(row.Status) {
		return nil, fail(409, "finished", "the mission has finished (%s)", row.Status)
	}
	return row, nil
}

func temporalError(err error) error {
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return fail(409, "finished", "the mission's workflow is not running")
	}
	return fail(503, "temporal_unavailable", "%s", pyfmt.Head(err.Error(), 500))
}

func (s *Server) signal(w http.ResponseWriter, r *http.Request, row *persistence.MissionRow, name string, arg any) error {
	cl, err := s.temporal.connect(r.Context(), s.Settings)
	if err != nil {
		return err
	}
	if err := cl.SignalWorkflow(r.Context(), row.WorkflowID, "", name, arg); err != nil {
		return temporalError(err)
	}
	s.temporal.forget(row.WorkflowID)
	writeJSON(w, 202, map[string]any{"accepted": true})
	return nil
}

func (s *Server) steer(w http.ResponseWriter, r *http.Request) error {
	row, err := s.durableMission(r)
	if err != nil {
		return err
	}
	b, err := body(r, "note")
	if err != nil {
		return err
	}
	note, ok := b["note"].(string)
	if !ok || pyfmt.PyStrip(note) == "" || len([]rune(note)) > durable.MaxSteerChars {
		return fail(422, "invalid_request", "note must be 1-%d characters", durable.MaxSteerChars)
	}
	return s.signal(w, r, row, durable.SignalSteer, pyfmt.PyStrip(note))
}

func (s *Server) snooze(w http.ResponseWriter, r *http.Request) error {
	row, err := s.durableMission(r)
	if err != nil {
		return err
	}
	b, err := body(r, "seconds")
	if err != nil {
		return err
	}
	n, ok := b["seconds"].(json.Number)
	seconds, convErr := n.Int64()
	if !ok || convErr != nil || seconds < 0 || seconds > maxSnoozeSeconds {
		return fail(422, "invalid_request", "seconds must be an integer from 0 to %d", maxSnoozeSeconds)
	}
	return s.signal(w, r, row, durable.SignalSnooze, seconds)
}

func checkEdit(n int, raw any) (map[string]any, error) {
	edit, ok := raw.(map[string]any)
	op, _ := edit["op"].(string)
	fields, known := editFields[op]
	if !ok || !known {
		return nil, fail(422, "invalid_request", "edit %d: op must be one of add, remove, edit, reopen, block, unblock", n)
	}
	allowed := []string{"op"}
	for k := range fields {
		allowed = append(allowed, k)
	}
	if err := onlyFields(edit, allowed...); err != nil {
		return nil, err
	}
	required := "id"
	if op == "add" {
		required = "description"
	}
	if v, ok := edit[required].(string); !ok || v == "" {
		return nil, fail(422, "invalid_request", "edit %d: %s is required", n, required)
	}
	for key, kind := range fields {
		v, present := edit[key]
		if !present || v == nil {
			continue
		}
		valid := false
		switch kind {
		case "str":
			_, valid = v.(string)
		case "bool":
			_, valid = v.(bool)
		case "list":
			list, isList := v.([]any)
			valid = isList
			for _, item := range list {
				if _, isStr := item.(string); !isStr {
					valid = false
				}
			}
		}
		if !valid {
			return nil, fail(422, "invalid_request", "edit %d: %s has the wrong type", n, key)
		}
	}
	return edit, nil
}

func (s *Server) editChecklist(w http.ResponseWriter, r *http.Request) error {
	row, err := s.durableMission(r)
	if err != nil {
		return err
	}
	b, err := body(r, "edits", "by")
	if err != nil {
		return err
	}
	edits, ok := b["edits"].([]any)
	if !ok || len(edits) < 1 || len(edits) > checklistedit.MaxEditOps {
		return fail(422, "invalid_request", "edits must be a list of 1-%d edits", checklistedit.MaxEditOps)
	}
	for i, e := range edits {
		if _, err := checkEdit(i+1, e); err != nil {
			return err
		}
	}
	by := ""
	if v, present := b["by"]; present {
		str, isStr := v.(string)
		if !isStr || len([]rune(str)) > maxWhoChars {
			return fail(422, "invalid_request", "by must be at most %d characters", maxWhoChars)
		}
		by = str
	}
	return s.signal(w, r, row, durable.SignalChecklistEdit, map[string]any{"edits": edits, "by": by})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) error {
	row, err := s.durableMission(r)
	if err != nil {
		return err
	}
	b, err := body(r, "decision", "by")
	if err != nil {
		return err
	}
	decision, _ := b["decision"].(string)
	known := false
	for _, d := range decisions {
		if decision == d {
			known = true
		}
	}
	if !known {
		return fail(422, "invalid_request", "decision must be one of %s", strings.Join(decisions, ", "))
	}
	by, ok := b["by"].(string)
	if !ok || pyfmt.PyStrip(by) == "" || len([]rune(by)) > maxWhoChars {
		return fail(422, "invalid_request", "by (who answers) must be 1-%d characters", maxWhoChars)
	}
	cl, err := s.temporal.connect(r.Context(), s.Settings)
	if err != nil {
		return err
	}
	var gateView *durable.GateView
	if err := query(r.Context(), cl, row.WorkflowID, durable.QueryGate, &gateView, true); err != nil {
		return temporalError(err)
	}
	if gateView != nil {
		allowed := false
		for _, o := range gateView.Options {
			if o == decision {
				allowed = true
			}
		}
		if !allowed {
			return fail(422, "invalid_request", "the open gate takes %s, not %s", strings.Join(gateView.Options, ", "), contracts.PyRepr(decision))
		}
	}
	return s.signal(w, r, row, durable.SignalHumanDecisionV2, map[string]any{"decision": decision, "by": pyfmt.PyStrip(by)})
}

func (s *Server) abort(w http.ResponseWriter, r *http.Request) error {
	row, err := s.durableMission(r)
	if err != nil {
		return err
	}
	if _, err := body(r); err != nil {
		return err
	}
	cl, err := s.temporal.connect(r.Context(), s.Settings)
	if err != nil {
		return err
	}
	if err := cl.CancelWorkflow(r.Context(), row.WorkflowID, ""); err != nil {
		return temporalError(err)
	}
	s.temporal.forget(row.WorkflowID)
	writeJSON(w, 202, map[string]any{"accepted": true})
	return nil
}

// isoformat is Python's datetime.isoformat for a UTC time (microseconds only when non-zero).
func isoformat(t time.Time) string {
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}
