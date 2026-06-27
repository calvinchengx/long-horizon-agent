package safety

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// Default-deny network egress + credential brokering (python/src/lha/safety/egress.py).
//
// The agent only ever holds placeholder tokens; the CredentialBroker swaps in the real secret at
// the egress boundary, and only for the host that secret is bound to, so a hijacked agent
// exfiltrates useless placeholders. EgressPolicy enforces an allow-list of (scheme, normalized
// host, port); CheckResolvedAddresses rejects hosts that resolve to private / loopback /
// link-local / multicast addresses (SSRF / DNS rebinding to internal services). Together these
// are the network half of the safety boundary for in-process HTTP tools; run_command egress is
// enforced by the sandbox (no network), not here.

// DefaultPorts maps each permitted scheme to its default port.
var DefaultPorts = map[string]int{"http": 80, "https": 443}

// ErrEgressDenied is matched (errors.Is) by every error that denies egress.
var ErrEgressDenied = errors.New("egress denied")

// EgressDeniedError carries the reason a URL or its resolved addresses are not permitted. Its
// Error() text is byte-identical to the Python EgressDenied message.
type EgressDeniedError struct {
	Reason string
	Cause  error // the resolver error, when resolution failed
}

func (e *EgressDeniedError) Error() string { return e.Reason }

// Unwrap exposes ErrEgressDenied and the underlying cause (if any) to errors.Is / errors.As.
func (e *EgressDeniedError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrEgressDenied, e.Cause}
	}
	return []error{ErrEgressDenied}
}

func denied(format string, args ...any) error {
	return &EgressDeniedError{Reason: fmt.Sprintf(format, args...)}
}

func cleanHost(host string) string {
	host = strings.Trim(pystr.Strip(host), "[]")
	return pystr.Lower(strings.TrimRight(host, "."))
}

// NormalizeHost lower-cases host, strips surrounding whitespace, brackets and trailing dots, and
// IDNA-encodes it (IDNA 2008 + UTS #46, the encoding HTTP clients use when connecting), so the
// host the policy checks is the host actually contacted. IP literals are returned as-is (IPv6
// without brackets). It returns "" for hosts that cannot be encoded; callers treat that as
// invalid / denied.
func NormalizeHost(host string) string {
	host = cleanHost(host)
	if host == "" {
		return ""
	}
	if isIPLiteral(host) || pystr.IsASCII(host) {
		return host
	}
	encoded, err := encodeIDNA2008(host)
	if err != nil {
		return ""
	}
	return strings.ToLower(encoded)
}

// IsAmbiguousIDN reports whether IDNA 2003 and IDNA 2008 encode host differently (e.g. "ß", "ς",
// ZWJ). Such hosts name two different domains depending on the encoder, so egress refuses them.
func IsAmbiguousIDN(host string) bool {
	host = cleanHost(host)
	if host == "" || pystr.IsASCII(host) {
		return false
	}
	modern := NormalizeHost(host)
	legacy, ok := encodeIDNA2003(host)
	if !ok {
		return false
	}
	return strings.ToLower(legacy) != modern
}

// ParsedURL is a URL validated for egress.
type ParsedURL struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

// ParseURL parses and validates a URL for egress: http/https, a host present, no userinfo, a
// valid port. Errors wrap ErrEgressDenied.
func ParseURL(rawURL string) (ParsedURL, error) {
	parts, err := urlSplit(pystr.Strip(rawURL))
	var port int
	var hasPort bool
	if err == nil {
		port, hasPort, err = parts.port()
	}
	if err != nil {
		return ParsedURL{}, &EgressDeniedError{
			Reason: "malformed url: " + contracts.PyRepr(rawURL), Cause: err}
	}
	if _, ok := DefaultPorts[parts.scheme]; !ok {
		return ParsedURL{}, denied("scheme not allowed: %s", contracts.PyRepr(parts.scheme))
	}
	if parts.hasUserinfo() {
		return ParsedURL{}, denied("credentials in the url are not allowed")
	}
	rawHost := parts.hostname()
	if IsAmbiguousIDN(rawHost) {
		return ParsedURL{}, denied("ambiguous internationalized host (IDNA 2003/2008 differ): %s",
			contracts.PyRepr(rawURL))
	}
	host := NormalizeHost(rawHost)
	if host == "" {
		return ParsedURL{}, denied("url has no valid host: %s", contracts.PyRepr(rawURL))
	}
	if !hasPort || port == 0 { // Python: ``port or DEFAULT_PORTS[scheme]``
		port = DefaultPorts[parts.scheme]
	}
	return ParsedURL{Scheme: parts.scheme, Host: host, Port: port}, nil
}

// EgressPolicy is an allow-list of permitted egress (default-deny: empty = nothing allowed).
//
// A URL is permitted iff its scheme is in AllowSchemes, its normalized host is in AllowHosts and
// its port is the scheme default or listed in AllowPorts. AllowHosts entries are normalized with
// NormalizeHost when checked (entries that do not normalize are ignored). A nil AllowSchemes
// means the default {"https", "http"}; a non-nil empty slice allows no scheme.
type EgressPolicy struct {
	AllowHosts   []string `json:"allow_hosts"`
	AllowSchemes []string `json:"allow_schemes"`
	AllowPorts   []int    `json:"allow_ports"` // beyond the scheme defaults
}

// NewEgressPolicy returns a policy allowing hosts on the default schemes and ports.
func NewEgressPolicy(hosts ...string) *EgressPolicy {
	return &EgressPolicy{AllowHosts: append([]string(nil), hosts...)}
}

func (p *EgressPolicy) allowsScheme(scheme string) bool {
	if p.AllowSchemes == nil {
		return scheme == "https" || scheme == "http"
	}
	return contains(p.AllowSchemes, scheme)
}

func (p *EgressPolicy) allowsHost(host string) bool {
	for _, h := range p.AllowHosts {
		if n := NormalizeHost(h); n != "" && n == host {
			return true
		}
	}
	return false
}

// Check returns the parsed URL if it is permitted, else an error wrapping ErrEgressDenied with
// the reason.
func (p *EgressPolicy) Check(rawURL string) (ParsedURL, error) {
	parsed, err := ParseURL(rawURL)
	if err != nil {
		return ParsedURL{}, err
	}
	if !p.allowsScheme(parsed.Scheme) {
		return ParsedURL{}, denied("scheme not allowed: %s", contracts.PyRepr(parsed.Scheme))
	}
	if !p.allowsHost(parsed.Host) {
		return ParsedURL{}, denied("host not in egress allow-list: %s", contracts.PyRepr(parsed.Host))
	}
	if parsed.Port != DefaultPorts[parsed.Scheme] {
		allowed := false
		for _, port := range p.AllowPorts {
			if port == parsed.Port {
				allowed = true
				break
			}
		}
		if !allowed {
			return ParsedURL{}, denied("port not allowed: %d", parsed.Port)
		}
	}
	return parsed, nil
}

// Permits reports whether Check accepts rawURL.
func (p *EgressPolicy) Permits(rawURL string) bool {
	_, err := p.Check(rawURL)
	return err == nil
}

// Resolver resolves (host, port) to IP address strings.
type Resolver func(ctx context.Context, host string, port int) ([]string, error)

// SystemResolver resolves host with the OS resolver (all address families). Link-local IPv6
// answers keep their "%zone" suffix, like getaddrinfo.
func SystemResolver(ctx context.Context, host string, _ int) ([]string, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		s := a.IP.String()
		if a.Zone != "" {
			s += "%" + a.Zone
		}
		out = append(out, s)
	}
	return out, nil
}

// CheckResolvedAddresses resolves host and returns an error wrapping ErrEgressDenied unless EVERY
// address is public. IP-literal hosts are checked as-is (the resolver is not called).
func CheckResolvedAddresses(ctx context.Context, host string, port int, resolve Resolver) ([]string, error) {
	if isIPLiteral(host) {
		if !IsPublicAddress(host) {
			return nil, denied("%s is a non-public address", contracts.PyRepr(host))
		}
		return []string{host}, nil
	}
	addresses, err := resolve(ctx, host, port)
	if err != nil {
		return nil, &EgressDeniedError{
			Reason: fmt.Sprintf("cannot resolve %s: %v", contracts.PyRepr(host), err), Cause: err}
	}
	if len(addresses) == 0 {
		return nil, denied("%s did not resolve", contracts.PyRepr(host))
	}
	for _, a := range addresses {
		if !IsPublicAddress(a) {
			return nil, denied("%s resolves to a non-public address: %s", contracts.PyRepr(host), a)
		}
	}
	return addresses, nil
}

// ErrUnboundCredential is returned by CredentialBroker.Register when no host normalizes.
var ErrUnboundCredential = errors.New("a brokered credential must be bound to at least one host")

type brokered struct {
	placeholder, secret string
	hosts               map[string]struct{}
}

// CredentialBroker maps placeholder tokens to real secrets, injected only at egress to their
// bound host(s). It is safe for concurrent use.
type CredentialBroker struct {
	mu      sync.RWMutex
	secrets []brokered // insertion order, like the Python dict
}

// NewCredentialBroker returns an empty broker (the zero value is also ready to use).
func NewCredentialBroker() *CredentialBroker { return &CredentialBroker{} }

// Register binds a placeholder (the agent may see it) to a secret (it never does) for hosts.
func (b *CredentialBroker) Register(placeholder, realSecret string, hosts ...string) error {
	bound := map[string]struct{}{}
	for _, h := range hosts {
		if n := NormalizeHost(h); n != "" {
			bound[n] = struct{}{}
		}
	}
	if len(bound) == 0 {
		return ErrUnboundCredential
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := brokered{placeholder, realSecret, bound}
	for i := range b.secrets {
		if b.secrets[i].placeholder == placeholder {
			b.secrets[i] = entry // re-registering keeps the original position
			return nil
		}
	}
	b.secrets = append(b.secrets, entry)
	return nil
}

// Resolve replaces the placeholders bound to host in value; others stay as placeholders.
func (b *CredentialBroker) Resolve(value, host string) string {
	target := NormalizeHost(host)
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.secrets {
		if _, ok := s.hosts[target]; ok {
			value = strings.ReplaceAll(value, s.placeholder, s.secret)
		}
	}
	return value
}

// ResolveHeaders resolves placeholders across header values for a request to host.
func (b *CredentialBroker) ResolveHeaders(headers map[string]string, host string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = b.Resolve(v, host)
	}
	return out
}

// --- urllib.parse.urlsplit (CPython 3.12.13), only what ParseURL needs ---------------------

type splitURL struct {
	scheme, netloc string
}

const whatwgC0ControlOrSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f" +
	"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

var errInvalidIPv6URL = errors.New("Invalid IPv6 URL")

func isSchemeChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '+' || r == '-' || r == '.'
}

func urlSplit(url string) (splitURL, error) {
	url = strings.TrimLeft(url, whatwgC0ControlOrSpace)
	for _, b := range []string{"\t", "\r", "\n"} {
		url = strings.ReplaceAll(url, b, "")
	}
	var out splitURL
	if i := strings.IndexByte(url, ':'); i > 0 {
		c := url[0]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			ok := true
			for _, r := range url[:i] {
				if !isSchemeChar(r) {
					ok = false
					break
				}
			}
			if ok {
				out.scheme, url = strings.ToLower(url[:i]), url[i+1:]
			}
		}
	}
	if strings.HasPrefix(url, "//") {
		delim := len(url)
		for _, c := range "/?#" {
			if w := strings.IndexRune(url[2:], c); w >= 0 && w+2 < delim {
				delim = w + 2
			}
		}
		out.netloc = url[2:delim]
		open, close := strings.Contains(out.netloc, "["), strings.Contains(out.netloc, "]")
		if open != close {
			return out, errInvalidIPv6URL
		}
		if open && close {
			if err := checkBracketedNetloc(out.netloc); err != nil {
				return out, err
			}
		}
	}
	if err := checkNetloc(out.netloc); err != nil {
		return out, err
	}
	return out, nil
}

func rpartition(s, sep string) (before string, found bool, after string) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], true, s[i+len(sep):]
	}
	return "", false, s
}

func checkBracketedNetloc(netloc string) error {
	_, _, hostAndPort := rpartition(netloc, "@")
	before, open, bracketed := partition(hostAndPort, "[")
	var hostname string
	if open {
		if before != "" {
			return errInvalidIPv6URL
		}
		var port string
		hostname, _, port = partition(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return errInvalidIPv6URL
		}
	} else {
		hostname, _, _ = partition(hostAndPort, ":")
	}
	return checkBracketedHost(hostname)
}

func checkBracketedHost(hostname string) error {
	if strings.HasPrefix(hostname, "v") {
		// re.match(r"\Av[a-fA-F0-9]+\..+\Z", hostname)
		rs := []rune(hostname)
		i := 1
		for i < len(rs) && (rs[i] >= '0' && rs[i] <= '9' || rs[i] >= 'a' && rs[i] <= 'f' || rs[i] >= 'A' && rs[i] <= 'F') {
			i++
		}
		if i == 1 || i >= len(rs) || rs[i] != '.' || i+1 >= len(rs) {
			return errors.New("IPvFuture address is invalid")
		}
		for _, r := range rs[i+1:] {
			if r == '\n' {
				return errors.New("IPvFuture address is invalid")
			}
		}
		return nil
	}
	a, err := parseIPAddress(hostname)
	if err != nil {
		return err
	}
	if !a.v6 {
		return errors.New("An IPv4 address cannot be in brackets")
	}
	return nil
}

// checkNetloc rejects non-ASCII netlocs whose NFKC form introduces a URL delimiter.
func checkNetloc(netloc string) error {
	if netloc == "" || pystr.IsASCII(netloc) {
		return nil
	}
	for _, r := range netloc {
		if strings.ContainsRune("@:#?", r) {
			continue
		}
		if pystr.HasNFKCURLDelimiter(r) {
			return fmt.Errorf("netloc %s contains invalid characters under NFKC normalization",
				"'"+netloc+"'")
		}
	}
	return nil
}

func (u splitURL) hasUserinfo() bool { return strings.Contains(u.netloc, "@") }

func (u splitURL) hostinfo() (hostname, port string) {
	_, _, hostinfo := rpartition(u.netloc, "@")
	_, open, bracketed := partition(hostinfo, "[")
	if open {
		var rest string
		hostname, _, rest = partition(bracketed, "]")
		_, _, port = partition(rest, ":")
	} else {
		hostname, _, port = partition(hostinfo, ":")
	}
	return hostname, port
}

// hostname is SplitResult.hostname ("" for None): lower-cased except a "%zone" suffix.
func (u splitURL) hostname() string {
	hostname, _ := u.hostinfo()
	if hostname == "" {
		return ""
	}
	h, pct, zone := partition(hostname, "%")
	out := pystr.Lower(h)
	if pct {
		out += "%" + zone
	}
	return out
}

// port is SplitResult.port: (0, false) for None, an error for a non-numeric or out-of-range port.
func (u splitURL) port() (int, bool, error) {
	_, port := u.hostinfo()
	if port == "" {
		return 0, false, nil
	}
	if !isASCIIDigits(port) {
		return 0, false, fmt.Errorf("Port could not be cast to integer value as %s", contracts.PyRepr(port))
	}
	digits := strings.TrimLeft(port, "0")
	if len(digits) > 5 {
		return 0, false, errors.New("Port out of range 0-65535")
	}
	n := 0
	if digits != "" {
		n, _ = strconv.Atoi(digits)
	}
	if n > 65535 {
		return 0, false, errors.New("Port out of range 0-65535")
	}
	return n, true, nil
}
