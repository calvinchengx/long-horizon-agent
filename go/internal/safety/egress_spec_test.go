package safety

import "testing"

// The cases of spec/safety/egress.json, repeated here so the package's own tests pin them (the mutation
// audit runs only these; internal/spec runs the JSON itself).

func TestSpecEgress(t *testing.T) {
	for _, c := range []struct{ host, want string }{
		{"Example.COM", "example.com"},
		{"example.com.", "example.com"},
		{"[::1]", "::1"},
		{"b\u00fccher.example", "xn--bcher-kva.example"},
		{"stra\u00dfe.de", "xn--strae-oqa.de"},
		{"xn--bcher-kva.example", "xn--bcher-kva.example"},
		{"  spaced.example  ", "spaced.example"},
		{"127.0.0.1", "127.0.0.1"},
		{"", ""},
	} {
		if got := NormalizeHost(c.host); got != c.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", c.host, got, c.want)
		}
	}
	for _, c := range []struct {
		url          string
		ok           bool
		scheme, host string
		port         int
	}{
		{"https://docs.example.com/x", true, "https", "docs.example.com", 443},
		{"http://docs.example.com:80/x", true, "http", "docs.example.com", 80},
		{"https://DOCS.EXAMPLE.COM./x", true, "https", "docs.example.com", 443},
		{"https://docs.example.com:8443/x", true, "https", "docs.example.com", 8443},
		{"ftp://docs.example.com/x", false, "", "", 0},
		{"https://user:pw@docs.example.com/x", false, "", "", 0},
		{"https://stra\u00dfe.de/x", false, "", "", 0},
		{"https://b\u00fccher.example/x", true, "https", "xn--bcher-kva.example", 443},
		{"https:///nohost", false, "", "", 0},
		{"https://docs.example.com:99999/x", false, "", "", 0},
		{"http://[::1]:8080/x", true, "http", "::1", 8080},
		{"not a url", false, "", "", 0},
	} {
		got, err := ParseURL(c.url)
		switch {
		case !c.ok && err == nil:
			t.Errorf("ParseURL(%q) = %+v, want an error", c.url, got)
		case c.ok && (err != nil || got.Scheme != c.scheme || got.Host != c.host || got.Port != c.port):
			t.Errorf("ParseURL(%q) = %+v, %v; want %s %s %d", c.url, got, err, c.scheme, c.host, c.port)
		}
	}
	for _, c := range []struct {
		address string
		public  bool
	}{
		{"127.0.0.1", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"::1", false},
		{"::ffff:127.0.0.1", false},
		{"0.0.0.0", false},
		{"100.64.0.1", false},
		{"fc00::1", false},
		{"fe80::1", false},
		{"fec0::1", false},
		{"93.184.216.34", true},
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"not-an-ip", false},
	} {
		if got := IsPublicAddress(c.address); got != c.public {
			t.Errorf("IsPublicAddress(%q) = %v, want %v", c.address, got, c.public)
		}
	}
	for _, c := range []struct {
		allow     []string
		url       string
		permitted bool
	}{
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https://docs.example.com/x", true},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "http://docs.example.com:80/x", true},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https://DOCS.EXAMPLE.COM./x", true},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https://docs.example.com:8443/x", false},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "ftp://docs.example.com/x", false},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https://user:pw@docs.example.com/x", false},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https://stra\u00dfe.de/x", false},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https://b\u00fccher.example/x", true},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https:///nohost", false},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "https://docs.example.com:99999/x", false},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "http://[::1]:8080/x", false},
		{[]string{"docs.example.com", "strasse.de", "xn--bcher-kva.example"}, "not a url", false},
	} {
		if got := NewEgressPolicy(c.allow...).Permits(c.url); got != c.permitted {
			t.Errorf("Permits(%v, %q) = %v, want %v", c.allow, c.url, got, c.permitted)
		}
	}
}
