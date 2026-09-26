package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// Web tools: search + fetch — the agent's internet / deep-research capability
// (python/src/lha/execution/tools/web.py).
//
// Both are Egress (blocked unless the dispatcher enables egress) and UntrustedInput (their
// results are redacted and fenced as untrusted data; the dispatcher derives the Rule-of-Two
// UNTRUSTED_CONTENT capability from the flag). Run paths register them only through
// BuildRunDispatcher when LHA_WEB_ALLOW_HOSTS is non-empty.
//
//   - fetch_url follows the operator's EgressPolicy (default-deny host allow-list, public
//     addresses only, per-hop redirect re-checks, size cap, brokered credentials bound to hosts).
//   - web_search talks only to its configured provider endpoint (Tavily or Exa), checked the same
//     way, and its API key is injected by a CredentialBroker bound to the endpoint's host.
//
// HTTP: pass Client to share one; otherwise each call uses a client over Transport (tests inject
// a fake RoundTripper) or, by default, a transport that ignores proxy settings from the
// environment and refuses to CONNECT to a non-public address. That last check is stricter than
// the Python implementation (whose client re-resolves after the policy check, the documented
// DNS-rebinding residual risk): here the address actually dialled is re-checked.
//
// Parity notes: transport-level error texts (connection refused, TLS, timeouts) are Go's, not
// httpx's; HTTP status errors reproduce httpx's raise_for_status message exactly.

// Web search providers and their default endpoints.
var SearchEndpoints = map[string]string{
	"tavily": "https://api.tavily.com/search",
	"exa":    "https://api.exa.ai/search",
}

// Defaults for the web tools.
const (
	DefaultWebTimeoutS         = 30.0
	DefaultMaxResponseBytes    = 2_000_000
	MaxRedirects               = 5
	searchKeyPlaceholder       = "{{LHA_WEB_SEARCH_API_KEY}}"
	mozillaStatusDocsURLPrefix = "For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/"
)

// httpSeams are the network seams shared by both tools.
type httpSeams struct {
	client    *http.Client
	transport http.RoundTripper
	timeout   time.Duration
}

func newSeams(client *http.Client, transport http.RoundTripper, timeoutS float64) httpSeams {
	if timeoutS <= 0 {
		timeoutS = DefaultWebTimeoutS
	}
	return httpSeams{client: client, transport: transport, timeout: time.Duration(timeoutS * float64(time.Second))}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// open returns the client for one call: the caller's (redirects never followed) or a fresh one.
func (h httpSeams) open() *http.Client {
	if h.client != nil {
		c := *h.client
		c.CheckRedirect = noRedirects
		return &c
	}
	transport := h.transport
	if transport == nil {
		transport = publicOnlyTransport(h.timeout)
	}
	return &http.Client{Transport: transport, CheckRedirect: noRedirects}
}

// publicOnlyTransport ignores proxy environment settings (trust_env=False) and refuses to connect
// to a non-public address (the address dialled, after resolution).
func publicOnlyTransport(timeout time.Duration) *http.Transport {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil || !safety.IsPublicAddress(host) {
				return &safety.EgressDeniedError{Reason: "refusing to connect to a non-public address: " + address}
			}
			return nil
		},
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}

// httpErrorText renders a client error without Go's "Get "url": " prefix.
func httpErrorText(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return pyval.OSErrorText(err)
}

func statusErrorText(resp *http.Response, rawURL string) string {
	code := resp.StatusCode
	reason := strings.TrimPrefix(resp.Status, strconv.Itoa(code))
	reason = strings.TrimPrefix(reason, " ")
	errorType := map[int]string{1: "Informational response", 3: "Redirect response", 4: "Client error",
		5: "Server error"}[code/100]
	if errorType == "" {
		errorType = "Invalid status code"
	}
	msg := fmt.Sprintf("%s '%d %s' for url '%s'\n", errorType, code, reason, rawURL)
	if isRedirectStatus(code) && resp.Header.Get("Location") != "" {
		msg += "Redirect location: '" + resp.Header.Get("Location") + "'\n"
	}
	return msg + mozillaStatusDocsURLPrefix + strconv.Itoa(code)
}

func isRedirectStatus(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

func readCapped(body io.Reader, limit int) ([]byte, error) {
	var data []byte
	buf := make([]byte, 65_536)
	for {
		n, err := body.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) >= limit {
			return data[:limit], nil
		}
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// targetMismatch checks that the host/port the client will connect to is exactly what the policy
// checked.
func targetMismatch(u *url.URL, parsed safety.ParsedURL) string {
	actualHost := safety.NormalizeHost(u.Hostname())
	if actualHost == "" {
		return "request host is not ASCII after encoding"
	}
	actualPort := -1
	if p := u.Port(); p != "" {
		actualPort, _ = strconv.Atoi(p)
	}
	if actualPort <= 0 {
		if def, ok := safety.DefaultPorts[strings.ToLower(u.Scheme)]; ok {
			actualPort = def
		} else {
			actualPort = -1
		}
	}
	if actualHost != parsed.Host || actualPort != parsed.Port {
		return fmt.Sprintf("request target %s:%d differs from the checked %s:%d",
			actualHost, actualPort, parsed.Host, parsed.Port)
	}
	return ""
}

// pinHost rewrites u's host to the checked (IDNA-encoded) host so the client dials exactly it.
func pinHost(u *url.URL, parsed safety.ParsedURL) {
	host := parsed.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if u.Port() != "" {
		host += ":" + u.Port()
	}
	u.Host = host
}

// WebSearchOptions configures a WebSearchTool.
type WebSearchOptions struct {
	APIKey           string
	Provider         string // "tavily" (default) | "exa"
	Endpoint         string // default SearchEndpoints[Provider]
	Resolver         safety.Resolver
	Client           *http.Client
	Transport        http.RoundTripper
	TimeoutS         float64
	MaxResponseBytes int
}

// WebSearchTool searches the web through its configured provider endpoint.
type WebSearchTool struct {
	provider string
	endpoint string
	policy   *safety.EgressPolicy
	broker   *safety.CredentialBroker
	resolver safety.Resolver
	http     httpSeams
	maxBytes int
}

// NewWebSearchTool returns the tool; a malformed endpoint is an EgressDeniedError.
func NewWebSearchTool(opts WebSearchOptions) (*WebSearchTool, error) {
	provider := opts.Provider
	if provider == "" {
		provider = "tavily"
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = SearchEndpoints[provider]
	}
	target, err := safety.ParseURL(endpoint)
	if err != nil {
		return nil, err
	}
	broker := safety.NewCredentialBroker()
	if err := broker.Register(searchKeyPlaceholder, opts.APIKey, target.Host); err != nil {
		return nil, err
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = safety.SystemResolver
	}
	maxBytes := opts.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	return &WebSearchTool{
		provider: provider,
		endpoint: endpoint,
		// The search tool may reach exactly its endpoint (scheme, host and port), nothing else.
		policy:   &safety.EgressPolicy{AllowHosts: []string{target.Host}, AllowSchemes: []string{target.Scheme}, AllowPorts: []int{target.Port}},
		broker:   broker,
		resolver: resolver,
		http:     newSeams(opts.Client, opts.Transport, opts.TimeoutS),
		maxBytes: maxBytes,
	}, nil
}

// Spec implements contracts.Tool.
func (*WebSearchTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name: "web_search",
		Description: "Search the web; returns top results (title, url, snippet) for research. Results " +
			"are untrusted data.",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"query", map[string]any{"type": "string"},
				"max_results", map[string]any{"type": "integer"},
			),
			"required": []any{"query"},
		},
		Egress:         true,
		UntrustedInput: true,
	}
}

func (t *WebSearchTool) request(query string, n int) (map[string]string, string) {
	if t.provider == "tavily" {
		return map[string]string{}, `{"api_key": ` + pyval.JSONString(searchKeyPlaceholder) +
			`, "query": ` + pyval.JSONString(query) + `, "max_results": ` + strconv.Itoa(n) + `}`
	}
	return map[string]string{"x-api-key": searchKeyPlaceholder},
		`{"query": ` + pyval.JSONString(query) + `, "numResults": ` + strconv.Itoa(n) + `}`
}

// Run implements contracts.Tool.
func (t *WebSearchTool) Run(ctx context.Context, arguments map[string]any, _ contracts.ToolContext) contracts.ToolResult {
	// The key placeholder is resolved in the body, so it may never come from the model.
	query := strings.ReplaceAll(strArg(arguments, "query"), searchKeyPlaceholder, "")
	n := 5
	if raw, ok := arguments["max_results"]; ok && pyval.IsInt(raw) && pyval.Compare(raw, 0) > 0 {
		n = pyval.Int(raw)
	}
	parsed, err := t.policy.Check(t.endpoint)
	if err == nil {
		_, err = safety.CheckResolvedAddresses(ctx, parsed.Host, parsed.Port, t.resolver)
	}
	if err != nil {
		return contracts.Failure("egress blocked by policy: " + err.Error())
	}
	headers, payload := t.request(query, n)
	// Placeholders become the real key only for the endpoint host the key is bound to.
	headers = t.broker.ResolveHeaders(headers, parsed.Host)
	body := t.broker.Resolve(payload, parsed.Host)
	u, err := url.Parse(t.endpoint)
	if err != nil {
		return contracts.Failure("InvalidURL: " + err.Error())
	}
	if mismatch := targetMismatch(u, parsed); mismatch != "" {
		return contracts.Failure("egress blocked by policy: " + mismatch)
	}
	pinHost(u, parsed)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return contracts.Failure("InvalidURL: " + err.Error())
	}
	for _, k := range pyval.SortedKeys(headers) {
		req.Header.Set(k, headers[k])
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Accept", "*/*")
	resp, err := t.http.open().Do(req)
	if err != nil {
		return contracts.Failure("web_search failed: " + httpErrorText(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return contracts.Failure("web_search failed: " + statusErrorText(resp, t.endpoint))
	}
	raw, err := readCapped(resp.Body, t.maxBytes)
	if err != nil {
		return contracts.Failure("web_search failed: " + httpErrorText(err))
	}
	data, err := decodePyJSON(pyval.DecodeUTF8Replace(raw))
	if err != nil {
		return contracts.Failure("web_search failed: invalid JSON response (" + err.Error() + ")")
	}
	var results []any
	if obj, ok := data.(map[string]any); ok {
		switch r := obj["results"].(type) {
		case nil:
			if _, present := obj["results"]; present {
				return contracts.Failure("TypeError: 'NoneType' object is not subscriptable")
			}
		case []any:
			results = r
		case map[string]any:
			return contracts.Failure("KeyError: slice(None, " + strconv.Itoa(n) + ", None)")
		case string:
			// Slicing a str yields characters, none of which is a dict.
		default:
			return contracts.Failure("TypeError: '" + pyval.TypeName(r) + "' object is not subscriptable")
		}
	}
	if len(results) > n {
		results = results[:n]
	}
	var lines []string
	for _, item := range results {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title := firstTruthy(obj, "title")
		link := firstTruthy(obj, "url")
		snippet := firstTruthy(obj, "content", "snippet", "text")
		lines = append(lines, "- "+title+"\n  "+link+"\n  "+pyval.Head(snippet, 300))
	}
	text := "(no results)"
	if len(lines) > 0 {
		text = strings.Join(lines, "\n")
	}
	return contracts.Success(MarkUntrusted(pyval.Head(text, MaxToolOutput), "web_search:"+t.provider))
}

// firstTruthy is `obj.get(k1) or obj.get(k2) or ... or ""`, as str().
func firstTruthy(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k]; ok && pyval.Truthy(v) {
			return pyval.Str(v)
		}
	}
	return ""
}

var forbiddenHeaders = map[string]bool{"host": true, "connection": true, "content-length": true,
	"transfer-encoding": true}

// FetchURLOptions configures a FetchURLTool.
type FetchURLOptions struct {
	// Policy nil denies everything (fail closed).
	Policy   *safety.EgressPolicy
	Broker   *safety.CredentialBroker
	Resolver safety.Resolver
	Client   *http.Client
	// Transport replaces the default public-only transport (tests).
	Transport        http.RoundTripper
	TimeoutS         float64
	MaxResponseBytes int
	// Placeholders is {placeholder: bound hosts}, shown to the model in the description.
	Placeholders map[string][]string
}

// FetchURLTool fetches a URL under a default-deny egress policy with SSRF protection:
// redirects are NOT auto-followed (each hop, max MaxRedirects, is re-checked against the policy
// and re-resolved), every hop's host must resolve only to public addresses, proxy settings from
// the environment are ignored, brokered credentials are substituted only into requests to the
// host they are bound to, the body is read up to MaxResponseBytes, and the page text is redacted
// and fenced as untrusted content.
type FetchURLTool struct {
	spec     contracts.ToolSpec
	policy   *safety.EgressPolicy
	broker   *safety.CredentialBroker
	resolver safety.Resolver
	http     httpSeams
	maxBytes int
}

func fetchURLSpec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name: "fetch_url",
		Description: "Fetch a URL and return its readable text content (HTML stripped; untrusted data). " +
			"Optional 'headers' may contain credential placeholders, filled in only for their " +
			"bound host.",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"url", map[string]any{"type": "string"},
				"headers", pyfmt.NewOrderedMap("type", "object", "additionalProperties", map[string]any{"type": "string"}),
			),
			"required": []any{"url"},
		},
		Egress:         true,
		UntrustedInput: true,
	}
}

// NewFetchURLTool returns the tool.
func NewFetchURLTool(opts FetchURLOptions) *FetchURLTool {
	spec := fetchURLSpec()
	if opts.Policy != nil && len(opts.Policy.AllowHosts) > 0 {
		hosts := uniqueSorted(opts.Policy.AllowHosts)
		extra := " Allowed hosts: " + strings.Join(hosts, ", ") + "."
		if len(opts.Placeholders) > 0 {
			var creds []string
			for _, name := range pyval.SortedKeys(opts.Placeholders) {
				creds = append(creds, name+" (for "+strings.Join(uniqueSorted(opts.Placeholders[name]), ", ")+")")
			}
			extra += " Credential placeholders: " + strings.Join(creds, "; ") + "."
		}
		spec.Description += extra
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = safety.SystemResolver
	}
	maxBytes := opts.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	return &FetchURLTool{spec: spec, policy: opts.Policy, broker: opts.Broker, resolver: resolver,
		http: newSeams(opts.Client, opts.Transport, opts.TimeoutS), maxBytes: maxBytes}
}

func uniqueSorted(items []string) []string {
	set := map[string]bool{}
	for _, s := range items {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Spec implements contracts.Tool.
func (t *FetchURLTool) Spec() contracts.ToolSpec { return t.spec }

// Run implements contracts.Tool.
func (t *FetchURLTool) Run(ctx context.Context, arguments map[string]any, _ contracts.ToolContext) contracts.ToolResult {
	rawURL := strArg(arguments, "url")
	rawHeaders := map[string]string{}
	if v, ok := arguments["headers"]; ok && pyval.Truthy(v) {
		switch h := v.(type) {
		case map[string]string:
			for k, s := range h {
				rawHeaders[k] = s
			}
		case map[string]any:
			for k, s := range h {
				str, isStr := s.(string)
				if !isStr {
					return contracts.Failure("'headers' must be an object of string values")
				}
				rawHeaders[k] = str
			}
		default:
			return contracts.Failure("'headers' must be an object of string values")
		}
	}
	for _, k := range pyval.SortedKeys(rawHeaders) {
		lower := pystr.Lower(k)
		if forbiddenHeaders[lower] || strings.HasPrefix(lower, "proxy-") {
			return contracts.Failure("header not allowed: " + contracts.PyRepr(k))
		}
	}
	if t.policy == nil {
		return contracts.Failure("egress blocked: no egress policy configured (default-deny)")
	}
	client := t.http.open()
	for hop := 0; hop <= MaxRedirects; hop++ {
		parsed, err := t.policy.Check(rawURL)
		if err == nil {
			_, err = safety.CheckResolvedAddresses(ctx, parsed.Host, parsed.Port, t.resolver)
		}
		if err != nil {
			return contracts.Failure("egress blocked by policy: " + err.Error() + " (" + contracts.PyRepr(rawURL) + ")")
		}
		headers := rawHeaders
		if t.broker != nil {
			headers = t.broker.ResolveHeaders(headers, parsed.Host)
		}
		u, err := url.Parse(pystr.Strip(rawURL))
		if err != nil {
			return contracts.Failure("InvalidURL: " + err.Error())
		}
		if mismatch := targetMismatch(u, parsed); mismatch != "" {
			return contracts.Failure("egress blocked by policy: " + mismatch + " (" + contracts.PyRepr(rawURL) + ")")
		}
		requestURL := *u
		pinHost(&requestURL, parsed)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
		if err != nil {
			return contracts.Failure("InvalidURL: " + err.Error())
		}
		req.Header.Set("Accept", "*/*")
		for _, k := range pyval.SortedKeys(headers) {
			req.Header.Set(k, headers[k])
		}
		resp, err := client.Do(req)
		if err != nil {
			return contracts.Failure("fetch_url failed: " + httpErrorText(err))
		}
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 { // httpx is_redirect: any 3xx
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if location == "" {
				return contracts.Failure("fetch_url failed: redirect without location")
			}
			ref, err := url.Parse(location)
			if err != nil {
				return contracts.Failure("fetch_url failed: " + err.Error())
			}
			rawURL = u.ResolveReference(ref).String()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			resp.Body.Close()
			return contracts.Failure("fetch_url failed: " + statusErrorText(resp, rawURL))
		}
		body, err := readCapped(resp.Body, t.maxBytes)
		resp.Body.Close()
		if err != nil {
			return contracts.Failure("fetch_url failed: " + httpErrorText(err))
		}
		text := decodeBody(body, resp.Header.Get("Content-Type"))
		return contracts.Success(MarkUntrusted(pyval.Head(HTMLToText(text), MaxToolOutput), rawURL))
	}
	return contracts.Failure(fmt.Sprintf("fetch_url failed: more than %d redirects", MaxRedirects))
}

// decodeBody decodes a response body like httpx's Response.text: the Content-Type charset when
// it is a known encoding, else UTF-8, with undecodable bytes replaced.
func decodeBody(body []byte, contentType string) string {
	label := ""
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		label = strings.ToLower(strings.TrimSpace(params["charset"]))
	}
	switch strings.ReplaceAll(label, "_", "-") {
	case "", "utf-8", "utf8", "u8", "utf":
		return pyval.DecodeUTF8Replace(body)
	case "ascii", "us-ascii", "646":
		var b strings.Builder
		for _, c := range body {
			if c < 0x80 {
				b.WriteByte(c)
			} else {
				b.WriteString("�")
			}
		}
		return b.String()
	case "latin-1", "latin1", "iso-8859-1", "iso8859-1", "8859", "cp819", "l1", "latin":
		rs := make([]rune, len(body))
		for i, c := range body {
			rs[i] = rune(c)
		}
		return string(rs)
	}
	if enc, _ := charset.Lookup(label); enc != nil {
		if out, err := enc.NewDecoder().Bytes(body); err == nil {
			return string(out)
		}
	}
	return pyval.DecodeUTF8Replace(body)
}

// HTMLToText strips script/style elements and tags and collapses whitespace:
//
//	re.sub(r"(?is)<(script|style)\b.*?</\1>", " ", html)
//	re.sub(r"(?s)<[^>]+>", " ", ...); re.sub(r"\s+", " ", ...).strip()
func HTMLToText(html string) string {
	html = stripScriptStyle(html)
	var b strings.Builder
	for i := 0; i < len(html); {
		if html[i] == '<' {
			if j := strings.IndexByte(html[i+1:], '>'); j > 0 {
				b.WriteByte(' ')
				i += j + 2
				continue
			}
		}
		b.WriteByte(html[i])
		i++
	}
	var out strings.Builder
	inSpace := false
	for _, r := range b.String() {
		if pystr.IsSpace(r) {
			if !inSpace {
				out.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		out.WriteRune(r)
	}
	return pystr.Strip(out.String())
}

// matchWordCI matches word (ASCII lowercase letters) at s[i:] with re.IGNORECASE rules; it returns
// the end offset or -1.
func matchWordCI(s string, i int, word string) int {
	for j := 0; j < len(word); j++ {
		if i >= len(s) {
			return -1
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if !pystr.FoldsToASCIILetter(r, word[j]) {
			return -1
		}
		i += size
	}
	return i
}

func isWordAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return pystr.IsWord(r)
}

// stripScriptStyle is re.sub(r"(?is)<(script|style)\b.*?</\1>", " ", html).
func stripScriptStyle(html string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(html); {
		if html[i] != '<' {
			i++
			continue
		}
		matched := false
		for _, tag := range []string{"script", "style"} {
			end := matchWordCI(html, i+1, tag)
			if end < 0 || isWordAt(html, end) {
				continue
			}
			// .*? then "</" + the same tag text (case-insensitively)
			for k := end; k < len(html); k++ {
				if html[k] == '<' && k+1 < len(html) && html[k+1] == '/' {
					if closeEnd := matchWordCI(html, k+2, tag); closeEnd >= 0 && closeEnd < len(html) && html[closeEnd] == '>' {
						b.WriteString(html[last:i])
						b.WriteByte(' ')
						i, last = closeEnd+1, closeEnd+1
						matched = true
						break
					}
				}
			}
			break
		}
		if !matched {
			i++
		}
	}
	b.WriteString(html[last:])
	return b.String()
}
