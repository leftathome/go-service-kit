package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Doer is the minimal HTTP interface a service should type its dependencies
// as. Both *http.Client and *Client satisfy it, so an integration written
// against Doer can be handed either without editing the integration.
//
// Type connector fields as Doer from day one:
//
//	type Config struct {
//	    HTTPClient outbound.Doer // *http.Client in tests, *outbound.Client live
//	}
//
// nagus is the reason this exists. Every one of its six integrations declared
// HTTPClient *http.Client, so adopting the kit's egress policy meant editing
// six files for no behavioural reason. A one-method interface removes that cost
// for every service that comes after.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

var (
	_ Doer              = (*Client)(nil)
	_ Doer              = (*http.Client)(nil)
	_ http.RoundTripper = (*Transport)(nil)
)

// Transport is the whole outbound policy expressed as an http.RoundTripper, so
// an existing *http.Client adopts it by setting one field:
//
//	tr, err := outbound.NewTransport(outbound.Config{...})
//	hc.Transport = tr
//
// Everything [Client] guarantees holds here, because this is the same
// machinery: the SSRF dial guard, the TLS 1.2 floor, the identifying
// User-Agent, the rate limiter, Retry-After and backoff, the call budget, and
// the CallEvent metrics seam.
//
// Two guarantees need explaining, because a RoundTripper sits BELOW the layer
// that normally provides them:
//
//   - Timeout. http.Client.Timeout is not available to a transport, so each
//     attempt is bounded by a context deadline instead, cancelled when the
//     response body is closed. That is the same end-to-end coverage
//     http.Client.Timeout gives -- headers and body -- and it survives being
//     attached to a client whose own Timeout is zero. A timeout-less
//     Transport is unconstructable, exactly as a timeout-less Client is.
//
//   - Redirect hygiene. The stdlib strips Authorization and Cookie on a
//     cross-domain redirect but forwards custom headers such as X-Api-Key,
//     and its own hop cap is a fixed 10. A CheckRedirect func belongs to the
//     client, which this layer does not own -- so the Transport detects a
//     redirect follow itself, through Request.Response (which net/http
//     populates only on client redirects), and applies the same cross-origin
//     strip and the same MaxRedirects cap. Put this transport on a bare
//     &http.Client{} and both still hold.
//
// Construct it with [NewTransport]; the zero value is not usable.
type Transport struct {
	eng          *engine
	base         http.RoundTripper
	timeout      time.Duration
	maxRedirects int
	sensitive    map[string]bool
}

// NewTransport validates cfg and builds a Transport. It accepts exactly the
// same Config as [New] and enforces exactly the same invariants.
func NewTransport(cfg Config) (*Transport, error) {
	b, err := build(cfg)
	if err != nil {
		return nil, err
	}
	return b.transport(), nil
}

// NewHTTPClient builds a plain *http.Client carrying the full outbound policy,
// for callers that need an *http.Client concretely -- a third-party SDK that
// takes one, say.
//
// Timeout is deliberately left at zero on the returned client: the per-attempt
// deadline lives in the Transport, where it also bounds the retries and the
// backoff waits that an http.Client.Timeout would cut across. Bound the whole
// call, retries included, with the request context -- the same rule
// [Client.Do] documents.
func NewHTTPClient(cfg Config) (*http.Client, error) {
	b, err := build(cfg)
	if err != nil {
		return nil, err
	}
	return b.httpClient(), nil
}

// Transport returns this client's policy as an http.RoundTripper. It SHARES
// the rate limiter and the call budget with the Client it came from, so a
// service can migrate call sites one at a time without accidentally running two
// independent quotas against one remote.
func (c *Client) Transport() *Transport { return c.tr }

// HTTPClient returns an *http.Client carrying this client's policy, sharing its
// rate limiter and call budget. See [NewHTTPClient] for why Timeout is zero.
func (c *Client) HTTPClient() *http.Client { return c.adopted }

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("outbound: nil request")
	}
	hops := redirectHops(req)
	if hops > t.maxRedirects {
		return nil, fmt.Errorf("%w: stopped after %d", ErrTooManyRedirects, t.maxRedirects)
	}
	if hops > 0 && !sameOriginAsRedirectSource(req) {
		// The client above us already followed the redirect and copied the
		// caller's headers onto the new request. Drop the auth-bearing ones
		// before they reach a host they were not issued for.
		req = req.Clone(req.Context())
		for name := range req.Header {
			if isSensitiveHeader(name, t.sensitive) {
				req.Header.Del(name)
			}
		}
	}
	return t.eng.do(req, t.sendOnce)
}

// CallsUsed reports how many attempts this transport has made. It shares the
// counter with the Client it was derived from, if any.
func (t *Transport) CallsUsed() int64 { return t.eng.used.Load() }

// Budget returns the configured call budget, or nil when the transport is
// unmetered.
func (t *Transport) Budget() Budget { return t.eng.budget }

// sendOnce performs one attempt under a fresh per-attempt deadline. The cancel
// is deferred to the response body's Close, so the deadline covers the body
// read too -- which is what http.Client.Timeout does, and what a naive
// context.WithTimeout around RoundTrip would get wrong by killing the body the
// instant RoundTrip returned.
func (t *Transport) sendOnce(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
	resp, err := t.base.RoundTrip(req.WithContext(ctx)) //nolint:bodyclose // returned to the caller, who owns the close
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelBody releases the attempt's context when the body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel() // context.CancelFunc is idempotent
	return err
}

// maxHopWalk bounds the Request.Response walk so a pathological chain cannot
// turn hop counting into an unbounded loop.
const maxHopWalk = 64

// redirectHops reports how many redirects were followed to produce req.
//
// net/http populates Request.Response ONLY on a request it created while
// following a client redirect, and the stdlib transport sets Response.Request
// on the way back, so the pair forms a walkable chain. An initial request has
// no Response and scores zero.
func redirectHops(req *http.Request) int {
	n := 0
	//nolint:bodyclose // walking a redirect chain the client already owns
	for r := req.Response; r != nil && n <= maxHopWalk; r = previousResponse(r) {
		n++
	}
	return n
}

func previousResponse(r *http.Response) *http.Response {
	if r.Request == nil {
		return nil
	}
	return r.Request.Response
}

// sameOriginAsRedirectSource reports whether req targets the same origin as the
// request whose response redirected here. A missing link in the chain is
// treated as a host change: the safe answer when the origin is unknown is to
// drop the credentials.
func sameOriginAsRedirectSource(req *http.Request) bool {
	if req.Response == nil || req.Response.Request == nil || req.Response.Request.URL == nil {
		return false
	}
	return sameOrigin(req.Response.Request.URL, req.URL)
}
