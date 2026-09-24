// Package egressproxy is a tiny allow-list HTTP forward proxy: the egress gate for the Docker
// sandbox (python/src/lha/execution/egress_proxy.py).
//
// When the operator lists egress hosts (LHA_SANDBOX_EGRESS), the sandbox container is attached
// ONLY to an --internal Docker network (no route out), and this proxy — running in its own
// container on both that network and the default bridge — is the one way out. Package managers
// honour HTTP(S)_PROXY; anything that ignores it simply has no route. The policy:
//
//   - a host is allowed iff it equals an allow-list entry, or is a subdomain of an entry written
//     with a leading dot (.golang.org allows proxy.golang.org and golang.org);
//   - IP-literal hosts are always denied (the allow-list names hosts, not addresses);
//   - the host is resolved and denied if ANY address is non-public (safety.IsPublicAddress);
//   - the proxy connects to the address it checked, never re-resolving (no DNS-rebinding window);
//   - ports 443 and 80 are allowed; any other port only via an explicit host:port entry;
//   - CONNECT (HTTPS, end-to-end TLS) and absolute-URI plain HTTP requests are supported;
//     everything else is refused. Denials get 403 + a short reason.
//
// One log line per request (allowed/denied) goes to the logger (stderr by default).
//
// The Docker sandbox runs the byte-identical Python source (PythonSource) in a python image by
// default, exactly like the Python implementation; this Go port is the same proxy for
// deployments that run a Go binary instead (ServeEnv reads the same LHA_PROXY_* environment;
// wire it to a command such as `lha egress-proxy` and pass that as DockerOptions.ProxyCommand).
package egressproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// DefaultPort is the proxy's default listening port.
const DefaultPort = 3128

// ReadyMessage is logged once the proxy listens (the Docker sandbox waits for it).
const ReadyMessage = "lha-egress-proxy listening"

const (
	maxHeaderBytes = 64 * 1024
	headerTimeout  = 30 * time.Second
	connectTimeout = 10 * time.Second
	pipeChunk      = 65_536
)

var hostRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,62})(\.[a-z0-9_]([a-z0-9_-]{0,62}))*$`)

var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "proxy-connection": true, "te": true, "trailer": true,
	"upgrade": true,
}

func isDefaultPort(port int) bool { return port == 80 || port == 443 }

// Denied is a request the policy refuses; Error() is the short reason sent back with the 403.
type Denied struct{ Reason string }

func (d *Denied) Error() string { return d.Reason }

func deny(format string, args ...any) error { return &Denied{Reason: fmt.Sprintf(format, args...)} }

// AllowEntry is one allow-list entry.
type AllowEntry struct {
	Host   string // normalized, without the leading dot
	Suffix bool   // true for ".example.org" (the domain and all subdomains)
	Port   int    // 0 = the default ports (80/443)
}

// MatchesHost reports whether host (normalized) is covered by the entry.
func (e AllowEntry) MatchesHost(host string) bool {
	return host == e.Host || (e.Suffix && strings.HasSuffix(host, "."+e.Host))
}

// String renders the entry as it would be written ("." prefix for suffix entries, ":port").
func (e AllowEntry) String() string {
	s := e.Host
	if e.Suffix {
		s = "." + s
	}
	if e.Port != 0 {
		s += ":" + strconv.Itoa(e.Port)
	}
	return s
}

func cleanHost(host string) string {
	host = strings.Trim(pystr.Strip(host), "[]")
	return pystr.Lower(strings.TrimRight(host, "."))
}

func isIPLiteral(host string) bool {
	h, _, _ := strings.Cut(host, "%")
	return safety.IsIPLiteral(h)
}

// SplitAllowList splits "pypi.org, .golang.org example.com:8443" on commas and whitespace.
func SplitAllowList(spec string) []string {
	return strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || pystr.IsSpace(r) })
}

// ParseAllowList parses allow-list entries. It returns a ValueError-typed error for an entry that
// is not a plain DNS name (an IP literal, a URL, a bad port) so a typo is loud instead of
// silently allowing nothing (or too much).
func ParseAllowList(items []string) ([]AllowEntry, error) {
	entries := []AllowEntry{}
	for _, raw := range items {
		item := pystr.Strip(raw)
		if item == "" {
			continue
		}
		port := 0
		host := item
		if i := strings.LastIndex(item, ":"); i >= 0 {
			portS := item[i+1:]
			host = item[:i]
			n, err := strconv.Atoi(portS)
			if portS == "" || strings.TrimLeft(portS, "0123456789") != "" || err != nil || n <= 0 || n >= 65536 {
				return nil, pyval.NewError("ValueError", "invalid port in egress allow-list entry "+contracts.PyRepr(raw))
			}
			port = n
		}
		suffix := strings.HasPrefix(host, ".")
		host = cleanHost(strings.TrimLeft(host, "."))
		if host == "" || isIPLiteral(host) || !hostRE.MatchString(host) {
			return nil, pyval.NewError("ValueError",
				"invalid egress allow-list entry "+contracts.PyRepr(raw)+" (expected a DNS name)")
		}
		entries = append(entries, AllowEntry{Host: host, Suffix: suffix, Port: port})
	}
	return entries, nil
}

// CheckHost is the pure name/port policy: it returns the normalized host, or a *Denied.
func CheckHost(host string, port int, allow []AllowEntry) (string, error) {
	name := cleanHost(host)
	if name == "" {
		return "", deny("missing host")
	}
	if isIPLiteral(name) {
		return "", deny("IP-literal hosts are not allowed: %s", name)
	}
	if !pystr.IsASCII(name) || !hostRE.MatchString(name) {
		return "", deny("invalid host: %s", contracts.PyRepr(host))
	}
	var matching []AllowEntry
	for _, e := range allow {
		if e.MatchesHost(name) {
			matching = append(matching, e)
		}
	}
	if len(matching) == 0 {
		return "", deny("host not in egress allow-list: %s", name)
	}
	for _, e := range matching {
		if (e.Port == 0 && isDefaultPort(port)) || e.Port == port {
			return name, nil
		}
	}
	return "", deny("port not allowed for %s: %d", name, port)
}

// SystemResolver resolves host with the OS resolver (all families, de-duplicated, v4 first).
func SystemResolver(ctx context.Context, host string, _ int) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var v4, v6 []string
	for _, a := range addrs {
		s := a.IP.String()
		if a.Zone != "" {
			s += "%" + a.Zone
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		if a.IP.To4() != nil {
			v4 = append(v4, s)
		} else {
			v6 = append(v6, s)
		}
	}
	return append(v4, v6...), nil
}

// ResolveChecked resolves host and returns its addresses, or a *Denied unless ALL are public.
func ResolveChecked(ctx context.Context, host string, port int, resolve safety.Resolver) ([]string, error) {
	addresses, err := resolve(ctx, host, port)
	if err != nil {
		return nil, deny("cannot resolve %s", host)
	}
	if len(addresses) == 0 {
		return nil, deny("%s did not resolve", host)
	}
	for _, a := range addresses {
		if !safety.IsPublicAddress(a) {
			return nil, deny("%s resolves to a non-public address: %s", host, a)
		}
	}
	return addresses, nil
}

// Dialer opens a TCP connection to address:port (tests inject a fake).
type Dialer func(ctx context.Context, address string, port int) (net.Conn, error)

// TCPDialer dials address:port over TCP.
func TCPDialer(ctx context.Context, address string, port int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
}

// Logger receives one line per request; level is "INFO", "WARNING" or "ERROR".
type Logger func(level, message string)

// StderrLogger logs like Python's logging.basicConfig format "%(asctime)s %(levelname)s
// %(message)s" to stderr.
func StderrLogger(level, message string) {
	fmt.Fprintf(os.Stderr, "%s %s %s\n", time.Now().Format("2006-01-02 15:04:05,000"), level, message)
}

// Proxy is the allow-list proxy server. Resolver / Dialer / Log are injectable.
type Proxy struct {
	Allow    []AllowEntry
	Resolver safety.Resolver
	Dialer   Dialer
	Log      Logger

	mu       sync.Mutex
	listener net.Listener
}

// New returns a proxy for the allow-list items (see ParseAllowList).
func New(items []string) (*Proxy, error) {
	allow, err := ParseAllowList(items)
	if err != nil {
		return nil, err
	}
	return &Proxy{Allow: allow, Resolver: SystemResolver, Dialer: TCPDialer, Log: StderrLogger}, nil
}

// Start listens on host:port (port 0 picks a free one) and serves in the background; it returns
// the bound host and port.
func (p *Proxy) Start(host string, port int) (string, int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return "", 0, err
	}
	p.mu.Lock()
	p.listener = ln
	p.mu.Unlock()
	go p.serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port, nil
}

// Close stops listening (in-flight connections finish on their own).
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener == nil {
		return nil
	}
	err := p.listener.Close()
	p.listener = nil
	return err
}

func (p *Proxy) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go p.handle(conn)
	}
}

func (p *Proxy) log(level, format string, args ...any) {
	if p.Log != nil {
		p.Log(level, fmt.Sprintf(format, args...))
	}
}

type request struct {
	method, target, version string
	headers                 [][2]string
}

func latin1(b []byte) string {
	rs := make([]rune, len(b))
	for i, c := range b {
		rs[i] = rune(c)
	}
	return string(rs)
}

func toLatin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			r = '?'
		}
		out = append(out, byte(r))
	}
	return out
}

// readHead reads the request head (up to the blank line); it returns nil for a malformed,
// oversized or timed-out head, and the bytes read past the head.
func readHead(conn net.Conn) (*request, []byte) {
	_ = conn.SetReadDeadline(time.Now().Add(headerTimeout))
	defer conn.SetReadDeadline(time.Time{})
	var buf []byte
	chunk := make([]byte, 4096)
	sep := []byte("\r\n\r\n")
	for {
		if i := bytes.Index(buf, sep); i >= 0 {
			if i > maxHeaderBytes {
				return nil, nil
			}
			return parseHead(buf[:i+len(sep)]), buf[i+len(sep):]
		}
		if len(buf)+1-len(sep) > maxHeaderBytes {
			return nil, nil
		}
		n, err := conn.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil && n == 0 {
			if i := bytes.Index(buf, sep); i >= 0 && i <= maxHeaderBytes {
				return parseHead(buf[:i+len(sep)]), buf[i+len(sep):]
			}
			return nil, nil
		}
	}
}

func parseHead(raw []byte) *request {
	lines := strings.Split(latin1(raw), "\r\n")
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 {
		return nil
	}
	req := &request{method: pystr.Upper(parts[0]), target: parts[1], version: parts[2]}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil
		}
		req.headers = append(req.headers, [2]string{pystr.Strip(name), pystr.Strip(value)})
	}
	return req
}

// splitAuthority parses a CONNECT target "host:port" (brackets for IPv6).
func splitAuthority(authority string) (string, int, error) {
	parts, err := urlsplit("//" + authority)
	var port int
	var hasPort bool
	if err == nil {
		port, hasPort, err = parts.port()
	}
	if err != nil {
		return "", 0, deny("malformed authority: %s", contracts.PyRepr(authority))
	}
	host := parts.hostname()
	if host == "" || !hasPort || parts.path != "" || parts.hasUsername() {
		return "", 0, deny("malformed authority: %s", contracts.PyRepr(authority))
	}
	return host, port, nil
}

func absoluteTarget(target string) (string, int, error) {
	parts, err := urlsplit(target)
	var port int
	if err == nil {
		port, _, err = parts.port()
	}
	if err != nil {
		return "", 0, deny("malformed url: %s", contracts.PyRepr(target))
	}
	if strings.ToLower(parts.scheme) != "http" {
		return "", 0, deny("only CONNECT and absolute http:// requests are proxied")
	}
	if parts.hasUsername() || parts.hasPassword() {
		return "", 0, deny("credentials in the url are not allowed")
	}
	host := parts.hostname()
	if host == "" {
		return "", 0, deny("url has no host: %s", contracts.PyRepr(target))
	}
	if port == 0 {
		port = 80
	}
	return host, port, nil
}

func originRequest(req *request, host string, port int) []byte {
	parts, _ := urlsplit(req.target)
	path := parts.path
	if path == "" {
		path = "/"
	}
	if parts.query != "" {
		path += "?" + parts.query
	}
	lines := []string{req.method + " " + path + " " + req.version}
	if port == 80 {
		lines = append(lines, "Host: "+host)
	} else {
		lines = append(lines, "Host: "+host+":"+strconv.Itoa(port))
	}
	for _, h := range req.headers {
		lowered := pystr.Lower(h[0])
		if lowered == "host" || hopByHop[lowered] {
			continue
		}
		lines = append(lines, h[0]+": "+h[1])
	}
	lines = append(lines, "Connection: close")
	return toLatin1(strings.Join(lines, "\r\n") + "\r\n\r\n")
}

func respond(conn net.Conn, status, reason string) {
	body := []byte(reason + "\n")
	head := fmt.Sprintf("HTTP/1.1 %s\r\nContent-Type: text/plain; charset=utf-8\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n\r\n", status, len(body))
	_, _ = conn.Write(append([]byte(head), body...))
}

// connectErrorText renders a failed connect like asyncio's OSError(errno, "Connect call failed
// ('addr', port)"); a timeout is TimeoutError() (empty text).
func connectErrorText(err error, address string, port int) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return ""
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		addr := "(" + contracts.PyRepr(address) + ", " + strconv.Itoa(port) + ")"
		if strings.Contains(address, ":") {
			addr = "(" + contracts.PyRepr(address) + ", " + strconv.Itoa(port) + ", 0, 0)"
		}
		return fmt.Sprintf("[Errno %d] Connect call failed %s", int(errno), addr)
	}
	return err.Error()
}

// openUpstream connects to the first reachable CHECKED address (never re-resolving the name).
func (p *Proxy) openUpstream(addresses []string, port int) (net.Conn, string, error) {
	dial := p.Dialer
	if dial == nil {
		dial = TCPDialer
	}
	last := ""
	for _, address := range addresses {
		ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		conn, err := dial(ctx, address, port)
		cancel()
		if err != nil {
			last = connectErrorText(err, address, port)
			continue
		}
		return conn, address, nil
	}
	return nil, "", fmt.Errorf("upstream unreachable: %s", last)
}

type closeWriter interface{ CloseWrite() error }

func (p *Proxy) handle(conn net.Conn) {
	var upstream net.Conn
	defer func() {
		if r := recover(); r != nil { // never let one bad connection kill the server
			p.log("ERROR", "error: %v", r)
		}
		if upstream != nil {
			upstream.Close()
		}
		conn.Close()
	}()
	req, rest := readHead(conn)
	if req == nil {
		respond(conn, "400 Bad Request", "malformed request")
		return
	}
	label := req.method + " " + req.target
	var host, name string
	var port int
	var addresses []string
	var err error
	if req.method == "CONNECT" {
		host, port, err = splitAuthority(req.target)
	} else {
		host, port, err = absoluteTarget(req.target)
	}
	if err == nil {
		name, err = CheckHost(host, port, p.Allow)
	}
	if err == nil {
		resolve := p.Resolver
		if resolve == nil {
			resolve = SystemResolver
		}
		addresses, err = ResolveChecked(context.Background(), name, port, resolve)
	}
	if err != nil {
		p.log("WARNING", "deny %s: %s", label, err)
		respond(conn, "403 Forbidden", "lha egress proxy: "+err.Error())
		return
	}
	up, address, err := p.openUpstream(addresses, port)
	if err != nil {
		p.log("WARNING", "fail %s: %s", label, err)
		respond(conn, "502 Bad Gateway", "lha egress proxy: "+err.Error())
		return
	}
	upstream = up
	p.log("INFO", "allow %s -> %s:%d", label, address, port)
	if req.method == "CONNECT" {
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
	} else if _, err := upstream.Write(originRequest(req, name, port)); err != nil {
		p.log("ERROR", "error: %s", err)
		return
	}
	// The exchange is over when the upstream side ends; then stop relaying the client side.
	go func() {
		_, _ = io.Copy(upstream, io.MultiReader(bytes.NewReader(rest), conn))
		if cw, ok := upstream.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
	}()
	buf := make([]byte, pipeChunk)
	_, _ = io.CopyBuffer(conn, upstream, buf)
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

// ServeEnv runs the proxy the way the Python module's __main__ does: configuration from
// LHA_PROXY_ALLOW (comma/whitespace separated entries), LHA_PROXY_PORT (default 3128) and
// LHA_PROXY_BIND (default 0.0.0.0); ReadyMessage is logged once listening. It blocks until ctx
// is done.
func ServeEnv(ctx context.Context, getenv func(string) string, log Logger) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	if log == nil {
		log = StderrLogger
	}
	port := DefaultPort
	if raw := getenv("LHA_PROXY_PORT"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return pyval.NewError("ValueError", "invalid literal for int() with base 10: "+contracts.PyRepr(raw))
		}
		port = n
	}
	bind := getenv("LHA_PROXY_BIND")
	if bind == "" {
		bind = "0.0.0.0"
	}
	proxy, err := New(SplitAllowList(getenv("LHA_PROXY_ALLOW")))
	if err != nil {
		return err
	}
	proxy.Log = log
	host, bound, err := proxy.Start(bind, port)
	if err != nil {
		return err
	}
	defer proxy.Close()
	names := make([]string, len(proxy.Allow))
	for i, e := range proxy.Allow {
		names[i] = e.String()
	}
	entries := strings.Join(names, ",")
	if entries == "" {
		entries = "(nothing)"
	}
	log("INFO", fmt.Sprintf("%s on %s:%d allow=%s", ReadyMessage, host, bound, entries))
	<-ctx.Done()
	return nil
}
