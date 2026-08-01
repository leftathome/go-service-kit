package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// --- helpers ---------------------------------------------------------------

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}
}

// mustClient fills in the mandatory identity/timeout fields so tests only
// state what they are actually exercising.
func mustClient(t *testing.T, cfg Config) *Client {
	t.Helper()
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
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// stubTransport answers in-process, so time-based tests never touch a socket
// and can run inside a synctest bubble.
type stubTransport struct {
	mu      sync.Mutex
	reqs    []*http.Request
	handler func(req *http.Request, attempt int) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req.Clone(req.Context()))
	attempt := len(s.reqs)
	s.mu.Unlock()
	return s.handler(req, attempt)
}

func (s *stubTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func (s *stubTransport) last() *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return nil
	}
	return s.reqs[len(s.reqs)-1]
}

func okTransport() *stubTransport {
	return &stubTransport{handler: func(req *http.Request, _ int) (*http.Response, error) {
		return stubResponse(req, http.StatusOK, nil), nil
	}}
}

func stubResponse(req *http.Request, status int, hdr map[string]string) *http.Response {
	h := make(http.Header, len(hdr))
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("body")),
		Request:    req,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
}

func get(t *testing.T, c *Client, url string) (*http.Response, error) {
	t.Helper()
	return c.Get(t.Context(), url)
}

// --- 1. timeouts are mandatory ---------------------------------------------

func TestNewRejectsTimeoutlessClient(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		Product:    "kittest",
		ContactURL: "https://example.invalid/bots",
		Unlimited:  true,
	})
	if err == nil {
		t.Fatal("New succeeded without a timeout; a timeout-less client must be unconstructable")
	}
	if !strings.Contains(err.Error(), "Timeout") {
		t.Fatalf("err = %v, want it to name Timeout", err)
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("New(Config{}) succeeded, want validation errors")
	}
}

func TestNewValidatesIdentityAndRateSettings(t *testing.T) {
	t.Parallel()
	base := Config{Product: "kittest", ContactURL: "https://example.invalid/bots", Timeout: time.Second, Unlimited: true}

	cases := map[string]func(*Config){
		"missing product":     func(c *Config) { c.Product = "" },
		"missing contact":     func(c *Config) { c.ContactURL = "" },
		"contact not a url":   func(c *Config) { c.ContactURL = "someone@example.invalid" },
		"unlimited plus rate": func(c *Config) { c.RequestsPerSecond = 5 },
		"negative rate":       func(c *Config) { c.Unlimited = false; c.RequestsPerSecond = -1 },
		"bad scheme":          func(c *Config) { c.AllowedSchemes = []string{"gopher"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New succeeded for %q, want error", name)
			}
		})
	}

	if _, err := New(base); err != nil {
		t.Fatalf("New(valid) = %v", err)
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, _ int) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}}
		c := mustClient(t, Config{Timeout: 5 * time.Second, transport: tr})

		start := time.Now()
		resp, err := get(t, c, "https://example.com/slow")
		if err == nil {
			closeBody(t, resp)
			t.Fatal("want timeout error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Fatalf("elapsed = %v, want 5s", elapsed)
		}
	})
}

// --- 2. rate limiting -------------------------------------------------------

func TestRateLimitedByDefault(t *testing.T) {
	t.Parallel()
	c, err := New(Config{
		Product:    "kittest",
		ContactURL: "https://example.invalid/bots",
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := float64(c.limiter.Limit()); got != DefaultRequestsPerSecond {
		t.Fatalf("default limit = %v, want %v", got, DefaultRequestsPerSecond)
	}
}

func TestRateLimiterPacesRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := okTransport()
		var delays []time.Duration
		c := mustClient(t, Config{
			RequestsPerSecond: 2,
			Burst:             1,
			transport:         tr,
			Metrics:           MetricsFunc(func(ev CallEvent) { delays = append(delays, ev.LimiterDelay) }),
		})

		start := time.Now()
		for range 3 {
			resp, err := get(t, c, "https://example.com/")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			closeBody(t, resp)
		}
		// burst of 1: first is free, the next two cost 500ms each.
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("elapsed = %v, want 1s", elapsed)
		}
		if tr.count() != 3 {
			t.Fatalf("requests = %d, want 3", tr.count())
		}
		want := []time.Duration{0, 500 * time.Millisecond, 500 * time.Millisecond}
		if len(delays) != len(want) {
			t.Fatalf("limiter delays = %v, want %v", delays, want)
		}
		for i := range want {
			if delays[i] != want[i] {
				t.Fatalf("limiter delays = %v, want %v", delays, want)
			}
		}
	})
}

func TestUnlimitedIsOptIn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := okTransport()
		c := mustClient(t, Config{Unlimited: true, transport: tr})

		start := time.Now()
		for range 20 {
			resp, err := get(t, c, "https://example.com/")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			closeBody(t, resp)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("elapsed = %v, want 0 for an unlimited client", elapsed)
		}
	})
}

// --- 3/4. Retry-After, backoff, attempt cap ---------------------------------

func TestRetryAfterSecondsOn429(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, attempt int) (*http.Response, error) {
			if attempt == 1 {
				return stubResponse(req, http.StatusTooManyRequests, map[string]string{"Retry-After": "3"}), nil
			}
			return stubResponse(req, http.StatusOK, nil), nil
		}}
		c := mustClient(t, Config{Unlimited: true, transport: tr})

		start := time.Now()
		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer closeBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("elapsed = %v, want 3s (Retry-After honored)", elapsed)
		}
	})
}

func TestRetryAfterHTTPDateOn503(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, attempt int) (*http.Response, error) {
			if attempt == 1 {
				when := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
				return stubResponse(req, http.StatusServiceUnavailable, map[string]string{"Retry-After": when}), nil
			}
			return stubResponse(req, http.StatusOK, nil), nil
		}}
		c := mustClient(t, Config{Unlimited: true, transport: tr})

		start := time.Now()
		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer closeBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if elapsed := time.Since(start); elapsed != 2*time.Second {
			t.Fatalf("elapsed = %v, want 2s", elapsed)
		}
	})
}

func TestRetryAfterBeyondCapGivesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, _ int) (*http.Response, error) {
			return stubResponse(req, http.StatusTooManyRequests, map[string]string{"Retry-After": "3600"}), nil
		}}
		c := mustClient(t, Config{Unlimited: true, MaxRetryWait: time.Minute, transport: tr})

		start := time.Now()
		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer closeBody(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 handed back to the caller", resp.StatusCode)
		}
		if tr.count() != 1 {
			t.Fatalf("attempts = %d, want 1 (no waiting an hour in the request path)", tr.count())
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("elapsed = %v, want 0", elapsed)
		}
	})
}

func TestExponentialBackoffAndAttemptCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, _ int) (*http.Response, error) {
			return stubResponse(req, http.StatusServiceUnavailable, nil), nil
		}}
		c := mustClient(t, Config{
			Unlimited:   true,
			MaxAttempts: 3,
			BaseBackoff: 100 * time.Millisecond,
			transport:   tr,
			jitter:      func(d time.Duration) time.Duration { return d }, // deterministic
		})

		start := time.Now()
		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer closeBody(t, resp)

		if tr.count() != 3 {
			t.Fatalf("attempts = %d, want 3 (the configured cap)", tr.count())
		}
		// 100ms then 200ms; no wait after the final attempt.
		if elapsed := time.Since(start); elapsed != 300*time.Millisecond {
			t.Fatalf("elapsed = %v, want 300ms", elapsed)
		}
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want the last 503 returned to the caller", resp.StatusCode)
		}
	})
}

func TestBackoffIsCappedAtMaxBackoff(t *testing.T) {
	t.Parallel()
	c := mustClient(t, Config{
		BaseBackoff: time.Second,
		MaxBackoff:  4 * time.Second,
		MaxAttempts: 10,
		jitter:      func(d time.Duration) time.Duration { return d },
	})
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 4 * time.Second} {
		if got := c.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestDefaultJitterStaysInBoundsAndVaries(t *testing.T) {
	t.Parallel()
	const d = time.Second
	seen := make(map[time.Duration]bool)
	for range 200 {
		got := defaultJitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, d/2, d)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter produced a single value; it is not jittering")
	}
}

func TestNonRetryableStatusIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, _ int) (*http.Response, error) {
			return stubResponse(req, http.StatusInternalServerError, nil), nil
		}}
		c := mustClient(t, Config{Unlimited: true, MaxAttempts: 5, transport: tr})

		resp, err := get(t, c, "https://example.com/")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer closeBody(t, resp)
		if tr.count() != 1 {
			t.Fatalf("attempts = %d, want 1 (500 is not retried)", tr.count())
		}
	})
}

// --- 5. identifying User-Agent ----------------------------------------------

func TestUserAgentIdentifiesProductAndContact(t *testing.T) {
	t.Parallel()
	tr := okTransport()
	c, err := New(Config{
		Product:    "nagus",
		Version:    "1.4.2",
		ContactURL: "https://orac.local/bots",
		Timeout:    time.Second,
		Unlimited:  true,
		transport:  tr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "curl/8.0") // client policy wins
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	closeBody(t, resp)

	ua := tr.last().Header.Get("User-Agent")
	for _, want := range []string{"nagus", "1.4.2", "https://orac.local/bots"} {
		if !strings.Contains(ua, want) {
			t.Errorf("User-Agent %q missing %q", ua, want)
		}
	}
	if strings.Contains(ua, "curl") {
		t.Errorf("User-Agent %q kept the caller's value; identity is client policy", ua)
	}
	if req.Header.Get("User-Agent") != "curl/8.0" {
		t.Error("Do mutated the caller's request")
	}
}

// --- 8. redirect hygiene ----------------------------------------------------

type headerRecorder struct {
	mu   sync.Mutex
	seen []http.Header
}

func (h *headerRecorder) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, r.Header.Clone())
}

func (h *headerRecorder) at(i int) http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[i]
}

func (h *headerRecorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

func TestRedirectDropsAuthHeadersOnHostChange(t *testing.T) {
	t.Parallel()
	dest := &headerRecorder{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dest.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", http.StatusFound)
	}))
	defer origin.Close()

	c := mustClient(t, Config{AllowPrivateNetworks: true})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "super-secret")   // stdlib would forward this
	req.Header.Set("Authorization", "Bearer abc") // stdlib strips this
	req.Header.Set("Cookie", "sid=1")
	req.Header.Set("X-Vendor-Token", "t0ken")
	req.Header.Set("Accept", "application/json") // benign, must survive

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer closeBody(t, resp)

	if dest.count() != 1 {
		t.Fatalf("target hits = %d, want 1", dest.count())
	}
	got := dest.at(0)
	for _, h := range []string{"X-Api-Key", "Authorization", "Cookie", "X-Vendor-Token"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s leaked across hosts with value %q", h, v)
		}
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q, want it preserved", got.Get("Accept"))
	}
	if !strings.Contains(got.Get("User-Agent"), "kittest") {
		t.Errorf("User-Agent = %q, want the client identity preserved", got.Get("User-Agent"))
	}
}

func TestRedirectKeepsAuthHeadersOnSameHost(t *testing.T) {
	t.Parallel()
	rec := &headerRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path == "/landed" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/landed", http.StatusFound)
	}))
	defer srv.Close()

	c := mustClient(t, Config{AllowPrivateNetworks: true})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "super-secret")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer closeBody(t, resp)

	if rec.count() != 2 {
		t.Fatalf("hits = %d, want 2", rec.count())
	}
	if got := rec.at(1).Get("X-Api-Key"); got != "super-secret" {
		t.Errorf("X-Api-Key = %q on a same-host redirect, want it kept", got)
	}
}

func TestRedirectHopCap(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer srv.Close()

	c := mustClient(t, Config{AllowPrivateNetworks: true, MaxRedirects: 3})
	resp, err := get(t, c, srv.URL)
	if err == nil {
		closeBody(t, resp)
		t.Fatal("redirect loop returned no error")
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v, want ErrTooManyRedirects", err)
	}
	if got := hits.Load(); got != 4 {
		t.Fatalf("server hits = %d, want 4 (initial + 3 followed redirects)", got)
	}
}

func TestSensitiveHeaderDetection(t *testing.T) {
	t.Parallel()
	sensitive := []string{
		"Authorization", "authorization", "Cookie", "Proxy-Authorization",
		"X-Api-Key", "X-API-KEY", "X-Auth-Token", "X-Amz-Security-Token",
		"X-CSRF-Token", "X-Session-Id", "X-Client-Secret", "X-Signature", "X-Password",
	}
	for _, h := range sensitive {
		if !isSensitiveHeader(h, nil) {
			t.Errorf("isSensitiveHeader(%q) = false, want true", h)
		}
	}
	benign := []string{"Accept", "Accept-Encoding", "User-Agent", "Content-Type", "If-None-Match", "Range"}
	for _, h := range benign {
		if isSensitiveHeader(h, nil) {
			t.Errorf("isSensitiveHeader(%q) = true, want false", h)
		}
	}
	if !isSensitiveHeader("X-Tenant", map[string]bool{"x-tenant": true}) {
		t.Error("configured extra sensitive header not honored")
	}
}

// --- 9. TLS floor, no InsecureSkipVerify escape hatch -----------------------

func TestTLSFloorAndNoInsecureOption(t *testing.T) {
	t.Parallel()
	c := mustClient(t, Config{})
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.hc.Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil, want an explicit MinVersion")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", tr.TLSClientConfig.MinVersion)
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify is set")
	}

	// No public path may reach InsecureSkipVerify: a private CA belongs in the
	// trust store / SSL_CERT_FILE, not in a constructor option.
	rt := reflect.TypeOf(Config{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "insecure") || strings.Contains(lower, "skipverify") || strings.Contains(lower, "tlsconfig") {
			t.Errorf("Config.%s exposes TLS verification as an option", f.Name)
		}
		if f.Type == reflect.TypeOf(&tls.Config{}) || f.Type == reflect.TypeOf(tls.Config{}) {
			t.Errorf("Config.%s lets a caller supply a tls.Config", f.Name)
		}
		if f.Type == reflect.TypeOf((*http.RoundTripper)(nil)).Elem() {
			t.Errorf("Config.%s lets a caller replace the guarded transport", f.Name)
		}
	}
}

// --- 10. call budget metric -------------------------------------------------

func TestMetricsSeamRecordsEveryAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var events []CallEvent

		tr := &stubTransport{handler: func(req *http.Request, attempt int) (*http.Response, error) {
			if attempt == 1 {
				return stubResponse(req, http.StatusTooManyRequests, map[string]string{"Retry-After": "1"}), nil
			}
			return stubResponse(req, http.StatusOK, nil), nil
		}}
		c := mustClient(t, Config{
			RequestsPerSecond: 10,
			Burst:             1,
			transport:         tr,
			Metrics: MetricsFunc(func(ev CallEvent) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, ev)
			}),
		})

		resp, err := get(t, c, "https://api.example.com/things")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		closeBody(t, resp)

		mu.Lock()
		defer mu.Unlock()
		if len(events) != 2 {
			t.Fatalf("events = %d, want 2 (one per attempt)", len(events))
		}
		if events[0].StatusCode != http.StatusTooManyRequests || events[1].StatusCode != http.StatusOK {
			t.Fatalf("statuses = %d, %d", events[0].StatusCode, events[1].StatusCode)
		}
		for i, ev := range events {
			if ev.Attempt != i+1 {
				t.Errorf("event %d attempt = %d", i, ev.Attempt)
			}
			if ev.Host != "api.example.com" {
				t.Errorf("event %d host = %q", i, ev.Host)
			}
			if ev.Method != http.MethodGet {
				t.Errorf("event %d method = %q", i, ev.Method)
			}
			if ev.CallsUsed != int64(i+1) {
				t.Errorf("event %d CallsUsed = %d, want %d", i, ev.CallsUsed, i+1)
			}
			if ev.BudgetRemaining != -1 {
				t.Errorf("event %d BudgetRemaining = %d, want -1 (no budget set)", i, ev.BudgetRemaining)
			}
		}
		if c.CallsUsed() != 2 {
			t.Errorf("CallsUsed() = %d, want 2", c.CallsUsed())
		}
	})
}

func TestCallBudgetExhausts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var remaining []int64
		tr := okTransport()
		c := mustClient(t, Config{
			Unlimited:  true,
			CallBudget: 2,
			transport:  tr,
			Metrics:    MetricsFunc(func(ev CallEvent) { remaining = append(remaining, ev.BudgetRemaining) }),
		})

		for i := range 2 {
			resp, err := get(t, c, "https://example.com/")
			if err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
			closeBody(t, resp)
		}
		resp, err := get(t, c, "https://example.com/")
		if err == nil {
			closeBody(t, resp)
			t.Fatal("third call succeeded, want ErrCallBudgetExhausted")
		}
		if !errors.Is(err, ErrCallBudgetExhausted) {
			t.Fatalf("err = %v, want ErrCallBudgetExhausted", err)
		}
		if tr.count() != 2 {
			t.Fatalf("requests = %d, want 2", tr.count())
		}
		if want := []int64{1, 0}; len(remaining) != 2 || remaining[0] != want[0] || remaining[1] != want[1] {
			t.Fatalf("BudgetRemaining sequence = %v, want %v", remaining, want)
		}
	})
}

// --- scheme allowlist -------------------------------------------------------

func TestSchemeAllowlist(t *testing.T) {
	t.Parallel()
	tr := okTransport()
	c := mustClient(t, Config{Unlimited: true, transport: tr})

	for _, raw := range []string{"ftp://example.com/x", "file:///etc/passwd", "gopher://example.com"} {
		resp, err := get(t, c, raw)
		if err == nil {
			closeBody(t, resp)
			t.Fatalf("%s: want ErrDisallowedScheme", raw)
		}
		if !errors.Is(err, ErrDisallowedScheme) {
			t.Errorf("%s: err = %v, want ErrDisallowedScheme", raw, err)
		}
	}
	if tr.count() != 0 {
		t.Fatalf("transport saw %d requests, want 0", tr.count())
	}

	httpsOnly := mustClient(t, Config{Unlimited: true, AllowedSchemes: []string{"https"}, transport: tr})
	resp, err := get(t, httpsOnly, "http://example.com/")
	if err == nil {
		closeBody(t, resp)
		t.Fatal("http against an https-only client succeeded, want ErrDisallowedScheme")
	}
	if !errors.Is(err, ErrDisallowedScheme) {
		t.Errorf("http against an https-only client: err = %v, want ErrDisallowedScheme", err)
	}
}

// --- retries replay the body ------------------------------------------------

func TestRetryReplaysRequestBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var bodies []string
		tr := &stubTransport{handler: func(req *http.Request, attempt int) (*http.Response, error) {
			b, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			bodies = append(bodies, string(b))
			if attempt == 1 {
				return stubResponse(req, http.StatusServiceUnavailable, map[string]string{"Retry-After": "1"}), nil
			}
			return stubResponse(req, http.StatusOK, nil), nil
		}}
		c := mustClient(t, Config{Unlimited: true, transport: tr})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/",
			strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		closeBody(t, resp)

		if len(bodies) != 2 || bodies[0] != "payload" || bodies[1] != "payload" {
			t.Fatalf("bodies = %q, want the payload sent twice", bodies)
		}
	})
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &stubTransport{handler: func(req *http.Request, _ int) (*http.Response, error) {
			return stubResponse(req, http.StatusServiceUnavailable, map[string]string{"Retry-After": "30"}), nil
		}}
		c := mustClient(t, Config{Unlimited: true, MaxAttempts: 5, MaxRetryWait: time.Hour, transport: tr})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err == nil {
			closeBody(t, resp)
			t.Fatal("want a context error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}
