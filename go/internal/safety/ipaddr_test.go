package safety

import (
	"context"
	"reflect"
	"testing"
)

// ipaddress.ip_address's answers (CPython 3.12.13): validity, family and integer value.
func TestParseIPAddress(t *testing.T) {
	for _, c := range []struct {
		in     string
		v6     bool
		hi, lo uint64
		ok     bool
	}{
		{"1::2", true, 0x1000000000000, 0x2, true},
		{"::", true, 0x0, 0x0, true},
		{"::1", true, 0x0, 0x1, true},
		{"1:2:3:4:5:6:7:8", true, 0x1000200030004, 0x5000600070008, true},
		{"1:2:3:4:5:6:7::", true, 0x1000200030004, 0x5000600070000, true},
		{"::1:2:3:4:5:6:7", true, 0x100020003, 0x4000500060007, true},
		{"1:2:3:4:5:6::7", true, 0x1000200030004, 0x5000600000007, true},
		{"1:2:3:4:5:6:7:8:9", false, 0, 0, false},
		{"1:2:3:4:5:6:7::8", false, 0, 0, false},
		{"0000:0000:0000:0000:0000:ffff:255.255.255.255", true, 0x0, 0xffffffffffff, true}, // 45 characters
		{"0000:0000:0000:0000:0000:ffff:255.255.255.2555", false, 0, 0, false},
		{"::ffff:1.2.3.4", true, 0x0, 0xffff01020304, true},
		{"::ffff:0.0.0.0", true, 0x0, 0xffff00000000, true},
		{"::ffff:0.16.0.0", true, 0x0, 0xffff00100000, true},
		{"abcd:ef01:2345:6789:ABCD:EF01:0:9", true, 0xabcdef0123456789, 0xabcdef0100000009, true},
		{"a:b:c:d:e:f:A:F", true, 0xa000b000c000d, 0xe000f000a000f, true},
		{"g::1", false, 0, 0, false},
		{"G::1", false, 0, 0, false},
		{"@::1", false, 0, 0, false},
		{"`::1", false, 0, 0, false},
		{"/::1", false, 0, 0, false},
		{":::1", false, 0, 0, false},
		{"1:::2", false, 0, 0, false},
		{"::1:", false, 0, 0, false},
		{"::1::", false, 0, 0, false},
		{"fe80::1%eth0", true, 0xfe80000000000000, 0x1, true},
		{"1.2.3.4", false, 0x0, 0x1020304, true},
		{"1:2:3:4:5:6:1.2.3.4", true, 0x1000200030004, 0x5000601020304, true},
		{"1:2:3:4:5:6:7:1.2.3.4", false, 0, 0, false},
		{"::2", true, 0x0, 0x2, true},
	} {
		a, err := parseIPAddress(c.in)
		switch {
		case c.ok && (err != nil || a != ipAddr{v6: c.v6, hi: c.hi, lo: c.lo}):
			t.Errorf("parseIPAddress(%q) = %+v, %v; want v6=%v %#x %#x", c.in, a, err, c.v6, c.hi, c.lo)
		case !c.ok && err == nil:
			t.Errorf("parseIPAddress(%q) = %+v, want an error", c.in, a)
		}
	}
}

func TestIsPublicAddressEdges(t *testing.T) {
	for addr, want := range map[string]bool{
		"2001:1::1": true, "2001:1::2": true, "2001:1::3": false, // the /128 exceptions are exact
		"2606:4700:4700::": true, // a public address whose low 64 bits are zero
		"::2":              false,
	} {
		if got := IsPublicAddress(addr); got != want {
			t.Errorf("IsPublicAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestSystemResolverLiterals(t *testing.T) {
	for host, want := range map[string][]string{
		"127.0.0.1":  {"127.0.0.1"},
		"fe80::1%zz": {"fe80::1%zz"}, // the zone is kept, like getaddrinfo
	} {
		got, err := SystemResolver(context.Background(), host, 80)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("SystemResolver(%q) = %q, %v; want %q", host, got, err, want)
		}
	}
}

// ipaddress's `in` for prefixes longer than 64 bits (python: ip_address(a) in ip_network(n)).
func TestV6NetContainsLongPrefixes(t *testing.T) {
	for _, c := range []struct {
		net, addr string
		in        bool
	}{
		{"::ffff:0:0/96", "::ffff:1.2.3.4", true},
		{"::ffff:0:0/96", "::fffe:1.2.3.4", false},
		{"::ffff:0:0/96", "::1:ffff:1.2.3.4", false},
		{"2001:db8::8000:0/97", "2001:db8::8000:1", true},
		{"2001:db8::8000:0/97", "2001:db8::7fff:ffff", false},
		{"2001:db8::8000:0/97", "2001:db8::1:8000:0", false},
	} {
		a, err := parseIPAddress(c.addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := mustV6(c.net).contains(a); got != c.in {
			t.Errorf("%s contains %s = %v, want %v", c.net, c.addr, got, c.in)
		}
	}
}
