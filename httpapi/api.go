// Package httpapi owns a service's two HTTP listeners.
//
// # Two listeners, on purpose
//
// PUBLIC (default :8080) carries the huma API and, when enabled, the OpenAPI
// document and the documentation UI. ADMIN (default :9090) carries /healthz,
// /readyz, /metrics, and net/http/pprof. Neither serves the other's paths, and
// the split is enforced by tests rather than by convention.
//
// The reason is that only one of the two ports is ever a candidate for an
// Ingress. /metrics is a live inventory of what the process does and how much
// of it; /debug/pprof will hand out heap and goroutine dumps, and
// /debug/pprof/profile will burn 30 seconds of CPU on request; and the OpenAPI
// document is a machine-readable map of every route, parameter, and type the
// service accepts. Putting those behind the same listener as the API means one
// mistaken Ingress publishes all of them. Splitting the listener makes exposure
// a deliberate act. The ServiceMonitor targets the admin port.
//
// # Composition
//
//	obsp, _ := obs.Setup(ctx, obsCfg)
//
//	ready := httpapi.NewReadiness()
//	ready.Register("store", store.Ping)
//
//	api := httpapi.New(httpapi.Options{
//	    Addr:           cfg.Addr,
//	    Title:          "widgets", Version: version,
//	    DocsEnabled:    cfg.DocsEnabled,
//	    TracerProvider: obsp.TracerProvider,
//	    MeterProvider:  obsp.MeterProvider,
//	    Logger:         obsp.Logger,
//	})
//	widget.Register(api.Huma, store)   // service routes
//
//	admin := httpapi.NewAdmin(httpapi.AdminOptions{
//	    Addr:         cfg.AdminAddr,
//	    Readiness:    ready,
//	    Registry:     obsp.PromRegistry,
//	    PprofEnabled: cfg.PprofEnabled,
//	})
//
//	lifecycle.Run(ctx, lifecycle.Spec{
//	    Servers:   []*http.Server{api.Server, admin.Server},
//	    Readiness: ready,
//	    Flush:     obsp.Shutdown,
//	})
//
// # Server hardening is not configurable
//
// Both constructors set ReadHeaderTimeout (gosec G112), ReadTimeout,
// WriteTimeout, IdleTimeout, and MaxHeaderBytes. There is no Options field for
// any of them, so a timeout-less server is UNCONSTRUCTABLE through this
// package's public API. Anything a service would plausibly want to vary is an
// Options field with a working default; anything security-relevant is not a
// field at all.
//
// # huma stops here
//
// huma types appear in this package and in a service's handler file. They must
// not reach domain types: a Widget does not import huma, and neither does a
// store adapter. The OpenAPI document is derived from the Go types, so the
// spec cannot drift from the code -- and httpapi's golden test means a change
// to it shows up as a diff in review.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Default listen addresses. The public port is the only one that may ever sit
// behind an Ingress; see the package doc.
const (
	DefaultAddr      = ":8080"
	DefaultAdminAddr = ":9090"
)

// Server timeouts. Not Options fields, by design -- see the package doc.
//
// The numbers assume a JSON API behind a cluster-local Service. A service that
// streams responses or accepts large uploads needs different ones and should
// say so in a bead rather than growing a knob here that every other service
// then has to reason about.
const (
	// readHeaderTimeout is the gosec G112 mitigation: the budget for a client
	// to finish sending request headers. A Slowloris client sends one header
	// byte per interval forever; this is what stops it holding a connection.
	readHeaderTimeout = 5 * time.Second

	// readTimeout covers headers plus body.
	readTimeout = 30 * time.Second

	// writeTimeout is measured from the end of the request headers, so it
	// bounds handler time plus response write.
	writeTimeout = 30 * time.Second

	// idleTimeout is how long a keep-alive connection may sit unused. Longer
	// than a typical scrape interval so scrapers reuse connections.
	idleTimeout = 120 * time.Second

	// maxHeaderBytes bounds header memory per connection. This is the stdlib
	// default value, set EXPLICITLY so that the guarantee is in the code
	// rather than in a default that could change.
	maxHeaderBytes = 1 << 20
)

// RED metric names as they appear on /metrics, produced by otelhttp
// v0.69.0 through the OTel Prometheus exporter with the
// UnderscoreEscapingWithSuffixes translation strategy that obs.Setup pins.
//
// These are OTel-namespaced names. They are NOT promhttp's
// http_requests_total / http_request_duration_seconds, and a dashboard written
// against those renders empty. There is no request COUNT metric: use
// the _count series of the duration histogram for rate and error rate.
//
//	http_server_request_duration_seconds        histogram  latency and, via _count, rate
//	http_server_request_body_size_bytes         histogram  inbound payload size
//	http_server_response_body_size_bytes        histogram  outbound payload size
//
// Explicit histogram bucket boundaries, set by otelhttp (seconds):
// 0.005 0.01 0.025 0.05 0.075 0.1 0.25 0.5 0.75 1 2.5 5 7.5 10.
//
// Label set on every RED series, asserted by TestREDMetricLabelSetIsBounded:
//
//	http_request_method         GET, POST, ... (non-standard methods become _OTHER)
//	http_response_status_code   200, 404, ...
//	http_route                  the ROUTE TEMPLATE, e.g. /widgets/{id}
//	network_protocol_name       http
//	network_protocol_version    1.1
//	server_address              the Host header's host part
//	url_scheme                  http or https
//	otel_scope_name             go.opentelemetry.io/contrib/.../otelhttp
//	otel_scope_version          the otelhttp version
//	otel_scope_schema_url       the semconv schema URL
//
// http_route is absent on requests that matched no route. Nothing in the set
// is unbounded: no url_path, no url_full, no user_agent_original, no
// client_address. That is the whole point -- one crawler walking
// /widgets/<uuid> would otherwise mint a series per request.
const (
	RequestDurationMetricName = "http_server_request_duration_seconds"
	RequestBodySizeMetricName = "http_server_request_body_size_bytes"
	ResponseBodySizeMetric    = "http_server_response_body_size_bytes"
)

// REDMetricNames lists the families above, for tests and dashboards.
var REDMetricNames = []string{
	RequestDurationMetricName,
	RequestBodySizeMetricName,
	ResponseBodySizeMetric,
}

// Options configures the PUBLIC listener. The zero value is valid: it yields a
// hardened, uninstrumented, undocumented API on DefaultAddr.
type Options struct {
	// Addr is the listen address. Default DefaultAddr (":8080").
	Addr string

	// Title, Version, and Description populate the OpenAPI info block. Title
	// defaults to "service" and Version to "0.0.0".
	Title       string
	Version     string
	Description string

	// DocsEnabled turns on /docs, /openapi.json, and /openapi.yaml. When
	// false, all three are absent from the mux and return 404: the OpenAPI
	// document is a machine-readable map of the attack surface, and a service
	// with no consumers reading it at runtime should not publish it.
	//
	// The API itself is unaffected either way.
	DocsEnabled bool

	// TracerProvider and MeterProvider drive the otelhttp middleware. Pass
	// obs.Providers.TracerProvider and obs.Providers.MeterProvider. Nil means
	// the OTel globals, which obs.Setup also sets.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider

	// Logger is used for the listener's own diagnostics. Nil means
	// slog.Default().
	Logger *slog.Logger

	// Middleware is the RESERVED AUTH SLOT. Handlers are applied outermost
	// first, wrapping everything on this listener including /docs and the
	// spec endpoints. Adding a bearer-token check later is a wiring change
	// here, not a redesign.
	//
	// Reject with WriteProblem so middleware failures have the same wire shape
	// as handler failures.
	//
	// Middleware runs OUTSIDE the otelhttp instrumentation, so a request it
	// rejects is not traced and not counted in the RED metrics. That is a
	// deliberate trade, not an oversight.
	//
	// otelhttp reads the matched route template off r.Pattern, on the exact
	// *http.Request pointer it handed downstream. ServeMux sets that field in
	// place, so the two must be adjacent: anything between them that clones
	// the request -- and r.WithContext, the most common thing a middleware
	// does, clones -- would leave otelhttp reading an untouched copy, silently
	// dropping http_route from every series. Losing the route label loses the
	// cardinality guard with it.
	//
	// A middleware that must be observable should record its own counter.
	Middleware []func(http.Handler) http.Handler
}

// API is the public listener.
type API struct {
	// Server is the configured http.Server. lifecycle.Run starts and drains
	// it; do not call ListenAndServe yourself.
	Server *http.Server

	// Huma is where a service registers its operations:
	//
	//	huma.Register(api.Huma, huma.Operation{...}, handler)
	Huma huma.API

	// Mux is the underlying router, for the rare hand-written handler that
	// cannot be a huma operation (a webhook receiver with a non-JSON body, for
	// instance). Anything registered here is absent from the OpenAPI document,
	// so prefer huma.
	Mux *http.ServeMux
}

// New builds the public listener. Register operations on the returned
// API.Huma, then hand API.Server to lifecycle.Run.
func New(opts Options) *API {
	addr := opts.Addr
	if addr == "" {
		addr = DefaultAddr
	}
	title := opts.Title
	if title == "" {
		title = "service"
	}
	version := opts.Version
	if version == "" {
		version = "0.0.0"
	}

	mux := http.NewServeMux()

	cfg := huma.DefaultConfig(title, version)
	if opts.Description != "" {
		cfg.Info.Description = opts.Description
	}

	// huma's own docs renderers all load Stoplight Elements, Scalar, or
	// Swagger UI from unpkg.com. That is a hard no in an air-gapped or
	// strict-CSP cluster, so the built-in route is off and [docsHandler]
	// serves a self-hosted UI instead.
	cfg.DocsPath = ""

	// The /schemas route and the $schema-injecting link transformer are both
	// off. The transformer adds a $schema member to every response body and a
	// Link header pointing at /schemas/<Name>.json; that changes the shape of
	// the data plane, is not part of the house envelope, and would make
	// response bodies differ depending on whether docs happen to be enabled.
	cfg.SchemasPath = ""
	cfg.CreateHooks = nil

	if opts.DocsEnabled {
		cfg.OpenAPIPath = "/openapi"
	} else {
		cfg.OpenAPIPath = ""
	}

	humaAPI := humago.New(mux, cfg)

	if opts.DocsEnabled {
		registerDocs(mux, title)
	}

	// otelhttp sits directly around the mux so that it reads the route
	// template off the same *http.Request the mux annotated. See
	// Options.Middleware for why user middleware goes outside it. A nil
	// provider is ignored by these options, which falls back to the OTel
	// globals that obs.Setup installs.
	instrumented := otelhttp.NewHandler(
		mux,
		"http.server",
		otelhttp.WithTracerProvider(opts.TracerProvider),
		otelhttp.WithMeterProvider(opts.MeterProvider),
	)

	handler := instrumented
	for i := len(opts.Middleware) - 1; i >= 0; i-- {
		if opts.Middleware[i] == nil {
			continue
		}
		handler = opts.Middleware[i](handler)
	}

	return &API{
		Server: newServer(addr, handler, opts.Logger),
		Huma:   humaAPI,
		Mux:    mux,
	}
}

// Handler returns the fully wrapped handler the server serves. Useful in tests
// that drive the listener through httptest without binding a port.
func (a *API) Handler() http.Handler { return a.Server.Handler }

// newServer is the ONLY place an http.Server is constructed in this package,
// which is what makes "a timeout-less server is unconstructable" a property of
// the code rather than a habit.
func newServer(addr string, h http.Handler, logger *slog.Logger) *http.Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}
