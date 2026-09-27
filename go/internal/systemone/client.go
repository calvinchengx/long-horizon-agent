package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety"
)

// The System One HTTP client (python: lha.systemone.client). One client covers every backend that
// speaks TypeSafe's API: the hosted endpoint and self-hosted servers such as Kev.
//
// A remote endpoint must be https; every request re-resolves its host, refuses non-public
// addresses and dials only the vetted ones (safety.PinnedDialer), and the API key is bound to the
// host by a CredentialBroker and scrubbed from errors. A loopback endpoint (localhost,
// *.localhost, 127.0.0.0/8, ::1) may use http and is dialled directly.

const (
	// TypeSafeEndpoint is the hosted System One endpoint.
	TypeSafeEndpoint = "https://api.typesafe.ai/v1/systemone"
	// TypeSafePriceInPerMTok is TypeSafe's price for Jev (USD per million input tokens; output
	// tokens are free).
	TypeSafePriceInPerMTok = 0.042
	// Role is the ledger role System One calls are recorded under.
	Role = "system_one"

	keyPlaceholder = "{{LHA_SYSTEM_ONE_API_KEY}}"
	charsPerToken  = 2.0
	maxResponse    = 16 << 20
)

// Retry is the retry policy of every request: answers are advisory, so one quick retry only.
var Retry = model.RetryPolicy{MaxRetries: 1, BaseDelaySeconds: 0.25, MaxDelaySeconds: 2.0}

// Model is a System One backend (python: SystemOneModel).
type Model interface {
	Name() string
	// Evaluate answers every question against state; any failure is an *Error.
	Evaluate(ctx context.Context, state any, questions []Named) (Result, error)
}

// IsLocalEndpoint reports a loopback endpoint.
func IsLocalEndpoint(target safety.ParsedURL) bool {
	host := strings.Trim(target.Host, "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DefaultPriceInPerMTok is the input price when none is configured: Jev's for TypeSafe's host,
// $0 for loopback, nil (unknown) for anything else.
func DefaultPriceInPerMTok(endpoint string) (*float64, error) {
	target, err := safety.ParseURL(endpoint)
	if err != nil {
		return nil, err
	}
	typesafe, _ := safety.ParseURL(TypeSafeEndpoint)
	switch {
	case target.Host == typesafe.Host:
		p := TypeSafePriceInPerMTok
		return &p, nil
	case IsLocalEndpoint(target):
		p := 0.0
		return &p, nil
	}
	return nil, nil
}

// Options configure NewClient.
type Options struct {
	Model    string
	Endpoint string // "" => TypeSafeEndpoint
	APIKey   string
	// PriceInPerMTok is USD per million input tokens (nil: unknown).
	PriceInPerMTok *float64
	Timeout        time.Duration // <= 0 => 5s
	Meter          *governor.CostMeter
	Role           string // "" => Role
	// Resolver, Transport and Dial are test seams (see memory.VoyageOptions).
	Resolver  safety.Resolver
	Transport http.RoundTripper
	Dial      safety.DialFunc
	Sleep     model.SleepFunc
}

// Client is a metered client for one System One endpoint and model.
type Client struct {
	model    string
	endpoint string
	key      string
	price    *float64
	meter    *governor.CostMeter
	role     string
	local    bool
	policy   *safety.EgressPolicy
	broker   *safety.CredentialBroker
	resolver safety.Resolver
	dialer   *safety.PinnedDialer // nil for loopback or a Transport seam
	client   *http.Client
	retry    model.RetryPolicy
}

// NewClient builds a client; a malformed endpoint, or a remote one that is not https, is an
// egress refusal.
func NewClient(o Options) (*Client, error) {
	endpoint := o.Endpoint
	if endpoint == "" {
		endpoint = TypeSafeEndpoint
	}
	target, err := safety.ParseURL(endpoint)
	if err != nil {
		return nil, err
	}
	local := IsLocalEndpoint(target)
	if !local && target.Scheme != "https" {
		return nil, &safety.EgressDeniedError{Reason: "a remote System One endpoint must use https: " + contracts.PyRepr(endpoint)}
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	role := o.Role
	if role == "" {
		role = Role
	}
	c := &Client{model: o.Model, endpoint: endpoint, key: o.APIKey, price: o.PriceInPerMTok, meter: o.Meter,
		role: role, local: local, broker: safety.NewCredentialBroker(), retry: Retry,
		policy: &safety.EgressPolicy{AllowHosts: []string{target.Host}, AllowSchemes: []string{target.Scheme}, AllowPorts: []int{target.Port}}}
	if o.Sleep != nil {
		c.retry.Sleep = o.Sleep
	}
	if o.APIKey != "" {
		if err := c.broker.Register(keyPlaceholder, o.APIKey, target.Host); err != nil {
			return nil, err
		}
	}
	c.resolver = o.Resolver
	if c.resolver == nil {
		c.resolver = safety.SystemResolver
	}
	transport := o.Transport
	if transport == nil {
		if local {
			transport = &http.Transport{Proxy: nil, ResponseHeaderTimeout: timeout}
		} else {
			c.dialer = safety.NewPinnedDialer(o.Dial, timeout)
			transport = safety.NewPinnedTransport(c.dialer, timeout, nil)
		}
	}
	c.client = &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c, nil
}

// Name is "systemone:<model>".
func (c *Client) Name() string { return "systemone:" + c.model }

// String never shows the key.
func (c *Client) String() string {
	return fmt.Sprintf("SystemOneClient(model=%s, endpoint=%s)", contracts.PyRepr(c.model), contracts.PyRepr(c.endpoint))
}

// Close releases idle connections.
func (c *Client) Close() error {
	c.client.CloseIdleConnections()
	return nil
}

// WorstCaseUSD is an upper bound on a request's cost (nil when the endpoint has no price).
func (c *Client) WorstCaseUSD(body []byte) *float64 {
	if c.price == nil {
		return nil
	}
	usd := math.Ceil(float64(utf8.RuneCount(body))/charsPerToken) * *c.price / 1e6
	return &usd
}

// Evaluate answers every question against state.
func (c *Client) Evaluate(ctx context.Context, state any, questions []Named) (Result, error) {
	req, err := RequestBody(c.model, state, questions)
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, errorf("system one request is not JSON: %v", err)
	}
	var result Result
	call := func(ctx context.Context) (contracts.Usage, error) {
		raw, err := model.WithRetries(ctx, c.retry, func(ctx context.Context) ([]byte, error) {
			return c.post(ctx, body)
		})
		if err != nil {
			return contracts.Usage{}, err
		}
		result, err = ParseResponse(raw, questions)
		if err != nil {
			return contracts.Usage{}, err
		}
		usage := contracts.Usage{InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
			Model: result.Model, Provider: c.Name()}
		if usage.Model == "" {
			usage.Model = c.model
		}
		if c.price != nil {
			usd := float64(result.InputTokens) * *c.price / 1e6
			usage.ReportedCostUSD = &usd
		}
		return usage, nil
	}
	if c.meter == nil {
		_, err = call(ctx)
	} else {
		err = c.meter.RunExternal(ctx, c.WorstCaseUSD(body), c.role, call)
	}
	if err != nil {
		var sysErr *Error
		var budget *governor.BudgetExceeded
		switch {
		case errors.As(err, &sysErr):
			return Result{}, sysErr
		case errors.As(err, &budget):
			return Result{}, errorf("system one call refused: %v", budget)
		}
		return Result{}, &Error{Message: c.describe(err)}
	}
	return result, nil
}

func (c *Client) describe(err error) string {
	var status *model.HTTPStatusError
	why := fmt.Sprintf("%s failed (%v)", c.endpoint, err)
	if errors.As(err, &status) {
		why = fmt.Sprintf("%s answered HTTP %d", c.endpoint, status.StatusCode)
	}
	why = "system one call failed: " + why
	if c.key != "" {
		why = strings.ReplaceAll(why, c.key, "***")
	}
	return why
}

// egressStop is an egress refusal that is never retried.
type egressStop struct{ err error }

func (e *egressStop) Error() string { return e.err.Error() }
func (e *egressStop) Unwrap() error { return safety.ErrEgressDenied }

func (c *Client) post(ctx context.Context, body []byte) ([]byte, error) {
	parsed, err := c.policy.Check(c.endpoint)
	if err != nil {
		return nil, &egressStop{err}
	}
	if c.dialer != nil {
		addresses, err := safety.CheckResolvedAddresses(ctx, parsed.Host, parsed.Port, c.resolver)
		if err != nil {
			return nil, &egressStop{err}
		}
		if err := c.dialer.Pin(parsed.Host, addresses); err != nil { // the connection dials only these
			return nil, &egressStop{err}
		}
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if c.key != "" {
		headers["Authorization"] = "Bearer " + keyPlaceholder
		headers = c.broker.ResolveHeaders(headers, parsed.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		reason := strings.TrimPrefix(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)), " ")
		return nil, &model.HTTPStatusError{Method: http.MethodPost, URL: c.endpoint, StatusCode: resp.StatusCode,
			Reason: reason, Header: resp.Header, Body: data}
	}
	return data, nil
}
