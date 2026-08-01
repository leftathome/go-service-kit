package outbound

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingDialer stands in for the real net.Dialer so guard tests never touch
// the network. It records every address the guard actually tried to dial.
type recordingDialer struct {
	mu    sync.Mutex
	addrs []string
	err   error
}

func (r *recordingDialer) dial(_ context.Context, _, addr string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, addr)
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (r *recordingDialer) dialed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addrs...)
}

func staticLookup(m map[string][]string) lookupFunc {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		raw, ok := m[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		addrs := make([]netip.Addr, 0, len(raw))
		for _, s := range raw {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		return addrs, nil
	}
}

func TestBlockedIP(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"127.0.0.1",        // loopback
		"127.9.9.9",        // loopback range
		"::1",              // v6 loopback
		"10.1.2.3",         // RFC1918
		"172.16.0.1",       // RFC1918
		"192.168.1.10",     // RFC1918
		"169.254.169.254",  // link-local / cloud metadata
		"fe80::1",          // v6 link-local
		"fd00::1",          // unique local
		"100.64.0.1",       // CGNAT
		"0.0.0.0",          // unspecified
		"::",               // v6 unspecified
		"255.255.255.255",  // broadcast
		"224.0.0.1",        // multicast
		"ff02::1",          // v6 multicast
		"::ffff:127.0.0.1", // v4-mapped loopback
		"::ffff:10.0.0.1",  // v4-mapped RFC1918
		"192.0.0.1",        // IETF protocol assignments
		"64:ff9b::7f00:1",  // NAT64 wrapping loopback
	}
	for _, s := range blocked {
		if !blockedIP(netip.MustParseAddr(s)) {
			t.Errorf("blockedIP(%s) = false, want true", s)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if blockedIP(netip.MustParseAddr(s)) {
			t.Errorf("blockedIP(%s) = true, want false", s)
		}
	}
}

// A DNS-rebinding attempt: a public-looking name that resolves to loopback.
// The guard must refuse at dial time, on the resolved IP.
func TestGuardedDialBlocksRebinding(t *testing.T) {
	t.Parallel()
	base := &recordingDialer{}
	dial := guardedDial(base.dial, staticLookup(map[string][]string{
		"totally.public.example": {"127.0.0.1"},
	}))

	conn, err := dial(t.Context(), "tcp", "totally.public.example:443")
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial succeeded, want ErrBlockedAddress")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if got := base.dialed(); len(got) != 0 {
		t.Fatalf("base dialer was called with %v, want no calls", got)
	}
}

func TestGuardedDialUsesResolvedIP(t *testing.T) {
	t.Parallel()
	base := &recordingDialer{}
	dial := guardedDial(base.dial, staticLookup(map[string][]string{
		"example.com": {"93.184.216.34"},
	}))

	conn, err := dial(t.Context(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	// Dialing the checked IP (not the name) closes the gap between the check
	// and the connect: there is no second resolution to poison.
	want := []string{"93.184.216.34:443"}
	got := base.dialed()
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("dialed %v, want %v", got, want)
	}
}

func TestGuardedDialBlocksIPLiteral(t *testing.T) {
	t.Parallel()
	base := &recordingDialer{}
	dial := guardedDial(base.dial, func(context.Context, string) ([]netip.Addr, error) {
		t.Error("lookup called for an IP literal")
		return nil, nil
	})

	if _, err := dial(t.Context(), "tcp", "10.0.0.5:8080"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if got := base.dialed(); len(got) != 0 {
		t.Fatalf("base dialer called with %v, want no calls", got)
	}
}

// A record set mixing public and private answers is itself the tell; refuse
// the whole dial rather than racing the attacker for the "good" answer.
func TestGuardedDialBlocksMixedResolution(t *testing.T) {
	t.Parallel()
	base := &recordingDialer{}
	dial := guardedDial(base.dial, staticLookup(map[string][]string{
		"mixed.example": {"93.184.216.34", "192.168.0.9"},
	}))

	if _, err := dial(t.Context(), "tcp", "mixed.example:80"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if got := base.dialed(); len(got) != 0 {
		t.Fatalf("base dialer called with %v, want no calls", got)
	}
}

func TestGuardedDialLookupFailure(t *testing.T) {
	t.Parallel()
	base := &recordingDialer{}
	dial := guardedDial(base.dial, staticLookup(nil))

	_, err := dial(t.Context(), "tcp", "nope.example:80")
	if err == nil {
		t.Fatal("want error for unresolvable host")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("err = %v, want a *net.DNSError", err)
	}
}

// End to end: the guard is wired into the client's transport, so a rebinding
// name never reaches the server even though the server is reachable.
func TestClientBlocksRebindingHost(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	c, err := New(Config{
		Product:    "kittest",
		ContactURL: "https://example.invalid/bots",
		Timeout:    5 * time.Second,
		Unlimited:  true,
		lookup:     staticLookup(map[string][]string{"totally.public.example": {host}}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://totally.public.example:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		closeBody(t, resp)
		t.Fatal("request succeeded, want ErrBlockedAddress")
	}
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("server saw %d hits, want 0", got)
	}
}

// The opt-out exists for internal clients talking to in-cluster services.
func TestAllowPrivateNetworksReachesLoopback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := mustClient(t, Config{AllowPrivateNetworks: true})
	resp, err := c.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer closeBody(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}
