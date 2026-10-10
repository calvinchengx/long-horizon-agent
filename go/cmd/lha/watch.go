package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// watchClear clears the screen and homes the cursor before each refresh.
const watchClear = "\x1b[2J\x1b[H"

// defaultWatchURL is the server base when --url and LHA_SERVE_URL are unset.
const defaultWatchURL = "http://127.0.0.1:8765"

// watchMission is the MissionDetail fields lha watch reads (spec/serve/openapi.json).
type watchMission struct {
	MissionID string      `json:"mission_id"`
	Title     string      `json:"title"`
	Status    string      `json:"status"`
	Durable   bool        `json:"durable"`
	Items     *watchItems `json:"items"`
	Spend     watchSpend  `json:"spend"`
	Live      *watchLive  `json:"live"`
	// LastEvent is MissionDetail.last_event: its id bounds the newest `limit` events.
	LastEvent *struct {
		ID int64 `json:"id"`
	} `json:"last_event"`
}

// watchItems is MissionDetail.items (ItemCounts); nil when the anchor cannot be read here.
type watchItems struct {
	Total      int `json:"total"`
	Todo       int `json:"todo"`
	InProgress int `json:"in_progress"`
	Blocked    int `json:"blocked"`
	Done       int `json:"done"`
	Split      int `json:"split"`
}

// watchSpend is MissionDetail.spend (CostSummary).
type watchSpend struct {
	Calls            int     `json:"calls"`
	KnownUSD         float64 `json:"known_usd"`
	UnknownCostCalls int     `json:"unknown_cost_calls"`
}

// watchLive is MissionDetail.live (LiveState); nil for a local run or a finished mission.
type watchLive struct {
	Gate *watchGate `json:"gate"`
}

// watchGate is the open gate (OpenGate).
type watchGate struct {
	GateID   string `json:"gate_id"`
	Question string `json:"question"`
}

// watchEvent is one MissionEvent.
type watchEvent struct {
	TS      string         `json:"ts"`
	CycleID string         `json:"cycle_id"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload"`
}

// resolveWatchTarget is python's resolve_target: (base, token) from a --url value and an explicit
// --token. The URL may be the `lha serve` start-up form with a ?token= query; an explicit token
// (--token or LHA_SERVE_TOKEN) wins over the query's.
func resolveWatchTarget(rawURL, token string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.TrimRight(rawURL, "/"), token
	}
	queryToken := u.Query().Get("token")
	u.RawQuery, u.Fragment = "", ""
	base := strings.TrimRight(u.String(), "/")
	if base == "" {
		base = defaultWatchURL
	}
	if token == "" {
		token = queryToken
	}
	return base, token
}

// watchSummary is python's _summary: the per-kind one-field summary shown after an event's kind.
func watchSummary(kind string, payload map[string]any) string {
	switch kind {
	case "tool_call":
		tool, _ := payload["tool"].(string)
		if ok, isBool := payload["ok"].(bool); isBool && !ok {
			return tool + " failed"
		}
		return tool
	case "cycle_started", "loop_detected":
		item, _ := payload["item_id"].(string)
		return item
	case "verify", "checkpoint", "review":
		verdict, _ := payload["verdict"].(string)
		return verdict
	case "session_progress":
		return "turns " + watchScalar(payload["turns"])
	case "parallel_wave":
		items, _ := payload["items"].([]any)
		parts := make([]string, 0, len(items))
		for _, item := range items {
			parts = append(parts, watchScalar(item))
		}
		return strings.Join(parts, ",")
	case "tool_approval":
		decision, _ := payload["decision"].(string)
		return decision
	default:
		if strings.HasPrefix(kind, "gate") {
			decision, _ := payload["decision"].(string)
			return decision
		}
		return ""
	}
}

// watchScalar renders one decoded JSON scalar the way python's str() does for the summary fields.
func watchScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return fmt.Sprintf("%v", x)
	}
}

// watchRender is the exact bytes `lha watch` prints for one render (python: watch_render).
// events are oldest first (the API's order); they print newest first.
func watchRender(m watchMission, events []watchEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s", m.MissionID, m.Status)
	if m.Durable {
		b.WriteString("  durable")
	}
	b.WriteString("\n")
	b.WriteString(m.Title)
	b.WriteString("\n")
	if m.Items == nil {
		b.WriteString("items -\n")
	} else {
		fmt.Fprintf(&b, "items %d/%d done, %d in progress, %d blocked, %d split\n",
			m.Items.Done, m.Items.Total, m.Items.InProgress, m.Items.Blocked, m.Items.Split)
	}
	fmt.Fprintf(&b, "spend $%.4f over %d calls", m.Spend.KnownUSD, m.Spend.Calls)
	if m.Spend.UnknownCostCalls > 0 {
		fmt.Fprintf(&b, " (%d unpriced)", m.Spend.UnknownCostCalls)
	}
	b.WriteString("\n")
	if m.Live != nil && m.Live.Gate != nil {
		fmt.Fprintf(&b, "gate %s: %s\n", m.Live.Gate.GateID, m.Live.Gate.Question)
	}
	b.WriteString("\nevents:\n")
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		cycle := e.CycleID
		if cycle == "" {
			cycle = "-"
		}
		fields := []string{e.TS, cycle, e.Kind}
		if s := watchSummary(e.Kind, e.Payload); s != "" {
			fields = append(fields, s)
		}
		b.WriteString("  " + strings.Join(fields, "  ") + "\n")
	}
	return b.String()
}

// decodeWatchJSON decodes a response with json.Number, so a payload's numbers keep their exact
// text (python: json.loads).
func decodeWatchJSON(r io.Reader, out any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return dec.Decode(out)
}

// watchFetch is python's watch_fetch: GET the mission and its newest `limit` events. A 404 or an
// unreachable server is a clean operator error.
func watchFetch(client *http.Client, base, token, missionID string, limit int) (watchMission, []watchEvent, error) {
	var mission watchMission
	var page struct {
		Events []watchEvent `json:"events"`
	}
	get := func(path string, out any) error {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("X-LHA-Token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("cannot reach the server at %s: %v", base, err)
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusNotFound:
			return fmt.Errorf("no mission %s", contracts.PyRepr(missionID))
		default:
			return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
		}
		return decodeWatchJSON(resp.Body, out)
	}
	id := url.PathEscape(missionID)
	if err := get("/api/v1/missions/"+id, &mission); err != nil {
		return mission, nil, err
	}
	// The events endpoint pages forward from `after` (exclusive), so the newest `limit` are those
	// after last_event.id - limit.
	after := 0
	if mission.LastEvent != nil && mission.LastEvent.ID > int64(limit) {
		after = int(mission.LastEvent.ID) - limit
	}
	if err := get(fmt.Sprintf("/api/v1/missions/%s/events?after=%d&limit=%d", id, after, limit), &page); err != nil {
		return mission, nil, err
	}
	events := page.Events
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return mission, events, nil
}

// isTerminal reports whether w is a character device (python: sys.stdout.isatty()).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// watch is lha watch: a refreshing terminal view of one mission (python: lha watch).
func (c *cli) watch(args []string) error {
	fs := c.newFlags("watch", commandHelpFor("watch")+
		"\n\nReads the same UI API as `lha serve` (spec/serve/openapi.json), so it works over SSH"+
		"\nwithout a browser. Without --once it clears the screen and refreshes every --interval"+
		"\nseconds until interrupted (Ctrl-C exits 0); a non-TTY stdout behaves as --once.")
	urlFlag := fs.String("url", "", "Server base (default: LHA_SERVE_URL, else http://127.0.0.1:8765); the serve start-up URL with its ?token= is accepted.")
	tokenFlag := fs.String("token", "", "API token (default: LHA_SERVE_TOKEN, else the URL's token).")
	interval := fs.Float64("interval", 2.0, "Seconds between refreshes.")
	limit := fs.Int("limit", 10, "Recent events shown.")
	once := fs.Bool("once", false, "Render once and exit (for non-TTY, tests, CI).")
	positional, err := c.parseInterleaved(fs, args, 1)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return &exitError{code: 2, message: "Usage: lha watch [OPTIONS] MISSION_ID\nTry 'lha watch --help' for help.\n\n" +
			"Error: Missing argument 'MISSION_ID'."}
	}
	if *limit < 1 {
		return rangeError("watch", "limit", *limit, 1, 0, false)
	}
	missionID := positional[0]
	base := *urlFlag
	if base == "" {
		base = os.Getenv("LHA_SERVE_URL")
	}
	if base == "" {
		base = defaultWatchURL
	}
	token := *tokenFlag
	if token == "" {
		token = os.Getenv("LHA_SERVE_TOKEN")
	}
	base, token = resolveWatchTarget(base, token)
	runOnce := *once || !isTerminal(c.stdout)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		if c.ctx.Err() != nil {
			return nil
		}
		mission, events, err := watchFetch(client, base, token, missionID, *limit)
		if err != nil {
			return fail(1, "%s", err)
		}
		if !runOnce {
			fmt.Fprint(c.stdout, watchClear)
		}
		fmt.Fprint(c.stdout, watchRender(mission, events))
		if f, ok := c.stdout.(interface{ Sync() error }); ok {
			_ = f.Sync()
		}
		if runOnce {
			return nil
		}
		select {
		case <-c.ctx.Done():
			return nil
		case <-time.After(time.Duration(*interval * float64(time.Second))):
		}
	}
}
