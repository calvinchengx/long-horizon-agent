package egressproxy

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// The egress proxy's request log, as sandbox_egress events committed with each checkpoint
// (python: lha.execution.egress_events).
//
// The proxy logs one line per request it decides ("allow CONNECT pypi.org:443 -> 1.2.3.4:443",
// "deny CONNECT github.com:443: host not in egress allow-list: github.com", "fail ...: upstream
// unreachable"). The Docker sandbox session reads the proxy container's log
// (DockerSandboxSession.DrainEgressEvents) and the agent loop commits what it found with the
// cycle's checkpoint. Requests are aggregated per (decision, method, host, port) within one drain,
// with a count. Plain-HTTP targets are full URLs; only their host and port are kept.

// EgressEventKind is the kind of the events ParseProxyLog returns.
const EgressEventKind = "sandbox_egress"

var proxyLineRE = regexp.MustCompile(
	` (?:INFO|WARNING) (allow|deny|fail) ([A-Z]+) (\S+)(?: -> (\S+)|: (.*))$`)

func targetHostPort(method, target string) (string, int) {
	raw := target
	if method == "CONNECT" {
		raw = "//" + target
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "?", 0
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		host = "?"
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return "?", 0
		}
		return host, n
	}
	if u.Scheme == "https" {
		return host, 443
	}
	return host, 80
}

// ParseProxyLog is the sandbox_egress events for lines of the proxy log (other lines ignored).
func ParseProxyLog(lines []string) []contracts.EventRecord {
	type key struct {
		decision, method, host string
		port                   int
	}
	order := []key{}
	details := map[key]string{}
	counts := map[key]int{}
	for _, line := range lines {
		m := proxyLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		host, port := targetHostPort(m[2], m[3])
		k := key{m[1], m[2], host, port}
		if _, ok := details[k]; !ok {
			detail := m[4]
			if detail == "" {
				detail = obs.RedactText(m[5])
			}
			details[k] = detail
			order = append(order, k)
		}
		counts[k]++
	}
	events := make([]contracts.EventRecord, 0, len(order))
	for _, k := range order {
		events = append(events, contracts.EventRecord{Kind: EgressEventKind, Payload: contracts.Payload(
			"decision", k.decision, "method", k.method, "host", k.host, "port", k.port,
			"detail", details[k], "count", counts[k],
		)})
	}
	return events
}

// ProxyLogCursor remembers how much of a (growing) proxy log was already turned into events.
type ProxyLogCursor struct{ seen int }

// Drain is the events for the complete lines of log past the previous drain.
func (c *ProxyLogCursor) Drain(log string) []contracts.EventRecord {
	lines := strings.Split(log, "\n")
	complete := lines[:len(lines)-1] // the last element is "" or a line still being written
	var fresh []string
	if c.seen < len(complete) {
		fresh = complete[c.seen:]
		c.seen = len(complete)
	}
	return ParseProxyLog(fresh)
}
