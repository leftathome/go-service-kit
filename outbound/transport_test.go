package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// baseCfg fills in the mandatory identity/timeout fields, like mustClient does
// for Client, so each test states only what it exercises.
func baseCfg(cfg Config) Config {
	if cfg.Product == "" {
		cfg.Product = "kittest"
	}
	if cfg.ContactURL == "" {
		cfg.ContactURL = "https://example.invalid/bots"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.RequestsPerSecond == 0 && !cfg.Unlimited {
		cfg.Unlimited = true
	}
	return cfg
}

func mustTransport(t *testing.T, cfg Config) *Transport {
	t.Helper()
	tr, err := NewTransport(baseCfg(cfg))
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return tr
}

func mustHTTPClient(t *testing.T, cfg Config) *http.Client {
	t.Helper()
	hc, err := NewHTTPClient(baseCfg(cfg))
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return hc
}

func req(ctx context.Context, t *testing.T, method, url string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// --- Doer -------------------------------------------------------------------

// The whole point of Doer: an integration types one field and accepts either
// implementation, so adopting the kit is not an edit per call site.
func TestDoerAcceptsEitherImplementation(t *testing.T) {
	t.Parallel()
	fetch := func(d Doer, url string) (int, error) {
		resp, err := d.Do(req(t.Context(), t, http.MethodGet, url))
		if err != nil {
			return 0, err
		}
		defer closeBody(t, resp)
		return resp.StatusCode, nil
	}

	for name, d := range map[string]Doer{
		"outbound.Client": mustClient(t, Config{Unlimited: true, transport: okTransport()}),
		"http.Client":     mustHTTPClient(t, Config{Unlimited: true, transport: okTransport()}),
	} {
		code, err := fetch(d, "https://example.com/things")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, code)
		}
	}
}

// --- validation parity --------------------------------------------------------

func TestTransportConstructorsValidateLikeNew(t *testing.T) {
	t.Parallel()
	bad := []Config{
		{Product: "kittest", ContactURL: "https://example.invalid/bots"},     // no timeout
		{ContactURL: "https://example.invalid/bots", Timeout: time.Second},   // no product
		{Product: "kittest", Timeout: time.Second},                           // no contact URL
		{Product: "kittest", ContactURL: "not-a-url", Timeout: time.Second},  // bad contact URL
		{Product: "kittest", ContactURL: "https://x.invalid/b", Timeout: -1}, // negative timeout
		{Product: "kittest", ContactURL: "https://x.invalid/b", Timeout: 1, Unlimited: true, RequestsPerSecond: 2},
	}
	for i, cfg := range bad {
		if _, err := NewTransport(cfg); err == nil {
			t.Errorf("case %d: NewTransport accepted an invalid config", i)
		}
		if _, err := NewHTTPClient(cfg); err == nil {
			t.Errorf("case %d: NewHTTPClient accepted an invalid config", i)
		}
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: New accepted an invalid config", i)
		}
	}
}

// --- the guard must survive every path out of this package --------------------

// A RoundTripper that quietly lost the SSRF guard would be worse than no
// RoundTripper at all: it looks adopted and is not.
func TestTransportKeepsTheSSRFGuard(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A bare client: no Timeout, stdlib CheckRedirect, nothing but our
	// transport. This is the adoption path the kit is advertising.
	hc := &http.Client{Transport: mustTransport(t, Config{})}
	resp, err := hc.Do(req(t.Context(), t, http.MethodGet, srv.URL))
	if err == nil {
		closeBody(t, resp)
		t.Fatal("a loopback destination was permitted through the RoundTripper")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}

	// And the opt-out still works, on the same path.
	ok := &http.Client{Transport: mustTransport(t, Config{AllowPrivateNetworks: true})}
	resp, err = ok.Do(req(t.Context(), t, http.MethodGet, srv.URL))
	if err != nil {
		t.Fatalf("AllowPrivateNetworks client: %v", err)
	}
	closeBody(t, resp)
}

func TestTransportKeepsTheTLSFloorAndAvoidsProxies(t *testing.T) {
	t.Parallel()
	tr := mustTransport(t, Config{})
	base, ok := tr.base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport is %T, want *http.Transport", tr.base)
	}
	if base.TLSClientConfig == nil || base.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS MinVersion = %v, want TLS 1.2", base.TLSClientConfig)
	}
	if base.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify is set")
	}
	// A proxy resolves and connects on our behalf, which moves the destination
	// decision outside the dial-time IP check.
	if base.Proxy != nil {
		t.Error("guarded transport has a proxy configured")
	}
	if internal, iok := mustTransport(t, Config{AllowPrivateNetworks: true}).base.(*http.Transport); !iok || internal.Proxy == nil {
		t.Error("internal transport should keep ProxyFromEnvironment")
	}
}

func TestTransportRejectsDisallowedSchemes(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Transport: mustTransport(t, Config{transport: okTransport()})}
	for _, raw := range []string{"ftp://example.com/x", "gopher://example.com"} {
		resp, err := hc.Do(req(t.Context(), t, http.MethodGet, raw))
		if err == nil {
			closeBody(t, resp)
			t.Errorf("%s was permitted", raw)
			continue
		}
		if !errors.Is(err, ErrDisallowedScheme) {
			t.Errorf("%s: err = %v, want ErrDisallowedScheme", raw, err)
		}
	}
}

// --- mandatory timeout, without an http.Client.Timeout to lean on -------------

type blockingTransport struct{}

func (blockingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestTransportEnforcesTimeoutOnATimeoutlessClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hc := &http.Client{Transport: mustTransport(t, Config{
			Timeout:     2 * time.Second,
			MaxAttempts: 1,
			transport:   blockingTransport{},
		})}
		if hc.Timeout != 0 {
			t.Fatalf("test premise broken: client Timeout = %v", hc.Timeout)
		}

		start := time.Now()
		resp, err := hc.Do(req(t.Context(), t, http.MethodGet, "https://example.com/"))
		if err == nil {
			closeBody(t, resp)
			t.Fatal("a stalled remote was not cut off")
		}
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("gave up after %v, want the 2s per-attempt timeout", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline", err)
		}
	})
}

// The attempt deadline must still be live while the caller reads the body --
// that is the coverage http.Client.Timeout gives, and it is what a naive
// context.WithTimeout around RoundTrip gets wrong in the other direction, by
// cancelling the instant RoundTrip returns and truncating every response.
//
// A real http.Transport aborts an in-flight body read when the request context
// fires, so the guarantee this package owns is exactly: the context is not
// cancelled when RoundTrip returns, it carries the configured deadline, and it
// IS released when the body is closed.
func TestTransportDeadlineOutlivesRoundTripAndIsReleasedOnClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attemptCtx context.Context
		stub := &stubTransport{handler: func(r *http.Request, _ int) (*http.Response, error) {
			attemptCtx = r.Context()
			resp := stubResponse(r, http.StatusOK, nil)
			resp.Body = &ctxBody{ctx: r.Context()}
			return resp, nil
		}}
		hc := &http.Client{Transport: mustTransport(t, Config{
			Timeout: 3 * time.Second, MaxAttempts: 1, transport: stub,
		})}

		resp, err := hc.Do(req(t.Context(), t, http.MethodGet, "https://example.com/"))
		if err != nil {
			t.Fatalf("Do: %v", err)
		}

		if attemptCtx.Err() != nil {
			t.Fatal("the attempt context was cancelled the moment RoundTrip returned; the body would be truncated")
		}
		deadline, ok := attemptCtx.Deadline()
		if !ok {
			t.Fatal("the attempt carried no deadline")
		}
		if got := time.Until(deadline); got != 3*time.Second {
			t.Errorf("deadline in %v, want the configured 3s", got)
		}

		buf := make([]byte, 1)
		if _, rerr := resp.Body.Read(buf); rerr != nil {
			t.Fatalf("body read failed while the deadline was live: %v", rerr)
		}
		// Let the deadline pass: a stalled body read is cut off, not parked.
		time.Sleep(4 * time.Second)
		if _, rerr := resp.Body.Read(buf); !errors.Is(rerr, context.DeadlineExceeded) {
			t.Fatalf("body read after the deadline returned %v, want DeadlineExceeded", rerr)
		}
		closeBody(t, resp)
	})
}

func TestTransportReleasesTheAttemptContextOnBodyClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attemptCtx context.Context
		stub := &stubTransport{handler: func(r *http.Request, _ int) (*http.Response, error) {
			attemptCtx = r.Context()
			return stubResponse(r, http.StatusOK, nil), nil
		}}
		hc := &http.Client{Transport: mustTransport(t, Config{
			Timeout: time.Hour, MaxAttempts: 1, transport: stub,
		})}
		resp, err := hc.Do(req(t.Context(), t, http.MethodGet, "https://example.com/"))
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		closeBody(t, resp)
		if attemptCtx.Err() == nil {
			t.Fatal("closing the body left the attempt context and its timer alive for an hour")
		}
	})
}

// ctxBody serves bytes until its context is done, the way a real transport's
// body behaves when the request context fires.
type ctxBody struct{ ctx context.Context }

func (b *ctxBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	p[0] = 'x'
	return 1, nil
}

func (b *ctxBody) Close() error { return nil }

// --- redirect hygiene, with the stdlib's CheckRedirect, not ours --------------

func redirectingStub(from, to string) *stubTransport {
	return &stubTransport{handler: func(r *http.Request, _ int) (*http.Response, error) {
		if r.URL.Host == from {
			return stubResponse(r, http.StatusFound, map[string]string{"Location": to}), nil
		}
		return stubResponse(r, http.StatusOK, nil), nil
	}}
}

func TestTransportStripsAuthHeadersOnCrossHostRedirectThroughABareClient(t *testing.T) {
	t.Parallel()
	stub := redirectingStub("origin.example", "https://target.example/landed")
	// Deliberately a BARE client: the stdlib's own CheckRedirect forwards
	// custom headers such as X-Api-Key across hosts, and caps hops at 10.
	hc := &http.Client{Transport: mustTransport(t, Config{transport: stub})}

	r := req(t.Context(), t, http.MethodGet, "https://origin.example/start")
	r.Header.Set("X-Api-Key", "super-secret")
	r.Header.Set("X-Vendor-Token", "t0ken")
	r.Header.Set("Authorization", "Bearer abc")
	r.Header.Set("Accept", "application/json")

	resp, err := hc.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer closeBody(t, resp)

	if stub.count() != 2 {
		t.Fatalf("attempts = %d, want 2", stub.count())
	}
	landed := stub.last().Header
	for _, h := range []string{"X-Api-Key", "X-Vendor-Token", "Authorization"} {
		if v := landed.Get(h); v != "" {
			t.Errorf("%s leaked to another host with value %q", h, v)
		}
	}
	if landed.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q, want it preserved", landed.Get("Accept"))
	}
	if !strings.Contains(landed.Get("User-Agent"), "kittest") {
		t.Errorf("User-Agent = %q, want the client identity", landed.Get("User-Agent"))
	}
}

func TestTransportKeepsAuthHeadersOnSameHostRedirect(t *testing.T) {
	t.Parallel()
	stub := &stubTransport{handler: func(r *http.Request, n int) (*http.Response, error) {
		if n == 1 {
			return stubResponse(r, http.StatusFound, map[string]string{"Location": "https://origin.example/landed"}), nil
		}
		return stubResponse(r, http.StatusOK, nil), nil
	}}
	hc := &http.Client{Transport: mustTransport(t, Config{transport: stub})}

	r := req(t.Context(), t, http.MethodGet, "https://origin.example/start")
	r.Header.Set("X-Api-Key", "super-secret")
	resp, err := hc.Do(r)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer closeBody(t, resp)

	if got := stub.last().Header.Get("X-Api-Key"); got != "super-secret" {
		t.Errorf("X-Api-Key = %q on a same-host redirect, want it kept", got)
	}
}

func TestTransportCapsRedirectsBelowTheStdlibDefault(t *testing.T) {
	t.Parallel()
	stub := &stubTransport{handler: func(r *http.Request, n int) (*http.Response, error) {
		return stubResponse(r, http.StatusFound, map[string]string{
			"Location": "https://origin.example/hop" + strings.Repeat("x", n),
		}), nil
	}}
	// The stdlib would follow 10. Ours stops at 2.
	hc := &http.Client{Transport: mustTransport(t, Config{MaxRedirects: 2, transport: stub})}

	resp, err := hc.Do(req(t.Context(), t, http.MethodGet, "https://origin.example/start"))
	if err == nil {
		closeBody(t, resp)
		t.Fatal("redirect loop returned no error")
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v, want ErrTooManyRedirects", err)
	}
	if got := stub.count(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + 2 followed redirects)", got)
	}
}

// --- politeness carries over --------------------------------------------------

func TestTransportPacesAndRetriesLikeTheClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stub := &stubTransport{handler: func(r *http.Request, n int) (*http.Response, error) {
			if n == 1 {
				return stubResponse(r, http.StatusTooManyRequests, map[string]string{"Retry-After": "2"}), nil
			}
			return stubResponse(r, http.StatusOK, nil), nil
		}}
		hc := mustHTTPClient(t, Config{RequestsPerSecond: 100, Burst: 1, transport: stub})

		start := time.Now()
		resp, err := hc.Do(req(t.Context(), t, http.MethodGet, "https://example.com/"))
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		closeBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 after the retry", resp.StatusCode)
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second {
			t.Fatalf("retried after %v, want the server's 2s Retry-After honoured", elapsed)
		}
		if stub.count() != 2 {
			t.Fatalf("attempts = %d, want 2", stub.count())
		}
	})
}

func TestTransportFeedsTheMetricsSeam(t *testing.T) {
	t.Parallel()
	var events []CallEvent
	hc := mustHTTPClient(t, Config{
		Unlimited: true,
		transport: okTransport(),
		Metrics:   MetricsFunc(func(ev CallEvent) { events = append(events, ev) }),
	})
	resp, err := hc.Do(req(t.Context(), t, http.MethodGet, "https://api.example.com/things"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	closeBody(t, resp)

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Host != "api.example.com" || events[0].StatusCode != http.StatusOK {
		t.Errorf("event = %+v", events[0])
	}
}

// --- one policy, one quota ----------------------------------------------------

// A service migrating call sites one at a time must not end up running two
// independent limiters and two independent budgets against one remote.
func TestClientAndItsAdoptedFormsShareOneBudget(t *testing.T) {
	t.Parallel()
	b := mustBudget(t, BudgetConfig{Limit: 3})
	c := mustClient(t, Config{Unlimited: true, Budget: b, transport: okTransport()})

	resp, err := c.Do(req(t.Context(), t, http.MethodGet, "https://example.com/a"))
	if err != nil {
		t.Fatalf("Client.Do: %v", err)
	}
	closeBody(t, resp)

	resp, err = c.HTTPClient().Do(req(t.Context(), t, http.MethodGet, "https://example.com/b"))
	if err != nil {
		t.Fatalf("HTTPClient().Do: %v", err)
	}
	closeBody(t, resp)

	resp, err = c.Transport().RoundTrip(req(t.Context(), t, http.MethodGet, "https://example.com/c"))
	if err != nil {
		t.Fatalf("Transport().RoundTrip: %v", err)
	}
	closeBody(t, resp)

	if got := b.Stats().Used; got != 3 {
		t.Errorf("budget Used = %d, want 3 across all three shapes", got)
	}
	if got := c.BudgetRemaining(); got != 0 {
		t.Errorf("BudgetRemaining() = %d, want 0", got)
	}
	if got := c.CallsUsed(); got != 3 {
		t.Errorf("CallsUsed() = %d, want 3", got)
	}
	if c.Transport().Budget() != Budget(b) {
		t.Error("Transport().Budget() is not the client's budget")
	}
	if got := c.Transport().CallsUsed(); got != 3 {
		t.Errorf("Transport().CallsUsed() = %d, want the shared counter", got)
	}
}
