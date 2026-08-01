// Package outbound provides the polite, guarded HTTP client every service in
// this family uses for egress. It is not merely politeness: it is the single
// choke point for all outbound traffic, so it owns the whole trust boundary.
//
// What it guarantees:
//
//   - Rate limited by default (golang.org/x/time/rate). An unlimited client is
//     opt-in and explicit via Config.Unlimited.
//   - An identifying User-Agent carrying a product name and a contact URL, so
//     an operator on the other end can reach a human instead of a firewall.
//   - Retry-After is honored on 429 and 503; otherwise exponential backoff with
//     jitter, bounded by Config.MaxAttempts.
//   - Timeouts are mandatory. A timeout-less client is unconstructable; there is
//     no public path that produces one. A stalled remote must not be able to
//     park an ingest goroutine forever.
//   - SSRF guard, enabled by default: the resolved IP is checked at dial time
//     (see guard.go), not the hostname, so DNS rebinding is still refused.
//     Internal clients opt out explicitly with Config.AllowPrivateNetworks.
//   - Redirect header hygiene: the standard library strips Authorization and
//     Cookie on a cross-domain redirect but forwards custom headers such as
//     X-Api-Key. This client is where API keys get attached, so CheckRedirect
//     drops every auth-bearing header when the host changes, and caps hops.
//   - TLS MinVersion 1.2. InsecureSkipVerify is not reachable through the
//     public API: a private CA belongs in the system trust store or
//     SSL_CERT_FILE, not in a constructor option.
//
// # Terms-of-service discipline
//
// Every integration MUST record the remote's rate limits and terms constraints
// in a comment at the construction site, next to the Config literal that
// encodes them. The Config fields are the machine-readable half of that record;
// the comment is the half that says why. Following the eBay Developer Program
// License 8.1(b) precedent already set in nagus:
//
//	// eBay Browse API: 5,000 calls/day on the production keyset; License
//	// 8.1(b) forbids retaining item data beyond 24h. Budget below is the
//	// daily cap; the retention rule lives in the store's TTL.
//	c, err := outbound.New(outbound.Config{
//	        Product: "nagus", Version: build.Version,
//	        ContactURL: "https://example.org/bots",
//	        Timeout: 20 * time.Second,
//	        RequestsPerSecond: 2, Burst: 2,
//	        CallBudget: 5000,
//	})
//
// A terms-of-use ban on automated collection applies whether the collection
// happens through an RSS feed, an undocumented internal JSON endpoint, or a
// headless browser. Moving the code does not change the consent: if the terms
// say no, the answer is no in every transport.
package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Defaults applied when the corresponding Config field is zero. There is
// deliberately no default timeout: see Config.Timeout.
const (
	DefaultRequestsPerSecond = 1.0
	DefaultBurst             = 1
	DefaultMaxAttempts       = 3
	DefaultBaseBackoff       = 500 * time.Millisecond
	DefaultMaxBackoff        = 30 * time.Second
	DefaultMaxRetryWait      = 60 * time.Second
	DefaultMaxRedirects      = 5
	// maxDrain bounds how much of a to-be-retried response body is read so the
	// connection can be reused.
	maxDrain = 64 << 10
)

// Sentinel errors. All are wrapped with context, so test with errors.Is.
var (
	ErrDisallowedScheme    = errors.New("outbound: URL scheme is not allowed")
	ErrTooManyRedirects    = errors.New("outbound: too many redirects")
	ErrCallBudgetExhausted = errors.New("outbound: call budget exhausted")
)

// CallEvent describes one outbound attempt. One event is emitted per HTTP
// attempt, so a request that is retried twice produces three events -- that is
// what the remote actually saw, and therefore what a call budget must count.
type CallEvent struct {
	Host         string        // request host, suitable as a metric label
	Method       string        // HTTP method
	Attempt      int           // 1-based attempt number within this call
	StatusCode   int           // 0 when the attempt failed before a response
	Err          error         // transport error, if any
	Duration     time.Duration // time spent in the round trip
	LimiterDelay time.Duration // time spent waiting on the rate limiter

	// CallsUsed is the cumulative number of attempts this client has made.
	// BudgetRemaining is what is left of Config.CallBudget, or -1 when no
	// budget is configured.
	CallsUsed       int64
	BudgetRemaining int64
}

// Metrics is the seam the observability package wires into. It is an interface
// (not an import of the obs package) so outbound stays dependency-free and any
// counter implementation can be attached at composition time.
type Metrics interface {
	RecordCall(ev CallEvent)
}

// MetricsFunc adapts a plain function to Metrics.
type MetricsFunc func(ev CallEvent)

// RecordCall implements Metrics.
func (f MetricsFunc) RecordCall(ev CallEvent) { f(ev) }

// Config describes one outbound integration. Record the remote's rate limits
// and terms constraints in a comment at the construction site; see the package
// comment.
type Config struct {
	// Product identifies the caller in the User-Agent. Required.
	Product string
	// Version is an optional build version added to the User-Agent.
	Version string
	// ContactURL is an absolute http(s) URL an operator can use to reach a
	// human about this traffic. Required.
	ContactURL string

	// Timeout bounds a single attempt, end to end. Required and must be
	// positive: there is no default, because a timeout-less client must be
	// unconstructable.
	Timeout time.Duration

	// RequestsPerSecond and Burst configure the token bucket. Zero means
	// DefaultRequestsPerSecond / DefaultBurst -- rate limiting is the default.
	RequestsPerSecond float64
	Burst             int
	// Unlimited disables rate limiting entirely. It is mutually exclusive with
	// RequestsPerSecond: an unlimited client has to be asked for by name.
	Unlimited bool

	// MaxAttempts caps total attempts per call, including the first.
	// BaseBackoff/MaxBackoff bound the exponential-with-jitter delay used when
	// the response carries no usable Retry-After. MaxRetryWait caps how long a
	// Retry-After may park the request; beyond it the response is handed back
	// to the caller unretried.
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	MaxRetryWait time.Duration

	// MaxRedirects caps redirect hops. Zero means DefaultMaxRedirects.
	MaxRedirects int
	// SensitiveHeaders names additional headers to drop on a cross-host
	// redirect, beyond the built-in auth-bearing set.
	SensitiveHeaders []string

	// AllowedSchemes restricts request URL schemes. Zero means http and https.
	AllowedSchemes []string

	// AllowPrivateNetworks disables the SSRF guard. Set it ONLY for clients
	// that talk to known internal services; a client that fetches untrusted
	// URLs must leave it false. From inside a cluster an SSRF reaches the
	// secret store and the control plane with nothing in the way.
	AllowPrivateNetworks bool

	// CallBudget, when positive, is the maximum number of attempts this client
	// will ever make; further calls fail with ErrCallBudgetExhausted. Use it to
	// encode a documented daily/keyset quota in code.
	CallBudget int64

	// Metrics receives one CallEvent per attempt. Optional.
	Metrics Metrics

	// Test seams. Unexported on purpose: no caller outside this package can
	// replace the guarded transport, the resolver, or the jitter source.
	transport http.RoundTripper
	lookup    lookupFunc
	jitter    func(time.Duration) time.Duration
}

// Client is a rate-limited, retrying, SSRF-guarded HTTP client. It is safe for
// concurrent use. Construct it with New; the zero value is not usable.
type Client struct {
	hc           *http.Client
	limiter      *rate.Limiter
	userAgent    string
	maxAttempts  int
	baseBackoff  time.Duration
	maxBackoff   time.Duration
	maxRetryWait time.Duration
	schemes      map[string]bool
	metrics      Metrics
	budget       int64
	used         atomic.Int64
	jitter       func(time.Duration) time.Duration
}

// New validates cfg and builds a client. It returns an aggregated error
// describing every problem rather than the first one found.
func New(cfg Config) (*Client, error) {
	var errs []error

	if strings.TrimSpace(cfg.Product) == "" {
		errs = append(errs, errors.New("outbound: Config.Product is required for an identifying User-Agent"))
	}
	if u, err := url.Parse(cfg.ContactURL); cfg.ContactURL == "" || err != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, errors.New("outbound: Config.ContactURL must be an absolute http(s) URL so an operator can reach a human"))
	}
	if cfg.Timeout <= 0 {
		errs = append(errs, errors.New("outbound: Config.Timeout must be positive; a timeout-less client is not constructable"))
	}
	if cfg.Unlimited && cfg.RequestsPerSecond != 0 {
		errs = append(errs, errors.New("outbound: Config.Unlimited and Config.RequestsPerSecond are mutually exclusive"))
	}
	if cfg.RequestsPerSecond < 0 || cfg.Burst < 0 || cfg.MaxAttempts < 0 || cfg.CallBudget < 0 ||
		cfg.BaseBackoff < 0 || cfg.MaxBackoff < 0 || cfg.MaxRetryWait < 0 || cfg.MaxRedirects < 0 {
		errs = append(errs, errors.New("outbound: numeric Config fields must not be negative"))
	}

	schemes := map[string]bool{"http": true, "https": true}
	if len(cfg.AllowedSchemes) > 0 {
		schemes = make(map[string]bool, len(cfg.AllowedSchemes))
		for _, s := range cfg.AllowedSchemes {
			s = strings.ToLower(strings.TrimSpace(s))
			if s != "http" && s != "https" {
				errs = append(errs, fmt.Errorf("outbound: Config.AllowedSchemes: %q is not an HTTP scheme", s))
				continue
			}
			schemes[s] = true
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	limiter := rate.NewLimiter(rate.Inf, 0)
	if !cfg.Unlimited {
		rps := cfg.RequestsPerSecond
		if rps == 0 {
			rps = DefaultRequestsPerSecond
		}
		burst := cfg.Burst
		if burst == 0 {
			burst = DefaultBurst
		}
		limiter = rate.NewLimiter(rate.Limit(rps), burst)
	}

	c := &Client{
		limiter:      limiter,
		userAgent:    userAgent(cfg),
		maxAttempts:  orInt(cfg.MaxAttempts, DefaultMaxAttempts),
		baseBackoff:  orDuration(cfg.BaseBackoff, DefaultBaseBackoff),
		maxBackoff:   orDuration(cfg.MaxBackoff, DefaultMaxBackoff),
		maxRetryWait: orDuration(cfg.MaxRetryWait, DefaultMaxRetryWait),
		schemes:      schemes,
		metrics:      cfg.Metrics,
		budget:       cfg.CallBudget,
		jitter:       cfg.jitter,
	}
	if c.jitter == nil {
		c.jitter = defaultJitter
	}

	extra := make(map[string]bool, len(cfg.SensitiveHeaders))
	for _, h := range cfg.SensitiveHeaders {
		extra[strings.ToLower(strings.TrimSpace(h))] = true
	}
	maxRedirects := orInt(cfg.MaxRedirects, DefaultMaxRedirects)

	c.hc = &http.Client{
		Timeout:       cfg.Timeout,
		Transport:     transportFor(cfg),
		CheckRedirect: checkRedirect(maxRedirects, extra),
	}
	return c, nil
}

// transportFor builds the guarded transport. The returned transport always has
// a TLS floor of 1.2 and, unless the caller opted out, a dialer that refuses
// private destinations on the resolved IP.
func transportFor(cfg Config) http.RoundTripper {
	if cfg.transport != nil {
		return cfg.transport // test seam only; unexported field
	}
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext:           base.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if cfg.AllowPrivateNetworks {
		// Internal client: an egress proxy is fine, the destination is trusted.
		tr.Proxy = http.ProxyFromEnvironment
		return tr
	}
	// Guarded client: no proxy. A proxy resolves and connects on our behalf,
	// which would move the destination decision outside the dial-time IP check.
	tr.DialContext = guardedDial(base.DialContext, cfg.lookup)
	return tr
}

// checkRedirect caps hops and drops auth-bearing headers when the host changes.
func checkRedirect(maxHops int, extra map[string]bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > maxHops {
			return fmt.Errorf("%w: stopped after %d", ErrTooManyRedirects, maxHops)
		}
		prev := via[len(via)-1]
		if sameOrigin(prev.URL, req.URL) {
			return nil
		}
		for name := range req.Header {
			if isSensitiveHeader(name, extra) {
				req.Header.Del(name)
			}
		}
		return nil
	}
}

// sameOrigin compares scheme, host, and effective port. A port change or an
// https-to-http downgrade counts as a host change: the credential was issued
// for one origin, not for whatever the redirect points at.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(canonicalHost(a), canonicalHost(b))
}

func canonicalHost(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// sensitiveExact and sensitiveSubstrings define "auth-bearing". The substring
// pass is deliberately broad: dropping a benign header on a cross-host redirect
// costs a retry, leaking a credential costs the credential.
var (
	sensitiveExact = map[string]bool{
		"authorization":       true,
		"proxy-authorization": true,
		"www-authenticate":    true,
		"cookie":              true,
		"cookie2":             true,
	}
	sensitiveSubstrings = []string{
		"auth", "key", "token", "secret", "credential",
		"password", "passwd", "signature", "session", "csrf", "sig",
	}
)

func isSensitiveHeader(name string, extra map[string]bool) bool {
	lower := strings.ToLower(name)
	if sensitiveExact[lower] || extra[lower] {
		return true
	}
	for _, s := range sensitiveSubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func userAgent(cfg Config) string {
	product := strings.TrimSpace(cfg.Product)
	if v := strings.TrimSpace(cfg.Version); v != "" {
		product += "/" + v
	}
	return fmt.Sprintf("%s (+%s)", product, strings.TrimSpace(cfg.ContactURL))
}

func orInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

// CallsUsed reports how many attempts this client has made. It is the
// consumption half of the call budget and is also carried on every CallEvent.
func (c *Client) CallsUsed() int64 { return c.used.Load() }

// Get issues a GET. The context is required: there is no context-less helper.
func (c *Client) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("outbound: build request: %w", err)
	}
	return c.Do(req)
}

// Do sends req, waiting on the rate limiter, retrying 429 and 503 within the
// configured attempt cap, and honoring Retry-After. The caller's request is not
// mutated. A response with a non-2xx status is not an error; the final response
// is returned so the caller can inspect it.
//
// Config.Timeout bounds each attempt. Bound the whole call -- limiter waits,
// retries and all -- with the request context.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("outbound: nil request")
	}
	if !c.schemes[strings.ToLower(req.URL.Scheme)] {
		return nil, fmt.Errorf("%w: %q", ErrDisallowedScheme, req.URL.Scheme)
	}

	ctx := req.Context()
	outreq := req.Clone(ctx)
	// Identity is client policy, not per-request decoration.
	outreq.Header.Set("User-Agent", c.userAgent)
	replayable := outreq.Body == nil || outreq.GetBody != nil

	for attempt := 1; ; attempt++ {
		used, remaining, ok := c.consume()
		if !ok {
			return nil, fmt.Errorf("%w after %d calls", ErrCallBudgetExhausted, c.budget)
		}

		waitStart := time.Now()
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("outbound: rate limiter: %w", err)
		}
		limiterDelay := time.Since(waitStart)

		if attempt > 1 && outreq.GetBody != nil {
			body, err := outreq.GetBody()
			if err != nil {
				return nil, fmt.Errorf("outbound: rewind request body: %w", err)
			}
			outreq.Body = body
		}

		start := time.Now()
		resp, err := c.hc.Do(outreq) //nolint:bodyclose // returned to the caller or drained below
		ev := CallEvent{
			Host:            outreq.URL.Host,
			Method:          outreq.Method,
			Attempt:         attempt,
			Err:             err,
			Duration:        time.Since(start),
			LimiterDelay:    limiterDelay,
			CallsUsed:       used,
			BudgetRemaining: remaining,
		}
		if resp != nil {
			ev.StatusCode = resp.StatusCode
		}
		c.record(ev)

		if err != nil {
			return nil, err
		}
		if attempt >= c.maxAttempts || !retryableStatus(resp.StatusCode) || !replayable {
			return resp, nil
		}
		delay, retry := c.retryDelay(resp, attempt)
		if !retry {
			return resp, nil
		}
		drain(resp)
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (c *Client) record(ev CallEvent) {
	if c.metrics != nil {
		c.metrics.RecordCall(ev)
	}
}

// consume takes one unit of call budget. It reports the cumulative count, what
// remains (-1 when unbudgeted), and whether the call may proceed.
func (c *Client) consume() (used, remaining int64, ok bool) {
	used = c.used.Add(1)
	if c.budget <= 0 {
		return used, -1, true
	}
	if used > c.budget {
		c.used.Add(-1)
		return c.used.Load(), 0, false
	}
	return used, c.budget - used, true
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// retryDelay prefers the server's Retry-After and falls back to backoff. It
// reports false when the wait the server asked for exceeds MaxRetryWait: the
// caller gets the response back rather than having the goroutine parked.
func (c *Client) retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
		if d > c.maxRetryWait {
			return 0, false
		}
		return max(d, 0), true
	}
	return c.backoff(attempt), true
}

// backoff is base * 2^(attempt-1), capped, then jittered.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseBackoff
	for range attempt - 1 {
		d *= 2
		if d >= c.maxBackoff {
			return c.jitter(c.maxBackoff)
		}
	}
	return c.jitter(d)
}

// defaultJitter returns a duration in [d/2, d] -- enough spread to break up
// synchronized retries without letting the delay collapse toward zero.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	//nolint:gosec // retry jitter, not a security decision
	return half + time.Duration(rand.Float64()*float64(d-half))
}

// parseRetryAfter accepts both forms of the header: delta-seconds and an
// HTTP-date.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		return when.Sub(now), true
	}
	return 0, false
}

// drain reads a bounded amount of a discarded response so the connection can be
// reused, then closes it.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	_ = resp.Body.Close()
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
