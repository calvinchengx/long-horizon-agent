package vendor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// Ported from python/tests/unit/test_large_missions.py (vendor) and test_web_pinning.py.

const publicA = "93.184.216.34"

func public(context.Context, string, int) ([]string, error) { return []string{publicA}, nil }

type page struct {
	status int
	header map[string]string
	body   string
}

// pages serves canned responses by URL (the httpx.MockTransport of the Python tests).
func pages(t *testing.T, routes map[string]page, seen *[]string) *http.Client {
	t.Helper()
	var mu sync.Mutex
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		if seen != nil {
			*seen = append(*seen, r.URL.String())
		}
		mu.Unlock()
		p, ok := routes[r.URL.String()]
		if !ok {
			p = page{status: 404}
		}
		h := http.Header{}
		for k, v := range p.header {
			h.Set(k, v)
		}
		return &http.Response{StatusCode: p.status, Status: strconv.Itoa(p.status) + " " + http.StatusText(p.status), Header: h,
			Body: io.NopCloser(strings.NewReader(p.body)), Request: r}, nil
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var fixedNow = func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

func TestVendorSnapshotsPagesWithAManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ref")
	client := pages(t, map[string]page{
		"https://docs.example.com/moved":      {302, map[string]string{"Location": "/docs/items"}, ""},
		"https://docs.example.com/docs/items": {200, map[string]string{"Content-Type": "text/html"}, "<html><p>Items API</p></html>"},
	}, nil)
	saved, err := VendorURLs(context.Background(), []string{"https://docs.example.com/moved"}, dir,
		Options{Client: client, Resolver: public, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if saved[0].Path != "docs.example.com/docs/items.html" || saved[0].TextPath != "docs.example.com/docs/items.html.txt" ||
		saved[0].URL != "https://docs.example.com/moved" || saved[0].Bytes != 29 || saved[0].FetchedAt != "2026-09-26T12:00:00+00:00" {
		t.Fatalf("%+v", saved[0])
	}
	text, _ := os.ReadFile(filepath.Join(dir, saved[0].TextPath))
	if string(text) != "Items API\n" {
		t.Fatalf("%q", text)
	}
	manifest, _ := os.ReadFile(filepath.Join(dir, Manifest))
	want := `{
  "files": [
    {
      "url": "https://docs.example.com/moved",
      "path": "docs.example.com/docs/items.html",
      "sha256": "` + saved[0].SHA256 + `",
      "bytes": 29,
      "content_type": "text/html",
      "fetched_at": "2026-09-26T12:00:00+00:00",
      "text_path": "docs.example.com/docs/items.html.txt"
    }
  ]
}
`
	if string(manifest) != want {
		t.Fatalf("manifest:\n%s", manifest)
	}
}

func TestVendorRefusesRedirectsOffTheNamedHosts(t *testing.T) {
	client := pages(t, map[string]page{
		"https://docs.example.com/a": {302, map[string]string{"Location": "https://evil.example/x"}, ""},
	}, nil)
	_, err := VendorURLs(context.Background(), []string{"https://docs.example.com/a"}, t.TempDir(),
		Options{Client: client, Resolver: public})
	var verr *Error
	if !errors.As(err, &verr) || verr.Msg != "https://evil.example/x: host not in egress allow-list: 'evil.example'" {
		t.Fatalf("%v", err)
	}
}

func TestVendorRefusesPrivateAddressesAndBadURLs(t *testing.T) {
	private := func(context.Context, string, int) ([]string, error) { return []string{"10.0.0.5"}, nil }
	_, err := VendorURLs(context.Background(), []string{"https://intranet.example/a"}, t.TempDir(), Options{Resolver: private})
	var verr *Error
	if !errors.As(err, &verr) || verr.Msg != "https://intranet.example/a: 'intranet.example' resolves to a non-public address: 10.0.0.5" {
		t.Fatalf("%v", err)
	}
	_, err = VendorURLs(context.Background(), []string{"ftp://x.example/a"}, t.TempDir(), Options{Resolver: public})
	if !IsEgressDenied(err) || err.Error() != "scheme not allowed: 'ftp'" {
		t.Fatalf("%v", err)
	}
	_, err = VendorURLs(context.Background(), []string{"https://docs.example.com:8443/a"}, t.TempDir(), Options{Resolver: public})
	if !errors.As(err, &verr) || verr.Msg != "https://docs.example.com:8443/a: port not allowed: 8443" {
		t.Fatalf("%v", err)
	}
}

func TestVendorSizeRedirectAndStatusErrors(t *testing.T) {
	loop := map[string]page{}
	for i := 0; i < 7; i++ {
		loop["https://docs.example.com/"+string(rune('a'+i))] = page{301, map[string]string{"Location": "/" + string(rune('a'+i+1))}, ""}
	}
	loop["https://docs.example.com/big"] = page{200, nil, strings.Repeat("x", MaxBytes+1)}
	loop["https://docs.example.com/gone"] = page{404, nil, ""}
	loop["https://docs.example.com/empty"] = page{302, map[string]string{"Location": ""}, ""}
	loop["https://docs.example.com/nm"] = page{304, nil, ""}
	for u, want := range map[string]string{
		"https://docs.example.com/a":     "https://docs.example.com/a: more than 5 redirects",
		"https://docs.example.com/big":   "https://docs.example.com/big: larger than 10000000 bytes",
		"https://docs.example.com/empty": "https://docs.example.com/empty: redirect without location",
		"https://docs.example.com/gone": "https://docs.example.com/gone: Client error '404 Not Found' for url 'https://docs.example.com/gone'\n" +
			"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/404",
		"https://docs.example.com/nm": "https://docs.example.com/nm: Redirect response '304 Not Modified' for url 'https://docs.example.com/nm'\n" +
			"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/304",
	} {
		_, err := VendorURLs(context.Background(), []string{u}, t.TempDir(), Options{Client: pages(t, loop, nil), Resolver: public})
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v\nwant %s", u, err, want)
		}
	}
}

// A rebinding resolver answers the check with a public address and every later lookup with
// loopback: the pinned dialer dials only the vetted address, with the original SNI and Host.
func TestVendorPinsEveryHop(t *testing.T) {
	var sni, hosts []string
	var mu sync.Mutex
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<p>reference</p>"))
	}))
	srv.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		sni = append(sni, h.ServerName)
		mu.Unlock()
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	var calls []string
	resolver := func(_ context.Context, host string, _ int) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		seen := false
		for _, c := range calls {
			seen = seen || c == host
		}
		calls = append(calls, host)
		if seen {
			return []string{"127.0.0.1"}, nil
		}
		return []string{publicA}, nil
	}
	var dialled []string
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, address)
		mu.Unlock()
		if address != net.JoinHostPort(publicA, "443") {
			return nil, errors.New("refused " + address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	var pinned *safety.PinnedDialer
	opts := Options{Resolver: resolver, Dial: dial, TLSConfig: &tls.Config{RootCAs: roots},
		Dialer: func(d *safety.PinnedDialer) { pinned = d }}
	saved, err := VendorURLs(context.Background(), []string{"https://example.com/ref"}, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if saved[0].Bytes != 16 || saved[0].Path != "example.com/ref.html" {
		t.Fatalf("%+v", saved[0])
	}
	first := pinned.Dialled()
	// Resolved once; a second vendoring sees the rebound (loopback) answer and is refused.
	_, again := VendorURLs(context.Background(), []string{"https://example.com/ref"}, t.TempDir(), opts)
	if again == nil || !strings.Contains(again.Error(), "non-public") {
		t.Fatalf("rebound answer accepted: %v", again)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || len(dialled) != 1 {
		t.Fatalf("resolver calls %v, dialled %v", calls, dialled)
	}
	for _, d := range dialled {
		if d != net.JoinHostPort(publicA, "443") {
			t.Fatalf("dialled %v", dialled)
		}
	}
	for _, name := range sni {
		if name != "example.com" {
			t.Fatalf("sni %v", sni)
		}
	}
	for _, h := range hosts {
		if h != "example.com" {
			t.Fatalf("hosts %v", hosts)
		}
	}
	if got := first; len(got) != 1 || len(sni) != 1 || got[0].Address != publicA || got[0].Host != "example.com" {
		t.Fatalf("%+v", got)
	}
}

func TestPinnedDialerRefusesUnpinnedHostsAndPrivateLiterals(t *testing.T) {
	var dialled []string
	d := safety.NewPinnedDialer(func(_ context.Context, _, address string) (net.Conn, error) {
		dialled = append(dialled, address)
		return nil, errors.New("refused " + address)
	}, time.Second)
	ctx := context.Background()
	if _, err := d.DialContext(ctx, "tcp", "a.test:443"); err == nil ||
		err.Error() != "egress: 'a.test' has no vetted address (it was not checked before connect)" {
		t.Fatal(err)
	}
	if _, err := d.DialContext(ctx, "tcp", "127.0.0.1:80"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatal(err)
	}
	if _, err := d.DialContext(ctx, "unix", "/var/run/docker.sock"); err == nil {
		t.Fatal("unix socket dialled")
	}
	if len(dialled) != 0 {
		t.Fatal(dialled)
	}
	if err := d.Pin("a.test", []string{publicA, "169.254.169.254"}); err == nil ||
		err.Error() != "refusing to pin 'a.test' to non-public or no addresses: ('93.184.216.34', '169.254.169.254')" {
		t.Fatal(err)
	}
	if err := d.Pin("a.test", nil); err == nil || !strings.Contains(err.Error(), "no addresses: ()") {
		t.Fatal(err)
	}
	// Public literals are dialled; pinned hosts fall back across their vetted addresses.
	_, _ = d.DialContext(ctx, "tcp", "93.184.216.35:80")
	if err := d.Pin("A.Test.", []string{publicA, "93.184.216.35"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DialContext(ctx, "tcp", "a.test:443"); err == nil || err.Error() != "refused 93.184.216.35:443" {
		t.Fatal(err)
	}
	want := []string{"93.184.216.35:80", publicA + ":443", "93.184.216.35:443"}
	if strings.Join(dialled, ",") != strings.Join(want, ",") {
		t.Fatal(dialled)
	}
	got := d.Dialled()
	if got[1] != (safety.Dialled{Host: "a.test", Address: publicA, Port: "443"}) {
		t.Fatalf("%+v", got)
	}
}

// The Go and the Python implementation leave the same files and MANIFEST.json bytes for the
// same pages (fetched_at pinned) and the same pre-existing manifest.
func TestManifestAndFilesMatchPython(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil || testing.Short() {
		t.Skip("uv not on PATH (or -short); skipping the cross-implementation check")
	}
	_, file, _, _ := runtime.Caller(0)
	project := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "python")
	existing := `{"files": [{"url": "https://docs.example.com/spec.json?v=2", "path": "old", "extra": 1.5},
 {"url": "https://zzz.example/old", "path": "z", "bytes": 3, "note": "café 😀", "big": 1e400, "n": 10000000000000000000000}]}`
	routes := map[string]page{
		"https://docs.example.com/moved":         {302, map[string]string{"Location": "/docs/items"}, ""},
		"https://docs.example.com/docs/items":    {200, map[string]string{"Content-Type": "text/html; charset=utf-8"}, "<html><style>x</style><p>Items API é</p></html>"},
		"https://docs.example.com/spec.json?v=2": {200, map[string]string{"Content-Type": "application/json"}, `{"a": 1}`},
		"https://api.example.org/":               {200, map[string]string{"Content-Type": "text/html; charset=iso-8859-1"}, "<b>caf\xe9</b>"},
	}
	urls := []string{"https://docs.example.com/moved", "https://docs.example.com/spec.json?v=2", "https://api.example.org/"}

	goDir := filepath.Join(t.TempDir(), "ref")
	pyDir := filepath.Join(t.TempDir(), "ref")
	for _, d := range []string{goDir, pyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, Manifest), []byte(existing), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := VendorURLs(context.Background(), urls, goDir, Options{Client: pages(t, routes, nil), Resolver: public, Now: fixedNow}); err != nil {
		t.Fatal(err)
	}

	script := `
import asyncio, json, sys, datetime
import httpx
from lha.state import vendor
routes = json.loads(sys.argv[2])
class Fixed(datetime.datetime):
    @classmethod
    def now(cls, tz=None):
        return datetime.datetime(2026, 9, 26, 12, 0, 0, tzinfo=tz)
vendor.datetime = Fixed
def handler(request):
    status, headers, body = routes.get(str(request.url), [404, {}, ""])
    return httpx.Response(status, headers=headers, content=body.encode("latin-1") if "8859" in headers.get("Content-Type", "") else body.encode())
async def public(host, port):
    return ["93.184.216.34"]
async def main():
    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
        await vendor.vendor_urls(json.loads(sys.argv[3]), sys.argv[1], client=client, resolver=public)
asyncio.run(main())
`
	routesJSON := "{"
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		p := routes[k]
		if i > 0 {
			routesJSON += ","
		}
		h := "{"
		first := true
		for hk, hv := range p.header {
			if !first {
				h += ","
			}
			first = false
			h += quote(hk) + ":" + quote(hv)
		}
		h += "}"
		body := p.body
		if strings.Contains(p.header["Content-Type"], "8859") { // python encodes it back to latin-1
			runes := make([]rune, len(body))
			for i := 0; i < len(body); i++ {
				runes[i] = rune(body[i])
			}
			body = string(runes)
		}
		routesJSON += quote(k) + ":[" + strconv.Itoa(p.status) + "," + h + "," + quote(body) + "]"
	}
	routesJSON += "}"
	urlsJSON := `["` + strings.Join(urls, `","`) + `"]`
	cmd := exec.Command("uv", "run", "--quiet", "--project", project, "python", "-c", script, pyDir, routesJSON, urlsJSON)
	cmd.Dir = project
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("python: %v\n%s", err, stderr.String())
	}
	goFiles, pyFiles := tree(t, goDir), tree(t, pyDir)
	if len(goFiles) != len(pyFiles) {
		t.Fatalf("files differ:\ngo %v\npy %v", keysOf(goFiles), keysOf(pyFiles))
	}
	for name, content := range pyFiles {
		if goFiles[name] != content {
			t.Errorf("%s differs:\n--- go\n%s\n--- python\n%s", name, goFiles[name], content)
		}
	}
}

func quote(s string) string {
	var b strings.Builder
	writeJSONString(&b, s)
	return b.String()
}

func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
