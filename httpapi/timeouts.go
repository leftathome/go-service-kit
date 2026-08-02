package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Server timeout defaults. Every one is a [Timeouts] field with this as its
// zero-value fallback.
//
// The numbers assume a JSON API behind a cluster-local Service. A service that
// streams responses, accepts large uploads, or serves a slow fan-out read needs
// different ones -- see [Timeouts] for how to say so and for the invariant that
// survives it.
const (
	// DefaultReadHeaderTimeout is the gosec G112 mitigation: the budget for a
	// client to finish sending request headers. A Slowloris client sends one
	// header byte per interval forever; this is what stops it holding a
	// connection.
	DefaultReadHeaderTimeout = 5 * time.Second

	// DefaultReadTimeout covers headers plus body.
	DefaultReadTimeout = 30 * time.Second

	// DefaultWriteTimeout is measured from the end of the request headers, so
	// it bounds handler time plus response write.
	DefaultWriteTimeout = 30 * time.Second

	// DefaultIdleTimeout is how long a keep-alive connection may sit unused.
	// Longer than a typical scrape interval so scrapers reuse connections.
	DefaultIdleTimeout = 120 * time.Second

	// DefaultMaxHeaderBytes bounds header memory per connection. This is the
	// stdlib default value, set EXPLICITLY so that the guarantee is in the code
	// rather than in a default that could change.
	DefaultMaxHeaderBytes = 1 << 20
)

// Timeouts overrides the [http.Server] budgets for one listener.
//
// # Zero means the default, never "no timeout"
//
// This is the whole point of the type, and it is the tension it exists to
// resolve. net/http's own convention is that a zero timeout means NO timeout,
// which is how a service ends up one forgotten struct field away from a
// Slowloris. This package's convention is the opposite: a zero field here means
// "use the documented default", and there is NO value of any field that yields
// an unbounded server. A timeout-less server remains UNCONSTRUCTABLE through
// this package's public API, which was true before this type existed and stays
// true now that the numbers are tunable.
//
// A NEGATIVE value would be net/http's way of spelling "no timeout", so it is
// rejected: [Timeouts.Validate] returns an error and the constructors panic on
// it. Failing loudly at startup is the only honest option -- silently
// substituting the default would mean a service that asked for no timeout got
// one anyway and never found out, and honouring it would break the invariant.
//
// # Raising WriteTimeout is sometimes correct
//
// WriteTimeout is measured from the end of the request headers, so it bounds
// handler time plus response write. When it expires mid-body the connection is
// closed with a partial response: the client sees TRUNCATED JSON, not an error
// status. That failure mode is why the default is deliberately generous for a
// point read and why it is worth raising rather than living with.
//
// nagus is the worked example. Its /watches endpoint is a fan-out READ that
// evaluates every configured watch -- a store query plus per-item valuation
// enrichment, some of it over the network -- and can legitimately exceed 30s.
// It is neither a stream nor an upload, which are the two exceptions the
// original fixed budget anticipated.
//
// Prefer, in this order:
//
//  1. Make the handler faster or bound its own work with context.WithTimeout
//     and return 504. A slow handler that reports its own timeout gives the
//     client an error it can act on; a truncated body gives it a parse failure
//     that looks like corruption.
//  2. Raise WriteTimeout here, to the smallest value that covers the p99 with
//     margin.
//
// Treat 120s as the practical ceiling for an API behind a cluster-local
// Service. Beyond that the request is a job, not a request, and the shape to
// reach for is a 202 plus a status endpoint -- not a longer budget. Nothing
// enforces that number: an enforced ceiling would just be a different arbitrary
// limit for the next service to fight.
//
// Note the interaction with lifecycle: an in-flight request may hold the drain
// for up to WriteTimeout, so raising it above lifecycle's DrainTimeout means
// the drain, not the timeout, is what cuts the response short on shutdown.
type Timeouts struct {
	// ReadHeaderTimeout is the budget for a client to finish sending request
	// headers. Zero means [DefaultReadHeaderTimeout].
	ReadHeaderTimeout time.Duration

	// ReadTimeout covers headers plus body. Zero means [DefaultReadTimeout].
	ReadTimeout time.Duration

	// WriteTimeout bounds handler time plus response write. Zero means
	// [DefaultWriteTimeout]. See the type doc before raising it.
	WriteTimeout time.Duration

	// IdleTimeout bounds an unused keep-alive connection. Zero means
	// [DefaultIdleTimeout].
	IdleTimeout time.Duration

	// MaxHeaderBytes bounds header memory per connection. Zero means
	// [DefaultMaxHeaderBytes].
	MaxHeaderBytes int
}

// WithDefaults returns a copy with every zero-valued field filled in. The
// constructors call it; it is exported so a test or a chart can compute the
// same numbers.
func (t Timeouts) WithDefaults() Timeouts {
	if t.ReadHeaderTimeout == 0 {
		t.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if t.ReadTimeout == 0 {
		t.ReadTimeout = DefaultReadTimeout
	}
	if t.WriteTimeout == 0 {
		t.WriteTimeout = DefaultWriteTimeout
	}
	if t.IdleTimeout == 0 {
		t.IdleTimeout = DefaultIdleTimeout
	}
	if t.MaxHeaderBytes == 0 {
		t.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	return t
}

// Validate reports the first field that would produce an unbounded server. A
// negative value is net/http's spelling of "no timeout" and this package does
// not offer that; see the type doc.
func (t Timeouts) Validate() error {
	for _, f := range []struct {
		name string
		d    time.Duration
	}{
		{"ReadHeaderTimeout", t.ReadHeaderTimeout},
		{"ReadTimeout", t.ReadTimeout},
		{"WriteTimeout", t.WriteTimeout},
		{"IdleTimeout", t.IdleTimeout},
	} {
		if f.d < 0 {
			return &negativeTimeoutError{Field: f.name, Value: f.d.String()}
		}
	}
	if t.MaxHeaderBytes < 0 {
		return &negativeTimeoutError{Field: "MaxHeaderBytes", Value: strconv.Itoa(t.MaxHeaderBytes)}
	}
	return nil
}

// negativeTimeoutError is a named type so the message is identical whichever
// field and whichever constructor produced it.
type negativeTimeoutError struct {
	Field string
	Value string
}

func (e *negativeTimeoutError) Error() string {
	return "httpapi: Timeouts." + e.Field + " is " + e.Value +
		`; a negative value means "no timeout" to net/http and this package does not offer that. ` +
		"Use zero for the default, or a positive value."
}

// newServer is the ONLY place an http.Server is constructed in this package,
// which is what makes "a timeout-less server is unconstructable" a property of
// the code rather than a habit. It panics on a negative budget rather than
// building a server that would violate the invariant.
func newServer(addr string, h http.Handler, logger *slog.Logger, t Timeouts) *http.Server {
	if logger == nil {
		logger = slog.Default()
	}
	if err := t.Validate(); err != nil {
		panic(err.Error())
	}
	t = t.WithDefaults()
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: t.ReadHeaderTimeout,
		ReadTimeout:       t.ReadTimeout,
		WriteTimeout:      t.WriteTimeout,
		IdleTimeout:       t.IdleTimeout,
		MaxHeaderBytes:    t.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}
