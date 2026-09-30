package safety

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Ported from python/tests/unit/test_safety.py, test_safety_egress.py and
// test_review_fixes_safety.py (egress parts), plus the Python-semantics edge cases the port
// reproduces.

const publicIP = "93.184.216.34"

func tableResolver(table map[string][]string) Resolver {
	return func(_ context.Context, host string, _ int) ([]string, error) {
		if addrs, ok := table[host]; ok {
			return addrs, nil
		}
		return []string{publicIP}, nil
	}
}

func TestEgressPolicyIsDefaultDeny(t *testing.T) {
	if (&EgressPolicy{}).Permits("https://example.com/x") {
		t.Error("empty policy permits")
	}
	var zero EgressPolicy
	if zero.Permits("https://example.com/x") {
		t.Error("zero policy permits")
	}
	p := NewEgressPolicy("api.tavily.com")
	if !p.Permits("https://api.tavily.com/search") {
		t.Error("allowed host denied")
	}
	if p.Permits("https://evil.test/exfiltrate") {
		t.Error("other host permitted")
	}
}

func TestPolicyChecksSchemePortAndNormalizesHost(t *testing.T) {
	p := NewEgressPolicy("Docs.Example.com")
	for url, want := range map[string]bool{
		"https://docs.example.com/x":         true,
		"https://DOCS.EXAMPLE.COM./x":        true,
		"http://docs.example.com:80/x":       true,
		"ftp://docs.example.com/x":           false,
		"file:///etc/passwd":                 false,
		"https://docs.example.com:8443/x":    false,
		"https://user:pw@docs.example.com/x": false,
		"https://docs.example.com.evil.test": false,
		"https://docs.example.com:0/x":       true, // Python: ``port or default``
		"https://docs.example.com:0443/x":    true,
	} {
		if got := p.Permits(url); got != want {
			t.Errorf("Permits(%q) = %v, want %v", url, got, want)
		}
	}
	withPort := &EgressPolicy{AllowHosts: []string{"docs.example.com"}, AllowPorts: []int{8443}}
	if !withPort.Permits("https://docs.example.com:8443/x") {
		t.Error("allowed port denied")
	}
	httpsOnly := &EgressPolicy{AllowHosts: []string{"docs.example.com"}, AllowSchemes: []string{"https"}}
	_, err := httpsOnly.Check("http://docs.example.com/x")
	if err == nil || err.Error() != "scheme not allowed: 'http'" {
		t.Errorf("scheme check: %v", err)
	}
	none := &EgressPolicy{AllowHosts: []string{"docs.example.com"}, AllowSchemes: []string{}}
	if none.Permits("https://docs.example.com/x") {
		t.Error("empty AllowSchemes permits")
	}
}

func TestCheckErrors(t *testing.T) {
	p := NewEgressPolicy("docs.example.com", "", "☃.com")
	cases := map[string]string{
		"ftp://x/":                              "scheme not allowed: 'ftp'",
		"https://u@x/":                          "credentials in the url are not allowed",
		"https://other.example/":                "host not in egress allow-list: 'other.example'",
		"https://docs.example.com:1/":           "port not allowed: 1",
		"https://docs.example.com:99999/":       "malformed url: 'https://docs.example.com:99999/'",
		"https://docs.example.com:8x/":          "malformed url: 'https://docs.example.com:8x/'",
		"https://[::1/":                         "malformed url: 'https://[::1/'",
		"https://[1.2.3.4]/":                    "malformed url: 'https://[1.2.3.4]/'",
		"https://\u2100.com/":                   "malformed url: 'https://\u2100.com/'",
		"https:///x":                            "url has no valid host: 'https:///x'",
		"https://straße.de/":                    "ambiguous internationalized host (IDNA 2003/2008 differ): 'https://straße.de/'",
		"https://☃.com/":                        "ambiguous internationalized host (IDNA 2003/2008 differ): 'https://☃.com/'",
		"https://a_b.bücher.de/":                "ambiguous internationalized host (IDNA 2003/2008 differ): 'https://a_b.bücher.de/'",
		"https://xn--bcher-kva-.example/":       "host not in egress allow-list: 'xn--bcher-kva-.example'",
		"https://bücher.xn--bcher-kva-.example": "ambiguous internationalized host (IDNA 2003/2008 differ): 'https://bücher.xn--bcher-kva-.example'",
	}
	for url, want := range cases {
		_, err := p.Check(url)
		if err == nil {
			t.Errorf("Check(%q) = nil, want %q", url, want)
			continue
		}
		if err.Error() != want {
			t.Errorf("Check(%q) = %q, want %q", url, err.Error(), want)
		}
		if !errors.Is(err, ErrEgressDenied) {
			t.Errorf("Check(%q): error does not wrap ErrEgressDenied", url)
		}
		var denied *EgressDeniedError
		if !errors.As(err, &denied) || denied.Reason != want {
			t.Errorf("Check(%q): not an *EgressDeniedError", url)
		}
	}
}

func TestParseURL(t *testing.T) {
	cases := []struct {
		url  string
		want ParsedURL
	}{
		{"  https://Docs.Example.com./x ", ParsedURL{"https", "docs.example.com", 443}},
		{"HTTP://h.com", ParsedURL{"http", "h.com", 80}},
		{"http://[fe80::1%25eth0]:8080/", ParsedURL{"http", "fe80::1%25eth0", 8080}},
		{"https://bücher.example/x", ParsedURL{"https", "xn--bcher-kva.example", 443}},
		{"https://h\t.com\n/", ParsedURL{"https", "h.com", 443}},
		{"https://[v1.x]/", ParsedURL{"https", "v1.x", 443}}, // IPvFuture passes urlsplit
		{"https://[vx.x]/", ParsedURL{}},
	}
	for _, c := range cases {
		got, err := ParseURL(c.url)
		if c.want == (ParsedURL{}) {
			if err == nil {
				t.Errorf("ParseURL(%q) = %+v, want error", c.url, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseURL(%q) = %+v, %v; want %+v", c.url, got, err, c.want)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM":                   "example.com",
		"[::1]":                         "::1",
		"bücher.example":                "xn--bcher-kva.example",
		"BÜCHER.example":                "xn--bcher-kva.example",
		"straße.de":                     "xn--strae-oqa.de",
		"bücher.example\u3002":          "xn--bcher-kva.example.",
		"a..bücher.de":                  "",
		".bücher.de":                    "",
		"a_b.bücher.de":                 "",
		"☃.com":                         "",
		"ab--c.bücher.de":               "",
		"-a.bücher.de":                  "",
		"\u0301a.de":                    "",
		"a\u200db.de":                   "", // ZWJ outside a virama context (CONTEXTJ)
		"\u0915\u094d\u200d.de":         "xn--11b6iy14e.de",
		"l\u00b7l.cat":                  "xn--ll-0ea.cat", // CONTEXTO middle dot
		"a\u00b7b.cat":                  "",
		"\u05d0\u05d1.il":               "xn--4dbc.il",
		"\u05d0a.il":                    "", // bidi rule
		"\u0661\u06f1.x":                "", // mixed Arabic-Indic digits
		"ab\u00ad.de":                   "ab.de",
		"xn--bcher-kva.bücher.de":       "xn--bcher-kva.xn--bcher-kva.de",
		"\u0130.de":                     "xn--i-9bb.de", // Python lower-cases İ to i + U+0307
		strings.Repeat("ü", 60) + ".de": "",
		"   ":                           "",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsAmbiguousIDN(t *testing.T) {
	for in, want := range map[string]bool{
		"straße.de":       true,
		"\u03b1\u03c2.gr": true,
		"a\u200db.de":     true, // IDNA 2003 drops the joiner; IDNA 2008 rejects it
		"bücher.example":  false,
		"example.com":     false,
		"":                false,
		"☃.com":           true,
		"\ufdd0.com":      false, // prohibited in IDNA 2003 (legacy fails)
		"\u05d0a.il":      false, // IDNA 2003 bidi requirement 2 fails
	} {
		if got := IsAmbiguousIDN(in); got != want {
			t.Errorf("IsAmbiguousIDN(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNonPublicAddresses(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.169.254", "::1", "fe80::1", "224.0.0.1",
		"::ffff:127.0.0.1", "0.0.0.0", "100.64.0.1", "fc00::1", "2002:7f00:1::", "2001:0:1:2:3:4:80ff:fefe",
		"fe80::1%eth0", "::", "ff02::1", "2001:db8::1", "240.0.0.1", "255.255.255.255", "192.0.0.1",
		"", "not-an-ip", "1.2.3", "01.2.3.4", "::1%", "12345::",
		"1:2:3:4:5:6:7:8:9", "1::2::3", "::ffff:1.2.3.04",
		"fec0::1", // deprecated site-local (fec0::/10), still routed internally by some networks
	} {
		if IsPublicAddress(addr) {
			t.Errorf("IsPublicAddress(%q) = true", addr)
		}
	}
	for _, addr := range []string{
		publicIP, "8.8.8.8", "2606:4700:4700::1111", "::ffff:8.8.8.8", "2002:808:808::1",
		"192.0.0.9", "2001:4:112::1", "2001:0:1:2:3:4:f7f7:f7f7", "2606:4700::1%lo0",
		"1.2.3.4%x", // the "%zone" suffix is dropped before parsing
	} {
		if !IsPublicAddress(addr) {
			t.Errorf("IsPublicAddress(%q) = false", addr)
		}
	}
}

func TestCheckResolvedAddresses(t *testing.T) {
	ctx := context.Background()
	resolve := tableResolver(map[string][]string{
		"rebind.test": {publicIP, "127.0.0.1"},
		"empty.test":  {},
	})
	_, err := CheckResolvedAddresses(ctx, "rebind.test", 443, resolve)
	if err == nil || err.Error() != "'rebind.test' resolves to a non-public address: 127.0.0.1" {
		t.Errorf("rebind: %v", err)
	}
	got, err := CheckResolvedAddresses(ctx, "ok.test", 443, resolve)
	if err != nil || len(got) != 1 || got[0] != publicIP {
		t.Errorf("ok.test: %v %v", got, err)
	}
	if _, err := CheckResolvedAddresses(ctx, "empty.test", 443, resolve); err == nil ||
		err.Error() != "'empty.test' did not resolve" {
		t.Errorf("empty: %v", err)
	}
	if _, err := CheckResolvedAddresses(ctx, "10.1.2.3", 443, nil); err == nil ||
		err.Error() != "'10.1.2.3' is a non-public address" {
		t.Errorf("literal: %v", err)
	}
	if got, err := CheckResolvedAddresses(ctx, "8.8.8.8", 443, nil); err != nil || got[0] != "8.8.8.8" {
		t.Errorf("public literal: %v %v", got, err)
	}
	boom := errors.New("[Errno 8] nodename nor servname provided")
	_, err = CheckResolvedAddresses(ctx, "x.test", 443, func(context.Context, string, int) ([]string, error) {
		return nil, boom
	})
	if err == nil || err.Error() != "cannot resolve 'x.test': [Errno 8] nodename nor servname provided" ||
		!errors.Is(err, boom) || !errors.Is(err, ErrEgressDenied) {
		t.Errorf("resolver error: %v", err)
	}
}

func TestSystemResolver(t *testing.T) {
	addrs, err := SystemResolver(context.Background(), "localhost", 80)
	if err != nil {
		t.Skipf("no resolver: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("localhost did not resolve")
	}
	if _, err := CheckResolvedAddresses(context.Background(), "localhost", 80, SystemResolver); err == nil {
		t.Error("localhost passed the public-address check")
	}
}

func TestCredentialBroker(t *testing.T) {
	b := NewCredentialBroker()
	if err := b.Register("{{TAVILY_KEY}}", "real-secret-123", "api.tavily.com"); err != nil {
		t.Fatal(err)
	}
	h := b.ResolveHeaders(map[string]string{"Authorization": "Bearer {{TAVILY_KEY}}"}, "API.Tavily.com.")
	if h["Authorization"] != "Bearer real-secret-123" {
		t.Errorf("bound host: %q", h["Authorization"])
	}
	if got := b.Resolve("no placeholder here", "api.tavily.com"); got != "no placeholder here" {
		t.Errorf("untouched value changed: %q", got)
	}
	leaked := b.ResolveHeaders(map[string]string{"Authorization": "Bearer {{TAVILY_KEY}}"}, "evil.test")
	if leaked["Authorization"] != "Bearer {{TAVILY_KEY}}" {
		t.Errorf("secret injected for an unbound host: %q", leaked["Authorization"])
	}
	if err := b.Register("{{X}}", "secret"); !errors.Is(err, ErrUnboundCredential) {
		t.Errorf("unbound register: %v", err)
	}
	if err := b.Register("{{X}}", "secret", "", "  "); err == nil ||
		err.Error() != "a brokered credential must be bound to at least one host" {
		t.Errorf("unnormalizable hosts: %v", err)
	}
	// Re-registering replaces the secret and hosts (and keeps the resolution order).
	if err := b.Register("{{TAVILY_KEY}}", "rotated", "other.test"); err != nil {
		t.Fatal(err)
	}
	if got := b.Resolve("{{TAVILY_KEY}}", "api.tavily.com"); got != "{{TAVILY_KEY}}" {
		t.Errorf("old binding still active: %q", got)
	}
	if got := b.Resolve("{{TAVILY_KEY}}", "other.test"); got != "rotated" {
		t.Errorf("rotated: %q", got)
	}
	var zero CredentialBroker
	if got := zero.Resolve("x", "h"); got != "x" {
		t.Errorf("zero broker: %q", got)
	}
}

func TestRuleOfTwo(t *testing.T) {
	if !Permits(UntrustedContent, ExternalComms) || CheckRuleOfTwo(UntrustedContent, ExternalComms) != nil {
		t.Error("two capabilities rejected")
	}
	if !Permits(UntrustedContent, UntrustedContent, PrivateData) {
		t.Error("duplicates counted twice")
	}
	if Permits(UntrustedContent, PrivateData, ExternalComms) {
		t.Error("trifecta permitted")
	}
	err := CheckRuleOfTwo(UntrustedContent, PrivateData, ExternalComms)
	if !errors.Is(err, ErrRuleOfTwoViolation) || err.Error() != "session would hold untrusted content + "+
		"private data + external comms (the lethal trifecta); split capabilities across sessions" {
		t.Errorf("trifecta: %v", err)
	}
	if !Permits() {
		t.Error("empty set rejected")
	}
}

// The messages of spec/execution/sandbox_egress.json's rule_of_two cases.
func TestRunRefusal(t *testing.T) {
	const why = "LHA_PRIVATE_DATA=true declares the workspace holds secrets or customer data"
	const tail = ", which brings untrusted content and external comms, and " + why + ". session would " +
		"hold untrusted content + private data + external comms (the lethal trifecta); split " +
		"capabilities across sessions. Use a docker/e2b sandbox without private data, or clear the allow-list ("
	const sandbox = "LHA_SANDBOX_EGRESS / LHA_SANDBOX_EGRESS_EXTRA_HOSTS / LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS"
	for _, c := range []struct {
		web, egress []string
		want        string
	}{
		{nil, []string{"github.com", "gitlab.com"}, "refusing to start: the sandbox can reach the network " +
			"(sandbox egress: github.com, gitlab.com)" + tail + sandbox + ")."},
		{[]string{"a.test", "b.test"}, []string{"pypi.org"}, "refusing to start: web tools are enabled " +
			"(egress allow-list: a.test, b.test) and the sandbox can reach the network (sandbox egress: " +
			"pypi.org)" + tail + "LHA_WEB_ALLOW_HOSTS / --allow-host; " + sandbox + ")."},
		{[]string{"docs.test"}, nil, "refusing to start: web tools are enabled (egress allow-list: " +
			"docs.test)" + tail + "LHA_WEB_ALLOW_HOSTS / --allow-host)."},
	} {
		if got := RunRefusal(c.web, c.egress, why, ErrRuleOfTwoViolation); got != c.want {
			t.Errorf("RunRefusal(%v, %v):\n got %s\nwant %s", c.web, c.egress, got, c.want)
		}
	}
}

func TestPunycode(t *testing.T) {
	for in, want := range map[string]string{
		"bücher": "bcher-kva", "straße": "strae-oqa", "ü": "tda", "☃": "n3h", "ab": "ab-",
		"\u0915\u094d\u200d": "11b6iy14e",
	} {
		if got := punycodeEncode(in); got != want {
			t.Errorf("punycodeEncode(%q) = %q, want %q", in, got, want)
		}
		if back, ok := punycodeDecode(want); !ok || back != in {
			t.Errorf("punycodeDecode(%q) = %q, %v; want %q", want, back, ok, in)
		}
	}
	for _, bad := range []string{"-", "a-!", "99999999999999999", "zzzzzzzzzzzzzzzzzzzzzzzz", "a-b"} {
		if _, ok := punycodeDecode(bad); ok && bad != "-" {
			t.Errorf("punycodeDecode(%q) succeeded", bad)
		}
	}
}

// urllib.parse.urlsplit's answers (CPython 3.12.13): which prefixes are schemes, where the netloc
// ends, and which bracketed hosts are rejected.
func TestURLSplit(t *testing.T) {
	for _, c := range []struct {
		in, scheme, netloc string
		fails              bool
	}{
		{"a://h", "a", "h", false},
		{"z://h", "z", "h", false},
		{"A://h", "a", "h", false},
		{"Z://h", "z", "h", false},
		{"@b://h", "", "", false},
		{"[b://h", "", "", false},
		{"`b://h", "", "", false},
		{"{b://h", "", "", false},
		{"xa://h", "xa", "h", false},
		{"xz://h", "xz", "h", false},
		{"xA://h", "xa", "h", false},
		{"xZ://h", "xz", "h", false},
		{"x0://h", "x0", "h", false},
		{"x9://h", "x9", "h", false},
		{"x+://h", "x+", "h", false},
		{"x-://h", "x-", "h", false},
		{"x.://h", "x.", "h", false},
		{"x@://h", "", "", false},
		{"x[://h", "", "", false},
		{"x`://h", "", "", false},
		{"x{://h", "", "", false},
		{"x/://h", "", "", false},
		{"x:://h", "x", "", false},
		{"x,://h", "", "", false},
		{"x*://h", "", "", false},
		{":x://h", "", "", false},
		{"//h/x?y#z", "", "h", false},
		{"//h?x/y", "", "h", false},
		{"//h#x?y/z", "", "h", false},
		{"//h/x#y", "", "h", false},
		{"//h/x?y", "", "h", false},
		{"//[v0.x]", "", "[v0.x]", false},
		{"//[v9.x]", "", "[v9.x]", false},
		{"//[va.x]", "", "[va.x]", false},
		{"//[vf.x]", "", "[vf.x]", false},
		{"//[vA.x]", "", "[vA.x]", false},
		{"//[vF.x]", "", "[vF.x]", false},
		{"//[v/.x]", "", "", true}, // Invalid IPv6 URL
		{"//[v:.x]", "", "", true}, // IPvFuture address is invalid
		{"//[v`.x]", "", "", true}, // IPvFuture address is invalid
		{"//[vg.x]", "", "", true}, // IPvFuture address is invalid
		{"//[v@.x]", "", "", true}, // '.x]' does not appear to be an IPv4 or IPv6 address
		{"//[vG.x]", "", "", true}, // IPvFuture address is invalid
		{"//[v1]", "", "", true},   // IPvFuture address is invalid
		{"//[v1.]", "", "", true},  // IPvFuture address is invalid
		{"//[v]", "", "", true},    // IPvFuture address is invalid
		{"//[v.x]", "", "", true},  // IPvFuture address is invalid
	} {
		got, err := urlSplit(c.in)
		switch {
		case c.fails && err == nil:
			t.Errorf("urlSplit(%q) = %+v, want an error", c.in, got)
		case !c.fails && (err != nil || got != splitURL{c.scheme, c.netloc}):
			t.Errorf("urlSplit(%q) = %+v, %v; want %q %q", c.in, got, err, c.scheme, c.netloc)
		}
	}
}

// parse_url's answers (python/src/lha/safety/egress.py).
func TestParseURLEdges(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"http://h:65535", "http h 65535"},
		{"http://h:0065535", "http h 65535"},
		{"http://h:65536", "malformed url: 'http://h:65536'"},
		{"http://h:100000", "malformed url: 'http://h:100000'"},
		{"https://example.com/a?b", "https example.com 443"},
		{"http://u@[::1]/", "credentials in the url are not allowed"},
		{"http://@[::1]/", "credentials in the url are not allowed"},
	} {
		got, err := ParseURL(c.in)
		text := fmt.Sprintf("%s %s %d", got.Scheme, got.Host, got.Port)
		if err != nil {
			text = err.Error()
		}
		if text != c.want {
			t.Errorf("ParseURL(%q) = %s, want %s", c.in, text, c.want)
		}
	}
}
