package safety

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// An HTTP transport that connects only to addresses the egress check already vetted
// (python/src/lha/safety/pinned_http.py).
//
// Checking a host's DNS answer and then letting the HTTP client resolve the name again leaves a
// DNS-rebinding window: a 0-TTL server can answer the check with a public address and the
// connection with 127.0.0.1. Here the name is resolved once, by the caller, through
// CheckResolvedAddresses; the caller pins the vetted addresses for that host on a PinnedDialer,
// and the dialer dials those addresses instead of the name. The HTTP request itself is
// unchanged: the Host header, the TLS SNI and the certificate check all use the original
// hostname (net/http takes them from the request URL, independent of the address dialled).
//
// The dialer never resolves anything: a host with no pin is refused (fail closed), and an IP
// literal is dialled only if it is public.

// DialFunc dials one TCP address ("ip:port").
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialled is one address a PinnedDialer dialled.
type Dialled struct {
	Host    string
	Address string
	Port    string
}

// PinnedDialer dials only pinned (already vetted) addresses; it never resolves a hostname.
type PinnedDialer struct {
	inner DialFunc

	mu      sync.Mutex
	pins    map[string][]string
	dialled []Dialled
}

// NewPinnedDialer wraps inner (nil = a net.Dialer with timeout) with the pinning.
func NewPinnedDialer(inner DialFunc, timeout time.Duration) *PinnedDialer {
	if inner == nil {
		inner = (&net.Dialer{Timeout: timeout}).DialContext
	}
	return &PinnedDialer{inner: inner, pins: map[string][]string{}}
}

// pyTuple renders addresses as python's repr of a tuple of str.
func pyTuple(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = contracts.PyRepr(s)
	}
	if len(parts) == 1 {
		return "(" + parts[0] + ",)"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// Pin allows connections to host only at addresses (each must be public).
func (d *PinnedDialer) Pin(host string, addresses []string) error {
	vetted := append([]string(nil), addresses...)
	ok := len(vetted) > 0
	for _, a := range vetted {
		ok = ok && IsPublicAddress(a)
	}
	if !ok {
		return fmt.Errorf("refusing to pin %s to non-public or no addresses: %s", contracts.PyRepr(host), pyTuple(vetted))
	}
	d.mu.Lock()
	d.pins[NormalizeHost(host)] = vetted
	d.mu.Unlock()
	return nil
}

// AddressesFor is the addresses DialContext may dial for host.
func (d *PinnedDialer) AddressesFor(host string) ([]string, error) {
	name := NormalizeHost(host)
	if name == "" {
		name = host
	}
	if isIPLiteral(name) {
		if !IsPublicAddress(name) {
			return nil, &EgressDeniedError{Reason: "egress: " + contracts.PyRepr(host) + " is a non-public address"}
		}
		return []string{name}, nil
	}
	d.mu.Lock()
	pinned, ok := d.pins[name]
	d.mu.Unlock()
	if !ok {
		return nil, &EgressDeniedError{Reason: "egress: " + contracts.PyRepr(host) +
			" has no vetted address (it was not checked before connect)"}
	}
	return pinned, nil
}

// Dialled returns every address dialled so far.
func (d *PinnedDialer) Dialled() []Dialled {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Dialled(nil), d.dialled...)
}

// DialContext dials host:port at its pinned addresses in order, returning the last error when
// none connects.
func (d *PinnedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, &EgressDeniedError{Reason: "egress: only TCP connections are allowed"}
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := d.AddressesFor(host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, a := range addresses {
		d.mu.Lock()
		d.dialled = append(d.dialled, Dialled{Host: NormalizeHost(host), Address: a, Port: port})
		d.mu.Unlock()
		conn, err := d.inner(ctx, network, net.JoinHostPort(a, port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// NewPinnedTransport is an HTTP/1.1 transport whose connections go through d: no proxy and no
// environment settings; TLS verification on, against the request's hostname. tlsConfig nil uses
// the system roots (tests pass their own).
func NewPinnedTransport(d *PinnedDialer, timeout time.Duration, tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // http2=False
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}

// IsEgressDenied reports an egress refusal (policy, resolution or pinning).
func IsEgressDenied(err error) bool { return errors.Is(err, ErrEgressDenied) }
