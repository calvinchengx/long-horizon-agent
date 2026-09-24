package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const publicIP = "93.184.215.14"

func TestPythonSourceIsTheReferenceProxy(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	ref := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "python", "src", "lha", "execution", "egress_proxy.py")
	want, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	if PythonSource != string(want) {
		t.Fatal("egress_proxy.py drifted from python/src/lha/execution/egress_proxy.py: copy it again")
	}
	if strings.Contains(PythonSource, "from lha") || strings.Contains(PythonSource, "import lha") ||
		!strings.Contains(PythonSource, "class EgressProxy") {
		t.Fatal("the shipped proxy source must be self-contained")
	}
}

func TestParseAllowList(t *testing.T) {
	entries, err := ParseAllowList(SplitAllowList(" pypi.org, .golang.org\nexample.com:8443  Registry.NPMJS.org. "))
	if err != nil {
		t.Fatal(err)
	}
	want := []AllowEntry{{"pypi.org", false, 0}, {"golang.org", true, 0}, {"example.com", false, 8443},
		{"registry.npmjs.org", false, 0}}
	if len(entries) != len(want) {
		t.Fatalf("%v", entries)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("entry %d = %v, want %v", i, entries[i], want[i])
		}
	}
	if e, _ := ParseAllowList([]string{"a.org", ""}); len(e) != 1 || e[0] != (AllowEntry{"a.org", false, 0}) {
		t.Fatal(e)
	}
	if e, _ := ParseAllowList(SplitAllowList("")); len(e) != 0 {
		t.Fatal(e)
	}
	for _, bad := range []string{"10.0.0.1", "[::1]", "https://pypi.org", "pypi.org:0", "pypi.org:http", "a b/c", "."} {
		if _, err := ParseAllowList([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	_, err = ParseAllowList([]string{"10.0.0.1"})
	if err.Error() != "invalid egress allow-list entry '10.0.0.1' (expected a DNS name)" {
		t.Fatal(err)
	}
}

func mustDeny(t *testing.T, host string, port int, allow []AllowEntry, contains string) {
	t.Helper()
	_, err := CheckHost(host, port, allow)
	var d *Denied
	if !errors.As(err, &d) || !strings.Contains(d.Reason, contains) {
		t.Errorf("CheckHost(%q, %d) = %v, want denial containing %q", host, port, err, contains)
	}
}

func TestCheckHost(t *testing.T) {
	allow, _ := ParseAllowList(SplitAllowList("pypi.org,.golang.org"))
	for host, want := range map[string]string{"pypi.org": "pypi.org", "PyPI.org.": "pypi.org",
		"proxy.golang.org": "proxy.golang.org", "golang.org": "golang.org"} {
		if got, err := CheckHost(host, 443, allow); err != nil || got != want {
			t.Errorf("CheckHost(%q) = %q, %v", host, got, err)
		}
	}
	for _, host := range []string{"files.pypi.org", "evilpypi.org", "pypi.org.evil.com", "evilgolang.org"} {
		mustDeny(t, host, 443, allow, "not in egress allow-list")
	}
	allow, _ = ParseAllowList([]string{"pypi.org"})
	for _, host := range []string{"151.101.0.223", "::1", "[2a04:4e42::223]", "127.0.0.1"} {
		mustDeny(t, host, 443, allow, "IP-literal")
	}
	for _, host := range []string{"", "pypi.org\x00.evil", "pypi.org/x", "pÿpi.org"} {
		mustDeny(t, host, 443, allow, "")
	}
	allow, _ = ParseAllowList(SplitAllowList("pypi.org, example.com:8443"))
	if _, err := CheckHost("pypi.org", 80, allow); err != nil {
		t.Fatal(err)
	}
	mustDeny(t, "pypi.org", 22, allow, "port not allowed")
	if _, err := CheckHost("example.com", 8443, allow); err != nil {
		t.Fatal(err)
	}
	mustDeny(t, "example.com", 443, allow, "port not allowed")
}

func staticResolver(table map[string][]string, calls *[]string, mu *sync.Mutex) func(context.Context, string, int) ([]string, error) {
	return func(_ context.Context, host string, _ int) ([]string, error) {
		if calls != nil {
			mu.Lock()
			*calls = append(*calls, host)
			mu.Unlock()
		}
		addrs, ok := table[host]
		if !ok {
			return nil, errors.New("NXDOMAIN")
		}
		return addrs, nil
	}
}

func TestResolveChecked(t *testing.T) {
	r := staticResolver(map[string][]string{"ok.org": {publicIP}, "mixed.org": {publicIP, "10.0.0.5"}, "none.org": {}}, nil, nil)
	if got, err := ResolveChecked(context.Background(), "ok.org", 443, r); err != nil || got[0] != publicIP {
		t.Fatal(got, err)
	}
	for host, want := range map[string]string{
		"mixed.org":   "mixed.org resolves to a non-public address: 10.0.0.5",
		"none.org":    "none.org did not resolve",
		"missing.org": "cannot resolve missing.org",
	} {
		if _, err := ResolveChecked(context.Background(), host, 443, r); err == nil || err.Error() != want {
			t.Errorf("%s: %v", host, err)
		}
	}
	addrs, err := SystemResolver(context.Background(), "localhost", 80)
	if err != nil || len(addrs) == 0 {
		t.Fatal(addrs, err)
	}
}

type harness struct {
	port     int
	mu       sync.Mutex
	connects []string
	resolved []string
	received bytes.Buffer
	proxy    *Proxy
}

// newHarness starts the proxy with a fake DNS and a dialer that records the chosen address and
// redirects to a local upstream (echo, or one canned HTTP response).
func newHarness(t *testing.T, mode string) *harness {
	t.Helper()
	h := &harness{}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upstream.Close() })
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if mode == "echo" {
					_, _ = io.Copy(c, c)
					return
				}
				r := bufio.NewReader(c)
				var head bytes.Buffer
				for {
					line, err := r.ReadString('\n')
					head.WriteString(line)
					if err != nil || line == "\r\n" {
						break
					}
				}
				h.mu.Lock()
				h.received.Write(head.Bytes())
				h.mu.Unlock()
				body := "hello from upstream"
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) +
					"\r\nConnection: close\r\n\r\n" + body))
			}(conn)
		}
	}()
	upstreamAddr := upstream.Addr().String()
	allow, _ := ParseAllowList(SplitAllowList("pypi.org,.example.org"))
	h.proxy = &Proxy{
		Allow: allow,
		Resolver: staticResolver(map[string][]string{
			"pypi.org":             {publicIP},
			"internal.example.org": {"10.0.0.5"},
			"rebind.example.org":   {publicIP, "127.0.0.1"},
		}, &h.resolved, &h.mu),
		Dialer: func(ctx context.Context, address string, port int) (net.Conn, error) {
			h.mu.Lock()
			h.connects = append(h.connects, address+":"+strconv.Itoa(port))
			h.mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstreamAddr)
		},
	}
	_, port, err := h.proxy.Start("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.proxy.Close() })
	h.port = port
	return h
}

func exchange(t *testing.T, port int, request string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	var head strings.Builder
	for {
		line, err := r.ReadString('\n')
		head.WriteString(line)
		if err != nil || line == "\r\n" {
			break
		}
	}
	return conn, r, head.String()
}

func deniedBody(t *testing.T, port int, request string) (string, string) {
	t.Helper()
	conn, r, head := exchange(t, port, request)
	defer conn.Close()
	body, _ := io.ReadAll(r)
	status, _, _ := strings.Cut(head, "\r\n")
	return status, string(body)
}

func TestConnectIsTunnelledToTheCheckedAddress(t *testing.T) {
	h := newHarness(t, "echo")
	conn, r, head := exchange(t, h.port, "CONNECT pypi.org:443 HTTP/1.1\r\nHost: pypi.org:443\r\n\r\n")
	defer conn.Close()
	if head != "HTTP/1.1 200 Connection Established\r\n\r\n" {
		t.Fatalf("%q", head)
	}
	if _, err := conn.Write([]byte("tls-bytes")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 9)
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "tls-bytes" {
		t.Fatal(string(buf), err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.connects) != 1 || h.connects[0] != publicIP+":443" || len(h.resolved) != 1 || h.resolved[0] != "pypi.org" {
		t.Fatal(h.connects, h.resolved)
	}
}

func TestDenials(t *testing.T) {
	h := newHarness(t, "echo")
	status, body := deniedBody(t, h.port, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	if status != "HTTP/1.1 403 Forbidden" || body != "lha egress proxy: host not in egress allow-list: example.com\n" {
		t.Fatalf("%q %q", status, body)
	}
	status, body = deniedBody(t, h.port, "CONNECT internal.example.org:443 HTTP/1.1\r\n\r\n")
	if status != "HTTP/1.1 403 Forbidden" || !strings.Contains(body, "non-public address: 10.0.0.5") {
		t.Fatal(status, body)
	}
	status, body = deniedBody(t, h.port, "CONNECT rebind.example.org:443 HTTP/1.1\r\n\r\n")
	if status != "HTTP/1.1 403 Forbidden" || !strings.Contains(body, "127.0.0.1") {
		t.Fatal(status, body)
	}
	for _, c := range []struct{ line, reason string }{
		{"CONNECT 127.0.0.1:443 HTTP/1.1", "IP-literal"},
		{"CONNECT [::1]:443 HTTP/1.1", "IP-literal"},
		{"CONNECT pypi.org:22 HTTP/1.1", "port not allowed"},
		{"CONNECT pypi.org HTTP/1.1", "malformed authority"},
		{"GET https://pypi.org/simple/ HTTP/1.1", "only CONNECT and absolute http://"},
		{"GET /simple/ HTTP/1.1", "only CONNECT and absolute http://"},
		{"GET http://user:pw@pypi.org/ HTTP/1.1", "credentials"},
		{"GET http://169.254.169.254/latest/meta-data/ HTTP/1.1", "IP-literal"},
	} {
		status, body := deniedBody(t, h.port, c.line+"\r\nHost: x\r\n\r\n")
		if status != "HTTP/1.1 403 Forbidden" || !strings.Contains(body, c.reason) {
			t.Errorf("%s: %q %q", c.line, status, body)
		}
	}
	if status, _ := deniedBody(t, h.port, "NONSENSE\r\n\r\n"); status != "HTTP/1.1 400 Bad Request" {
		t.Fatal(status)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.connects) != 0 {
		t.Fatal(h.connects)
	}
}

func TestPlainHTTPIsForwardedInOriginForm(t *testing.T) {
	h := newHarness(t, "http")
	conn, r, head := exchange(t, h.port, "GET http://pypi.org/simple/six/?q=1 HTTP/1.1\r\nHost: evil.internal\r\n"+
		"Proxy-Authorization: Basic eA==\r\nAccept: */*\r\n\r\n")
	defer conn.Close()
	body, _ := io.ReadAll(r)
	if !strings.HasPrefix(head, "HTTP/1.1 200 OK") || string(body) != "hello from upstream" {
		t.Fatalf("%q %q", head, body)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sent := h.received.String()
	want := "GET /simple/six/?q=1 HTTP/1.1\r\nHost: pypi.org\r\nAccept: */*\r\nConnection: close\r\n\r\n"
	if sent != want {
		t.Fatalf("%q", sent)
	}
	if len(h.connects) != 1 || h.connects[0] != publicIP+":80" {
		t.Fatal(h.connects)
	}
}

func TestUnreachableUpstreamIs502(t *testing.T) {
	allow, _ := ParseAllowList([]string{"pypi.org"})
	p := &Proxy{
		Allow:    allow,
		Resolver: staticResolver(map[string][]string{"pypi.org": {publicIP}}, nil, nil),
		Dialer: func(context.Context, string, int) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		},
	}
	_, port, err := p.Start("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	status, body := deniedBody(t, port, "CONNECT pypi.org:443 HTTP/1.1\r\n\r\n")
	want := "lha egress proxy: upstream unreachable: [Errno " + strconv.Itoa(int(syscall.ECONNREFUSED)) +
		"] Connect call failed ('" + publicIP + "', 443)\n"
	if status != "HTTP/1.1 502 Bad Gateway" || body != want {
		t.Fatalf("%q %q", status, body)
	}
}

func TestServeEnv(t *testing.T) {
	env := map[string]string{"LHA_PROXY_ALLOW": "pypi.org", "LHA_PROXY_PORT": "0", "LHA_PROXY_BIND": "127.0.0.1"}
	lines := make(chan string, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeEnv(ctx, func(k string) string { return env[k] }, func(level, msg string) { lines <- level + " " + msg })
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("no ready line")
	}
	m := regexp.MustCompile(`^INFO lha-egress-proxy listening on 127\.0\.0\.1:(\d+) allow=pypi\.org$`).FindStringSubmatch(line)
	if m == nil {
		t.Fatal(line)
	}
	port, _ := strconv.Atoi(m[1])
	status, body := deniedBody(t, port, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	if status != "HTTP/1.1 403 Forbidden" || !strings.Contains(body, "example.com") {
		t.Fatal(status, body)
	}
	if l := <-lines; l != "WARNING deny CONNECT example.com:443: host not in egress allow-list: example.com" {
		t.Fatal(l)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
