package egressproxy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// splitResult is the subset of CPython's urllib.parse.SplitResult the proxy uses.
type splitResult struct {
	scheme, netloc, path, query, fragment string
}

const whatwgC0ControlOrSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f" +
	"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

var errInvalidIPv6 = errors.New("Invalid IPv6 URL")

func isSchemeChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '+' || r == '-' || r == '.'
}

// urlsplit is urllib.parse.urlsplit(url) (CPython 3.12). The NFKC netloc check is not
// reproduced: such hosts are non-ASCII and the host check denies them anyway.
func urlsplit(url string) (splitResult, error) {
	var out splitResult
	url = strings.TrimLeft(url, whatwgC0ControlOrSpace)
	for _, b := range []string{"\t", "\r", "\n"} {
		url = strings.ReplaceAll(url, b, "")
	}
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
		out.netloc, url = url[2:delim], url[delim:]
		open, closeBr := strings.Contains(out.netloc, "["), strings.Contains(out.netloc, "]")
		if open != closeBr {
			return out, errInvalidIPv6
		}
		if open && closeBr {
			if err := checkBracketedNetloc(out.netloc); err != nil {
				return out, err
			}
		}
	}
	if before, after, ok := strings.Cut(url, "#"); ok {
		url, out.fragment = before, after
	}
	if before, after, ok := strings.Cut(url, "?"); ok {
		url, out.query = before, after
	}
	out.path = url
	return out, nil
}

func checkBracketedNetloc(netloc string) error {
	hostAndPort := netloc[strings.LastIndex(netloc, "@")+1:]
	before, bracketed, open := strings.Cut(hostAndPort, "[")
	var hostname string
	if open {
		if before != "" {
			return errInvalidIPv6
		}
		var port string
		hostname, port, _ = strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return errInvalidIPv6
		}
	} else {
		hostname, _, _ = strings.Cut(hostAndPort, ":")
	}
	if strings.HasPrefix(hostname, "v") {
		// re.match(r"\Av[a-fA-F0-9]+\..+\Z", hostname)
		i := 1
		for i < len(hostname) && strings.ContainsRune("0123456789abcdefABCDEF", rune(hostname[i])) {
			i++
		}
		if i == 1 || i >= len(hostname)-1 || hostname[i] != '.' || strings.Contains(hostname[i+1:], "\n") {
			return errors.New("IPvFuture address is invalid")
		}
		return nil
	}
	if !safety.IsIPLiteral(hostname) {
		return fmt.Errorf("%s does not appear to be an IPv4 or IPv6 address", contracts.PyRepr(hostname))
	}
	if !strings.Contains(hostname, ":") {
		return errors.New("An IPv4 address cannot be in brackets")
	}
	return nil
}

func (s splitResult) hostinfo() (hostname, port string, hasPort bool) {
	hostinfo := s.netloc[strings.LastIndex(s.netloc, "@")+1:]
	if _, bracketed, open := strings.Cut(hostinfo, "["); open {
		var rest string
		hostname, rest, _ = strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
	} else {
		hostname, port, _ = strings.Cut(hostinfo, ":")
	}
	return hostname, port, port != ""
}

// hostname is SplitResult.hostname ("" for None).
func (s splitResult) hostname() string {
	hostname, _, _ := s.hostinfo()
	if hostname == "" {
		return ""
	}
	h, zone, pct := strings.Cut(hostname, "%")
	out := pystr.Lower(h)
	if pct {
		out += "%" + zone
	}
	return out
}

// port is SplitResult.port: hasPort=false for None; a ValueError-like error when invalid.
func (s splitResult) port() (int, bool, error) {
	_, port, has := s.hostinfo()
	if !has {
		return 0, false, nil
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return 0, false, fmt.Errorf("Port could not be cast to integer value as %s", contracts.PyRepr(port))
		}
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

// hasUsername / hasPassword are "username is not None" / "password is not None".
func (s splitResult) hasUsername() bool { return strings.Contains(s.netloc, "@") }

func (s splitResult) hasPassword() bool {
	i := strings.LastIndex(s.netloc, "@")
	return i >= 0 && strings.Contains(s.netloc[:i], ":")
}
