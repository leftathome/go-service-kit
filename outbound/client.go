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
// # Three shapes, one policy
//
// Every guarantee above holds identically whichever of these a service takes.
// They are the same engine behind three surfaces, so no adoption path is
// weaker than another. Pick by what the calling code already looks like:
//
//	c, err := outbound.New(cfg)            // *Client: Get/Do, the native shape
//	tr, err := outbound.NewTransport(cfg)  // http.RoundTripper: hc.Transport = tr
//	hc, err := outbound.NewHTTPClient(cfg) // *http.Client, for an SDK that demands one
//
// [Client.Transport] and [Client.HTTPClient] hand out the other two shapes
// backed by the SAME limiter and the SAME call budget, so a service can convert
// call sites one at a time without accidentally running two quotas against one
// remote.
//
// Better still, type the dependency as [Doer] and the choice stops mattering at
// the call site. nagus is the reason both of these exist: six integrations
// declared HTTPClient *http.Client, so adopting the kit's egress policy was six
// files of mechanical edits for no behavioural reason.
//
// # Terms-of-service discipline
//
// Every integration MUST record the remote's rate limits and terms constraints
// in a comment at the construction site, next to the Config literal that
// encodes them. The Config fields are the machine-readable half of that record;
// the comment is the half that says why. Following the eBay Developer Program
// License 8.1(b) precedent already set in nagus:
//
//	// eBay Browse API: 5,000 calls/day on the production keyset, per UTC
//	// day; License 8.1(b) forbids retaining item data beyond 24h. The
//	// retention rule lives in the store's TTL. MaxAttempts is 1 because a
//	// metered API is the wrong place for a silent retry: the quota is
//	// charged per attempt, not per logical call.
//	daily, err := outbound.NewWindowedBudget(outbound.BudgetConfig{
//	        Limit: 5000, Window: 24 * time.Hour, Calendar: true,
//	})
//	c, err := outbound.New(outbound.Config{
//	        Product: "nagus", Version: build.Version,
//	        ContactURL: "https://example.org/bots",
//	        Timeout: 20 * time.Second,
//	        RequestsPerSecond: 2, Burst: 2,
//	        MaxAttempts: 1,
//	        Budget: daily,
//	})
//
// Feed the server's own accounting back in whenever it offers one -- it is
// authoritative and the local ledger is a guess:
//
//	if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Remaining"), 10, 64); err == nil {
//	        daily.Observe(n)
//	}
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

	// CallsUsed is the cumulative number of attempts this client has made
	// since it was constructed -- a process-lifetime count, which after a
	// windowed budget rolls is deliberately NOT the same as the budget's own
	// Used. BudgetRemaining is what the budget had left after this attempt
	// was charged, or -1 when the client is unmetered.
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
	// will ever make, for the LIFETIME OF THE PROCESS; further calls fail with
	// ErrCallBudgetExhausted.
	//
	// Real quotas are windowed and are often reported by the server, which a
	// process-lifetime counter cannot express: a pod that lives a week does
	// not get one day's worth of a daily quota. Prefer Budget, and reach for
	// CallBudget only for a genuine "this process must never make more than N
	// calls" ceiling. It is exactly BudgetConfig{Limit: N} with no window.
	// The two are mutually exclusive.
	CallBudget int64

	// Budget meters attempts against a real quota: windowed, resettable, and
	// able to take a server-reported remaining count. See [WindowedBudget].
	Budget Budget

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
//
// A service that already has *http.Client-shaped call sites does not have to
// change them: see [Doer], [Client.Transport] and [Client.HTTPClient].
type Client struct {
	eng *engine
	hc  *http.Client

	// tr and adopted expose the same policy and the same engine -- therefore
	// the same limiter and the same budget -- to callers that need a
	// RoundTripper or a concrete *http.Client.
	tr      *Transport
	adopted *http.Client
}

// engine holds the per-call policy shared by Client and Transport: identity,
// pacing, retries, budget and the metrics seam. Keeping it in one place is why
// adopting the RoundTripper cannot quietly get a weaker client than Client.Do.
type engine struct {
	limiter      *rate.Limiter
	userAgent    string
	maxAttempts  int
	baseBackoff  time.Duration
	maxBackoff   time.Duration
	maxRetryWait time.Duration
	schemes      map[string]bool
	metrics      Metrics
	budget       Budget
	used         atomic.Int64
	jitter       func(time.Duration) time.Duration
}

// sendFunc performs one attempt. Client.Do supplies http.Client.Do (so the
// stdlib handles redirects above the retry loop); Transport supplies a single
// guarded round trip.
type sendFunc func(*http.Request) (*http.Response, error)

// built is the validated, constructed policy, from which any of the three
// public shapes can be handed out.
type built struct {
	eng          *engine
	base         http.RoundTripper
	timeout      time.Duration
	maxRedirects int
	sensitive    map[string]bool
}

func (b *built) transport() *Transport {
	return &Transport{
		eng:          b.eng,
		base:         b.base,
		timeout:      b.timeout,
		maxRedirects: b.maxRedirects,
		sensitive:    b.sensitive,
	}
}

func (b *built) httpClient() *http.Client {
	// CheckRedirect as well as the Transport's own strip: the transport covers
	// a caller who attaches it to their own bare client, this covers the hop
	// before the transport ever sees it. Neither is redundant enough to drop.
	return &http.Client{
		Transport:     b.transport(),
		CheckRedirect: checkRedirect(b.maxRedirects, b.sensitive),
	}
}

// New validates cfg and builds a client. It returns an aggregated error
// describing every problem rather than the first one found.
func New(cfg Config) (*Client, error) {
	b, err := build(cfg)
	if err != nil {
		return nil, err
	}
	c := &Client{
		eng: b.eng,
		hc: &http.Client{
			Timeout:       b.timeout,
			Transport:     b.base,
			CheckRedirect: checkRedirect(b.maxRedirects, b.sensitive),
		},
		tr: b.transport(),
	}
	// Share the engine, so a service migrating call sites one at a time runs
	// one limiter and one budget, not two.
	c.adopted = &http.Client{
		Transport:     c.tr,
		CheckRedirect: checkRedirect(b.maxRedirects, b.sensitive),
	}
	return c, nil
}

// build validates cfg once for New, NewTransport and NewHTTPClient, so no
// public constructor can produce a client with weaker guarantees than another.
func build(cfg Config) (*built, error) {
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

	budget, err := budgetFor(cfg)
	if err != nil {
		errs = append(errs, err)
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

	eng := &engine{
		limiter:      limiter,
		userAgent:    userAgent(cfg),
		maxAttempts:  orInt(cfg.MaxAttempts, DefaultMaxAttempts),
		baseBackoff:  orDuration(cfg.BaseBackoff, DefaultBaseBackoff),
		maxBackoff:   orDuration(cfg.MaxBackoff, DefaultMaxBackoff),
		maxRetryWait: orDuration(cfg.MaxRetryWait, DefaultMaxRetryWait),
		schemes:      schemes,
		metrics:      cfg.Metrics,
		budget:       budget,
		jitter:       cfg.jitter,
	}
	if eng.jitter == nil {
		eng.jitter = defaultJitter
	}

	extra := make(map[string]bool, len(cfg.SensitiveHeaders))
	for _, h := range cfg.SensitiveHeaders {
		extra[strings.ToLower(strings.TrimSpace(h))] = true
	}

	return &built{
		eng:          eng,
		base:         transportFor(cfg),
		timeout:      cfg.Timeout,
		maxRedirects: orInt(cfg.MaxRedirects, DefaultMaxRedirects),
		sensitive:    extra,
	}, nil
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

// CallsUsed reports how many attempts this client has made since it was
// constructed. It is also carried on every CallEvent.
//
// It is a PROCESS-LIFETIME count and is not the same number as a windowed
// budget's Used: a client on a per-UTC-day budget will report a CallsUsed far
// above Stats().Used after the first roll. Ask the budget about the quota; ask
// the client about the client.
func (c *Client) CallsUsed() int64 { return c.eng.used.Load() }

// Budget returns the configured call budget, or nil when this client is
// unmetered. Use it to feed a server-reported remaining count back in, or to
// publish the quota as a metric:
//
//	if b := c.Budget(); b != nil {
//	        b.Observe(remainingFromHeader)
//	}
func (c *Client) Budget() Budget { return c.eng.budget }

// BudgetRemaining reports what is left of the call budget, or -1 when this
// client is unmetered. It is the accessor the CallEvent side channel was
// previously the only source of.
func (c *Client) BudgetRemaining() int64 {
	if c.eng.budget == nil {
		return -1
	}
	return c.eng.budget.Stats().Remaining
}

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
	return c.eng.do(req, c.hc.Do)
}

// do is the retry loop, shared by Client.Do and Transport.RoundTrip. send
// performs one attempt: for a Client that is http.Client.Do (redirects handled
// above the retry loop, as they always were); for a Transport it is a single
// guarded round trip.
func (e *engine) do(req *http.Request, send sendFunc) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("outbound: nil request")
	}
	if !e.schemes[strings.ToLower(req.URL.Scheme)] {
		return nil, fmt.Errorf("%w: %q", ErrDisallowedScheme, req.URL.Scheme)
	}

	ctx := req.Context()
	outreq := req.Clone(ctx)
	// Identity is client policy, not per-request decoration.
	outreq.Header.Set("User-Agent", e.userAgent)
	replayable := outreq.Body == nil || outreq.GetBody != nil

	for attempt := 1; ; attempt++ {
		used, remaining, ok := e.consume()
		if !ok {
			return nil, e.budgetExhausted()
		}

		waitStart := time.Now()
		if err := e.limiter.Wait(ctx); err != nil {
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
		resp, err := send(outreq) //nolint:bodyclose // returned to the caller or drained below
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
		e.record(ev)

		if err != nil {
			return nil, err
		}
		if attempt >= e.maxAttempts || !retryableStatus(resp.StatusCode) || !replayable {
			return resp, nil
		}
		delay, retry := e.retryDelay(resp, attempt)
		if !retry {
			return resp, nil
		}
		drain(resp)
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (e *engine) record(ev CallEvent) {
	if e.metrics != nil {
		e.metrics.RecordCall(ev)
	}
}

// consume takes one unit of call budget. It reports this engine's cumulative
// attempt count, what the budget has left (-1 when unmetered), and whether the
// call may proceed. Nothing is charged when it may not.
func (e *engine) consume() (used, remaining int64, ok bool) {
	if e.budget == nil {
		return e.used.Add(1), -1, true
	}
	if _, remaining, ok = e.budget.Reserve(); !ok {
		return e.used.Load(), 0, false
	}
	return e.used.Add(1), remaining, true
}

// budgetExhausted describes the quota that refused the call. The window bounds
// are included when there are any, because "wait until 00:00 UTC" is the
// actionable half of the message.
func (e *engine) budgetExhausted() error {
	st := e.budget.Stats()
	if st.WindowEnd.IsZero() {
		return fmt.Errorf("%w: %d of %d used", ErrCallBudgetExhausted, st.Used, st.Limit)
	}
	return fmt.Errorf("%w: %d of %d used in the window ending %s",
		ErrCallBudgetExhausted, st.Used, st.Limit, st.WindowEnd.UTC().Format(time.RFC3339))
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable
}

// retryDelay prefers the server's Retry-After and falls back to backoff. It
// reports false when the wait the server asked for exceeds MaxRetryWait: the
// caller gets the response back rather than having the goroutine parked.
func (e *engine) retryDelay(resp *http.Response, attempt int) (time.Duration, bool) {
	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
		if d > e.maxRetryWait {
			return 0, false
		}
		return max(d, 0), true
	}
	return e.backoff(attempt), true
}

// backoff is base * 2^(attempt-1), capped, then jittered.
func (e *engine) backoff(attempt int) time.Duration {
	d := e.baseBackoff
	for range attempt - 1 {
		d *= 2
		if d >= e.maxBackoff {
			return e.jitter(e.maxBackoff)
		}
	}
	return e.jitter(d)
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
