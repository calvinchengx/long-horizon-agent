package safety

import (
	"errors"
	"strconv"
	"strings"
)

// This file ports the parts of CPython's ipaddress module the egress checks use:
// ipaddress.ip_address(str) parsing (strict dotted-quad IPv4, RFC 4291 IPv6 with an optional
// "%scope" suffix) and the is_private / is_global / is_reserved / ... predicates, with the
// special-purpose network lists of the CPython version the reference runs on (3.12.13).
// net/netip is close but not identical (zone syntax, which strings parse at all), and these
// answers decide whether a host may be contacted, so the rules are reproduced literally.

var errBadAddress = errors.New("does not appear to be an IPv4 or IPv6 address")

// ipAddr is a parsed address: v4 uses the low 32 bits of lo.
type ipAddr struct {
	v6     bool
	hi, lo uint64
}

// parseIPAddress is ipaddress.ip_address for a str argument.
func parseIPAddress(s string) (ipAddr, error) {
	if v, err := parseIPv4(s); err == nil {
		return ipAddr{lo: uint64(v)}, nil
	}
	if a, err := parseIPv6(s); err == nil {
		return a, nil
	}
	return ipAddr{}, errBadAddress
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseIPv4 is IPv4Address._ip_int_from_string (plus the "/" rejection in __init__).
func parseIPv4(s string) (uint32, error) {
	if s == "" || strings.Contains(s, "/") {
		return 0, errBadAddress
	}
	octets := strings.Split(s, ".")
	if len(octets) != 4 {
		return 0, errBadAddress
	}
	var v uint32
	for _, o := range octets {
		// octet_str.isascii() and octet_str.isdigit(): ASCII digits only.
		if !isASCIIDigits(o) || len(o) > 3 {
			return 0, errBadAddress
		}
		if o != "0" && o[0] == '0' {
			return 0, errBadAddress // leading zeros are not permitted
		}
		n := 0
		for i := 0; i < len(o); i++ {
			n = n*10 + int(o[i]-'0')
		}
		if n > 255 {
			return 0, errBadAddress
		}
		v = v<<8 | uint32(n)
	}
	return v, nil
}

const hextetCount = 8

// parseIPv6 is IPv6Address(str): split off the scope id, then _ip_int_from_string.
func parseIPv6(s string) (ipAddr, error) {
	if strings.Contains(s, "/") {
		return ipAddr{}, errBadAddress
	}
	addr, sep, scope := partition(s, "%")
	if sep && (scope == "" || strings.Contains(scope, "%")) {
		return ipAddr{}, errBadAddress
	}
	if addr == "" || len([]rune(addr)) > 45 {
		return ipAddr{}, errBadAddress
	}
	parts := strings.Split(addr, ":")
	if len(parts) < 3 {
		return ipAddr{}, errBadAddress
	}
	if last := parts[len(parts)-1]; strings.Contains(last, ".") {
		v4, err := parseIPv4(last)
		if err != nil {
			return ipAddr{}, errBadAddress
		}
		parts = parts[:len(parts)-1]
		parts = append(parts, hex16(v4>>16), hex16(v4&0xFFFF))
	}
	if len(parts) > hextetCount+1 {
		return ipAddr{}, errBadAddress
	}
	skip := -1
	for i := 1; i < len(parts)-1; i++ {
		if parts[i] == "" {
			if skip >= 0 {
				return ipAddr{}, errBadAddress // at most one "::"
			}
			skip = i
		}
	}
	var partsHi, partsLo, skipped int
	if skip >= 0 {
		partsHi = skip
		partsLo = len(parts) - skip - 1
		if parts[0] == "" {
			partsHi--
			if partsHi != 0 {
				return ipAddr{}, errBadAddress // ^: requires ^::
			}
		}
		if parts[len(parts)-1] == "" {
			partsLo--
			if partsLo != 0 {
				return ipAddr{}, errBadAddress // :$ requires ::$
			}
		}
		skipped = hextetCount - (partsHi + partsLo)
		if skipped < 1 {
			return ipAddr{}, errBadAddress
		}
	} else {
		if len(parts) != hextetCount || parts[0] == "" || parts[len(parts)-1] == "" {
			return ipAddr{}, errBadAddress
		}
		partsHi, partsLo = len(parts), 0
	}
	var hextets []uint64
	for i := 0; i < partsHi; i++ {
		h, err := parseHextet(parts[i])
		if err != nil {
			return ipAddr{}, err
		}
		hextets = append(hextets, h)
	}
	for i := 0; i < skipped; i++ {
		hextets = append(hextets, 0)
	}
	for i := len(parts) - partsLo; i < len(parts); i++ {
		h, err := parseHextet(parts[i])
		if err != nil {
			return ipAddr{}, err
		}
		hextets = append(hextets, h)
	}
	var a ipAddr
	a.v6 = true
	for i, h := range hextets {
		if i < 4 {
			a.hi = a.hi<<16 | h
		} else {
			a.lo = a.lo<<16 | h
		}
	}
	return a, nil
}

func hex16(v uint32) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{digits[v&0xF]}, b...)
		v >>= 4
	}
	return string(b)
}

func parseHextet(s string) (uint64, error) {
	// _HEX_DIGITS.issuperset(s), len <= 4, then int(s, 16) (which rejects "").
	if s == "" || len(s) > 4 {
		return 0, errBadAddress
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint64(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint64(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint64(c-'A'+10)
		default:
			return 0, errBadAddress
		}
	}
	return v, nil
}

type v4net struct {
	addr uint32
	bits int
}

func (n v4net) contains(ip uint32) bool {
	if n.bits == 0 {
		return true
	}
	mask := ^uint32(0) << (32 - n.bits)
	return ip&mask == n.addr&mask
}

type v6net struct {
	hi, lo uint64
	bits   int
}

func (n v6net) contains(a ipAddr) bool {
	switch {
	case n.bits <= 64:
		if n.bits == 0 {
			return true
		}
		mask := ^uint64(0) << (64 - n.bits)
		return a.hi&mask == n.hi&mask
	default:
		if a.hi != n.hi {
			return false
		}
		if n.bits == 128 {
			return a.lo == n.lo
		}
		mask := ^uint64(0) << (128 - n.bits)
		return a.lo&mask == n.lo&mask
	}
}

func mustV4(cidr string) v4net {
	addr, bits, _ := strings.Cut(cidr, "/")
	v, err := parseIPv4(addr)
	if err != nil {
		panic(cidr)
	}
	return v4net{v, atoi(bits)}
}

func mustV6(cidr string) v6net {
	addr, bits, _ := strings.Cut(cidr, "/")
	a, err := parseIPv6(addr)
	if err != nil {
		panic(cidr)
	}
	return v6net{a.hi, a.lo, atoi(bits)}
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(s)
	}
	return n
}

// _IPv4Constants / _IPv6Constants of CPython 3.12.13.
var (
	v4LinkLocal  = mustV4("169.254.0.0/16")
	v4Loopback   = mustV4("127.0.0.0/8")
	v4Multicast  = mustV4("224.0.0.0/4")
	v4PublicNet  = mustV4("100.64.0.0/10")
	v4Reserved   = mustV4("240.0.0.0/4")
	v4PrivateNet = []v4net{
		mustV4("0.0.0.0/8"), mustV4("10.0.0.0/8"), mustV4("127.0.0.0/8"),
		mustV4("169.254.0.0/16"), mustV4("172.16.0.0/12"), mustV4("192.0.0.0/24"),
		mustV4("192.0.0.170/31"), mustV4("192.0.2.0/24"), mustV4("192.168.0.0/16"),
		mustV4("198.18.0.0/15"), mustV4("198.51.100.0/24"), mustV4("203.0.113.0/24"),
		mustV4("240.0.0.0/4"), mustV4("255.255.255.255/32"),
	}
	v4PrivateExceptions = []v4net{mustV4("192.0.0.9/32"), mustV4("192.0.0.10/32")}

	v6LinkLocal  = mustV6("fe80::/10")
	v6Multicast  = mustV6("ff00::/8")
	v6PrivateNet = []v6net{
		mustV6("::1/128"), mustV6("::/128"), mustV6("::ffff:0:0/96"), mustV6("64:ff9b:1::/48"),
		mustV6("100::/64"), mustV6("2001::/23"), mustV6("2001:db8::/32"), mustV6("2002::/16"),
		mustV6("3fff::/20"), mustV6("fc00::/7"), mustV6("fe80::/10"),
	}
	v6PrivateExceptions = []v6net{
		mustV6("2001:1::1/128"), mustV6("2001:1::2/128"), mustV6("2001:3::/32"),
		mustV6("2001:4:112::/48"), mustV6("2001:20::/28"), mustV6("2001:30::/28"),
	}
	v6Reserved = []v6net{
		mustV6("::/8"), mustV6("100::/8"), mustV6("200::/7"), mustV6("400::/6"),
		mustV6("800::/5"), mustV6("1000::/4"), mustV6("4000::/3"), mustV6("6000::/3"),
		mustV6("8000::/3"), mustV6("A000::/3"), mustV6("C000::/3"), mustV6("E000::/4"),
		mustV6("F000::/5"), mustV6("F800::/6"), mustV6("FE00::/9"),
	}
)

func v4IsPrivate(ip uint32) bool {
	in := false
	for _, n := range v4PrivateNet {
		if n.contains(ip) {
			in = true
			break
		}
	}
	if !in {
		return false
	}
	for _, n := range v4PrivateExceptions {
		if n.contains(ip) {
			return false
		}
	}
	return true
}

// v4Public: not (is_private or is_loopback or is_link_local or is_multicast or is_reserved or
// is_unspecified or not is_global).
func v4Public(ip uint32) bool {
	private := v4IsPrivate(ip)
	global := !v4PublicNet.contains(ip) && !private
	return !(private || v4Loopback.contains(ip) || v4LinkLocal.contains(ip) ||
		v4Multicast.contains(ip) || v4Reserved.contains(ip) || ip == 0 || !global)
}

func v6Public(a ipAddr) bool {
	private := false
	for _, n := range v6PrivateNet {
		if n.contains(a) {
			private = true
			break
		}
	}
	if private {
		for _, n := range v6PrivateExceptions {
			if n.contains(a) {
				private = false
				break
			}
		}
	}
	reserved := false
	for _, n := range v6Reserved {
		if n.contains(a) {
			reserved = true
			break
		}
	}
	unspecified := a.hi == 0 && a.lo == 0
	loopback := a.hi == 0 && a.lo == 1
	// is_global is "not is_private" for IPv6, so "not is_global" adds nothing here.
	return !(private || loopback || v6LinkLocal.contains(a) || v6Multicast.contains(a) ||
		reserved || unspecified)
}

// IsPublicAddress reports whether address is a globally routable unicast address. IPv4-mapped
// and 6to4 IPv6 addresses are judged by their embedded IPv4 address, Teredo addresses by their
// client address; a "%zone" suffix is ignored; anything that is not an IP address is not public.
func IsPublicAddress(address string) bool {
	host, _, _ := partition(address, "%")
	a, err := parseIPAddress(host)
	if err != nil {
		return false
	}
	if !a.v6 {
		return v4Public(uint32(a.lo))
	}
	switch {
	case a.hi == 0 && a.lo>>32 == 0xFFFF: // ipv4_mapped
		return v4Public(uint32(a.lo))
	case a.hi>>48 == 0x2002: // sixtofour
		return v4Public(uint32(a.hi >> 16))
	case a.hi>>32 == 0x20010000: // teredo: (server, client); the client is ~ip & 0xFFFFFFFF
		return v4Public(^uint32(a.lo))
	}
	return v6Public(a)
}

// isIPLiteral reports whether ipaddress.ip_address(host) accepts host.
func isIPLiteral(host string) bool {
	_, err := parseIPAddress(host)
	return err == nil
}
