// Package vendor snapshots reference material into a mission workspace (`lha vendor`;
// python/src/lha/state/vendor.py).
//
// A clean-room project depends on outside knowledge (API docs, schemas, SDK behaviour). Rather
// than opening the sandbox to the web, the operator snapshots the pages it needs into the
// workspace, once, and lists them in the mission's references: the agent reads them offline,
// every cycle can cite the same bytes, and the snapshot is reviewable and pinned by a SHA-256
// manifest.
//
// Fetching goes through the same egress rules as fetch_url: http(s) only, no credentials in the
// URL, IDNA 2008 host normalization, public addresses only, and every redirect hop re-checked
// against the hosts the operator named. Each hop's host is resolved once and only a vetted
// address is dialled (safety.PinnedDialer: no DNS rebinding). Pages are stored raw; HTML
// additionally gets a .txt rendering. The on-disk layout and MANIFEST.json bytes match Python's.
package vendor

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// Limits and names (python: MANIFEST, MAX_BYTES, MAX_REDIRECTS).
const (
	Manifest     = "MANIFEST.json"
	MaxBytes     = 10_000_000
	MaxRedirects = 5
	fetchTimeout = 60 * time.Second
)

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Error is a URL that could not be vendored (policy, network or size); python: VendorError.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// PyTypeName names the Python exception type.
func (e *Error) PyTypeName() string { return "VendorError" }

// VendoredFile is one saved URL (a MANIFEST.json entry).
type VendoredFile struct {
	URL         string `json:"url"`
	Path        string `json:"path"` // relative to the vendor directory
	SHA256      string `json:"sha256"`
	Bytes       int    `json:"bytes"`
	ContentType string `json:"content_type"`
	FetchedAt   string `json:"fetched_at"`
	TextPath    string `json:"text_path"`
}

// Options are VendorURLs' seams.
type Options struct {
	// Client replaces the pinned HTTP client entirely (a test seam, like python's client=):
	// addresses are still checked, but not pinned.
	Client *http.Client
	// Resolver resolves each hop's host (nil = safety.SystemResolver).
	Resolver safety.Resolver
	// Dial replaces the socket layer under the pinning (tests; python: network_backend).
	Dial safety.DialFunc
	// TLSConfig replaces the TLS client configuration (tests with their own CA).
	TLSConfig *tls.Config
	// Now is the clock for fetched_at (nil = time.Now).
	Now func() time.Time
	// Dialer receives the pinned dialer VendorURLs uses (tests inspect what was dialled).
	Dialer func(*safety.PinnedDialer)
}

// VendorURLs fetches urls into dir `into` and (re)writes its MANIFEST.json; it returns what was
// saved. A URL the policy refuses up front is a *safety.EgressDeniedError; a failed fetch is an
// *Error naming the URL. Pages saved before a failure stay on disk; the manifest is written
// only when every URL succeeded.
func VendorURLs(ctx context.Context, urls []string, into string, o Options) ([]VendoredFile, error) {
	if err := os.MkdirAll(into, 0o777); err != nil {
		return nil, err
	}
	var allowed []string // the operator's own URLs define the allow-list
	for _, u := range urls {
		parsed, err := safety.ParseURL(u)
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, parsed.Host)
	}
	policy := safety.NewEgressPolicy(allowed...)
	resolver := o.Resolver
	if resolver == nil {
		resolver = safety.SystemResolver
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	client := o.Client
	var dialer *safety.PinnedDialer
	if client == nil {
		dialer = safety.NewPinnedDialer(o.Dial, fetchTimeout)
		if o.Dialer != nil {
			o.Dialer(dialer)
		}
		transport := safety.NewPinnedTransport(dialer, fetchTimeout, o.TLSConfig)
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport}
	} else {
		c := *client
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	var saved []VendoredFile
	for _, u := range urls {
		item, err := vendorOne(ctx, client, policy, resolver, dialer, u, into, now)
		if err != nil {
			return saved, err
		}
		saved = append(saved, item)
	}
	if err := writeManifest(into, saved); err != nil {
		return saved, err
	}
	return saved, nil
}

func vendorOne(ctx context.Context, client *http.Client, policy *safety.EgressPolicy, resolver safety.Resolver,
	dialer *safety.PinnedDialer, rawURL, root string, now func() time.Time) (VendoredFile, error) {
	current := rawURL
	for hop := 0; hop <= MaxRedirects; hop++ {
		parsed, err := policy.Check(current)
		var addresses []string
		if err == nil {
			addresses, err = safety.CheckResolvedAddresses(ctx, parsed.Host, parsed.Port, resolver)
		}
		if err != nil {
			return VendoredFile{}, &Error{current + ": " + err.Error()}
		}
		if dialer != nil {
			if err := dialer.Pin(parsed.Host, addresses); err != nil { // the connection dials only these
				return VendoredFile{}, err
			}
		}
		u, err := url.Parse(strings.TrimSpace(current))
		if err != nil {
			return VendoredFile{}, &Error{current + ": " + err.Error()}
		}
		request := *u
		request.Host = checkedHost(u, parsed) // dial exactly the host the policy checked
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, request.String(), nil)
		if err != nil {
			return VendoredFile{}, &Error{current + ": " + err.Error()}
		}
		resp, err := client.Do(req)
		if err != nil {
			return VendoredFile{}, &Error{current + ": " + tools.HTTPErrorText(err)}
		}
		// httpx is_redirect: 301/302/303/307/308 with a Location header.
		if location, has := resp.Header["Location"]; has && isRedirect(resp.StatusCode) {
			resp.Body.Close()
			loc := strings.Join(location, ", ")
			if loc == "" {
				return VendoredFile{}, &Error{current + ": redirect without location"}
			}
			ref, err := url.Parse(loc)
			if err != nil {
				return VendoredFile{}, &Error{current + ": " + err.Error()}
			}
			current = u.ResolveReference(ref).String()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			resp.Body.Close()
			return VendoredFile{}, &Error{current + ": " + tools.HTTPStatusErrorText(resp, current)}
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
		resp.Body.Close()
		if err != nil {
			return VendoredFile{}, &Error{current + ": " + tools.HTTPErrorText(err)}
		}
		if len(data) > MaxBytes {
			return VendoredFile{}, &Error{fmt.Sprintf("%s: larger than %d bytes", current, MaxBytes)}
		}
		header := strings.Join(resp.Header.Values("Content-Type"), ", ")
		contentType, _, _ := strings.Cut(header, ";")
		contentType = pyfmt.PyStrip(contentType)
		rel, err := TargetPath(current, contentType)
		if err != nil {
			return VendoredFile{}, &Error{current + ": " + err.Error()}
		}
		dest := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
			return VendoredFile{}, err
		}
		if err := os.WriteFile(dest, data, 0o666); err != nil {
			return VendoredFile{}, err
		}
		textRel := ""
		if strings.Contains(contentType, "html") {
			textRel = rel + ".txt"
			text := tools.HTMLToText(tools.DecodeBody(data, header))
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(textRel)), []byte(text+"\n"), 0o666); err != nil {
				return VendoredFile{}, err
			}
		}
		sum := sha256.Sum256(data)
		return VendoredFile{
			URL:         rawURL,
			Path:        rel,
			SHA256:      hex.EncodeToString(sum[:]),
			Bytes:       len(data),
			ContentType: contentType,
			FetchedAt:   now().UTC().Format("2006-01-02T15:04:05+00:00"),
			TextPath:    textRel,
		}, nil
	}
	return VendoredFile{}, &Error{fmt.Sprintf("%s: more than %d redirects", rawURL, MaxRedirects)}
}

func isRedirect(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// checkedHost is the URL's host:port with the host replaced by the policy-normalized one.
func checkedHost(u *url.URL, parsed safety.ParsedURL) string {
	host := parsed.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	return host
}

// TargetPath is where a fetched URL is stored, relative to the vendor directory (python:
// _target_path): <host>/<sanitized path segments>[_<sha256(query)[:8]>][.html].
func TargetPath(rawURL, contentType string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	decoded, err := url.PathUnescape(removeDotSegments(u.EscapedPath()))
	if err != nil {
		return "", err
	}
	var parts []string
	for _, p := range strings.Split(decoded, "/") {
		if p == "" || p == "." || p == ".." {
			continue
		}
		part := strings.Trim(unsafeChars.ReplaceAllString(p, "_"), "._")
		if part == "" {
			part = "_"
		}
		parts = append(parts, part)
	}
	name := strings.Join(parts, "/")
	if name == "" {
		name = "index"
	}
	if query := httpxQuery(u.RawQuery); query != "" {
		sum := sha256.Sum256([]byte(query))
		name += "_" + hex.EncodeToString(sum[:])[:8]
	}
	last := name[strings.LastIndex(name, "/")+1:]
	if !strings.Contains(last, ".") && strings.Contains(contentType, "html") {
		name += ".html"
	}
	return path.Join(unsafeChars.ReplaceAllString(httpxHost(u.Hostname()), "_"), name), nil
}

// httpxHost is httpx's URL.host: lowercase, and IDNA-decoded when it starts with "xn--".
func httpxHost(host string) string {
	host = strings.ToLower(host)
	if strings.HasPrefix(host, "xn--") {
		if decoded, err := idna.Punycode.ToUnicode(host); err == nil {
			return decoded
		}
	}
	return host
}

// httpxQuery is the query as httpx stores it: characters outside the RFC 3986 query set
// percent-encoded (uppercase hex); existing escapes kept.
func httpxQuery(raw string) string {
	const safe = "!$&'()*+,;=:@/?%-._~"
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// removeDotSegments is RFC 3986 section 5.2.4 (httpx normalizes paths this way).
func removeDotSegments(p string) string {
	if !strings.Contains(p, ".") {
		return p
	}
	in := p
	var out []string
	for in != "" {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = in[2:]
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = in[3:]
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		case in == "/..":
			in = "/"
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		case in == "." || in == "..":
			in = ""
		default:
			start := 0
			if in[0] == '/' {
				start = 1
			}
			end := strings.IndexByte(in[start:], '/')
			if end < 0 {
				end = len(in)
			} else {
				end += start
			}
			out = append(out, in[:end])
			in = in[end:]
		}
	}
	return strings.Join(out, "")
}

// --- manifest --------------------------------------------------------------------------------

// writeManifest merges saved into the manifest (a re-vendored URL replaces its old entry).
func writeManifest(root string, saved []VendoredFile) error {
	manifestPath := filepath.Join(root, Manifest)
	keys, entries := loadManifest(manifestPath)
	for _, item := range saved {
		entry := pyfmt.NewOrderedMap(
			"url", item.URL, "path", item.Path, "sha256", item.SHA256, "bytes", item.Bytes,
			"content_type", item.ContentType, "fetched_at", item.FetchedAt, "text_path", item.TextPath)
		if _, seen := entries[item.URL]; !seen {
			keys = append(keys, item.URL)
		}
		entries[item.URL] = entry
	}
	files := make([]*pyfmt.OrderedMap, len(keys))
	for i, k := range keys {
		files[i] = entries[k]
	}
	sort.SliceStable(files, func(i, j int) bool {
		return pyfmt.PyStr(files[i].Values["url"]) < pyfmt.PyStr(files[j].Values["url"])
	})
	list := make([]any, len(files))
	for i, f := range files {
		list[i] = f
	}
	var b strings.Builder
	writeJSON(&b, pyfmt.NewOrderedMap("files", list), "")
	b.WriteString("\n")
	return os.WriteFile(manifestPath, []byte(b.String()), 0o666)
}

// loadManifest reads the existing entries keyed by str(url), in file order; anything malformed
// starts a fresh manifest (python resets on ValueError/KeyError/TypeError/AttributeError).
func loadManifest(manifestPath string) ([]string, map[string]*pyfmt.OrderedMap) {
	entries := map[string]*pyfmt.OrderedMap{}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, entries
	}
	decoded, err := pyfmt.DecodeOrdered(data)
	top, ok := decoded.(*pyfmt.OrderedMap)
	if err != nil || !ok {
		return nil, entries
	}
	files, ok := top.Values["files"].([]any)
	if !ok {
		return nil, entries
	}
	var keys []string
	for _, f := range files {
		entry, ok := f.(*pyfmt.OrderedMap)
		if !ok {
			return nil, map[string]*pyfmt.OrderedMap{}
		}
		u, has := entry.Values["url"]
		if !has {
			return nil, map[string]*pyfmt.OrderedMap{}
		}
		key := pyfmt.PyStr(u)
		if _, seen := entries[key]; !seen {
			keys = append(keys, key)
		}
		entries[key] = entry
	}
	return keys, entries
}

// writeJSON is python's json.dumps(v, indent=2) (ensure_ascii=True) for decoded values.
func writeJSON(b *strings.Builder, v any, indent string) {
	inner := indent + "  "
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeJSONString(b, x)
	case int:
		b.WriteString(strconv.Itoa(x))
	case json.Number:
		// python: ints stay ints, floats are repr()'d, and an overflowing float is (-)Infinity
		if f, _ := strconv.ParseFloat(string(x), 64); strings.ContainsAny(string(x), ".eE") && math.IsInf(f, 0) {
			if f < 0 {
				b.WriteString("-Infinity")
			} else {
				b.WriteString("Infinity")
			}
			return
		}
		b.WriteString(pyfmt.PyReprValue(x))
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[")
		for i, e := range x {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("\n" + inner)
			writeJSON(b, e, inner)
		}
		b.WriteString("\n" + indent + "]")
	case *pyfmt.OrderedMap:
		if len(x.Keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{")
		for i, k := range x.Keys {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("\n" + inner)
			writeJSONString(b, k)
			b.WriteString(": ")
			writeJSON(b, x.Values[k], inner)
		}
		b.WriteString("\n" + indent + "}")
	default:
		writeJSONString(b, fmt.Sprint(v))
	}
}

// writeJSONString mirrors json.encoder.py_encode_basestring_ascii.
func writeJSONString(b *strings.Builder, s string) {
	const hexd = "0123456789abcdef"
	u4 := func(r rune) {
		b.WriteString(`\u`)
		for shift := 12; shift >= 0; shift -= 4 {
			b.WriteByte(hexd[(r>>uint(shift))&0xf])
		}
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= ' ' && r <= '~':
				b.WriteRune(r)
			case r > 0xffff:
				r -= 0x10000
				u4(0xd800 | (r>>10)&0x3ff)
				u4(0xdc00 | r&0x3ff)
			default:
				u4(r)
			}
		}
	}
	b.WriteByte('"')
}

// IsEgressDenied reports a URL the egress policy refused before anything was fetched.
func IsEgressDenied(err error) bool {
	var denied *safety.EgressDeniedError
	return errors.As(err, &denied)
}
