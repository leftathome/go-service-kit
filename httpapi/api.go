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
// # Single-port mode is a MIGRATION AFFORDANCE, not an equal option
//
// [AdminHandlers] and [Admin.RegisterOn] will mount the admin routes on an
// existing mux, including the API listener's. That exists for ONE reason: a
// live service cannot always do a hard cutover.
//
// Consider the deployment nagus actually runs -- replicas: 1, strategy:
// Recreate, everything on :8080 today. Splitting the listeners means the chart
// moves all three probes to :9090 in the same release that the binary starts
// listening there, and chart and image become an atomic pair forever after.
// The deploy is survivable. The ROLLBACK is what kills you: reverting the image
// "to get back to a known-good binary" while leaving the new chart in place
// gives you a pod that does not listen on :9090, probed on :9090, killed by
// liveness -- and under Recreate the old pod is already gone. That is a full
// outage produced by the recovery action.
//
// The transition path removes the pairing. Serve the admin routes on BOTH ports
// for one release, move the chart's probes and the ServiceMonitor to the admin
// port in the next, then drop the API-port copies in a third. Every step is
// independently revertible because no release requires the other side to move
// with it.
//
//	admin := httpapi.NewAdmin(httpapi.AdminOptions{Addr: ":9090", ...})
//	admin.RegisterOn(api.Mux)   // TRANSITIONAL: remove once probes are on :9090
//	// and exclude them from this listener's RED metrics while they are here:
//	//   httpapi.Options{InstrumentationFilter: httpapi.ExceptAdminPaths}
//
// TWO LISTENERS REMAIN THE DESTINATION. The reason is at the top of this doc
// and it does not weaken for a migrating service: /metrics is a live inventory
// of what the process does, /debug/pprof will hand out heap dumps, and the
// OpenAPI document is a machine-readable map of every route and type the
// service accepts. The API port is the only one that is ever a candidate for an
// Ingress, so anything left on it is one mistaken Ingress away from being
// public. A service that stops after the first step has taken on the debt
// permanently and should say so in a bead.
//
// # Non-REST surfaces on the public listener
//
// Not every consumer-facing endpoint can be a huma operation. The fleet case is
// MCP: a single POST whose behaviour is dispatched by a "method" member in a
// polymorphic JSON-RPC 2.0 body. OpenAPI cannot describe that usefully, and a
// JSON-RPC error is not problem+json.
//
// The supported pattern is [API.RawRoute], which is a thin, greppable wrapper
// over API.Mux:
//
//	api.RawRoute("POST /mcp", mcpHandler)
//
// A raw route gets everything the listener provides except the OpenAPI
// document: the hardened server, otelhttp tracing, and RED metrics with
// http_route set to the pattern, so /mcp appears in the same dashboards as
// every huma operation.
//
// THE HONEST CAVEAT. A raw route is an EXCEPTION to this package's
// one-error-format-per-listener rule. [WriteProblem] emits RFC 9457
// problem+json and every huma operation does the same, but a JSON-RPC endpoint
// MUST answer with a JSON-RPC error object or its clients break. So a listener
// carrying both has two error formats on it, and a client cannot infer which
// one it will get from the status code alone -- it has to know the route. That
// is a real cost and the reason raw routes are named rather than ambient. Keep
// it bounded: the format is per ROUTE and each raw route documents its own, the
// rest of the listener stays problem+json, and a route that COULD be a huma
// operation should be one. [API.RawRoutes] lists what a service has opted out
// of, so a test can assert the set has not grown by accident.
//
// # Server hardening is configurable, but never absent
//
// Both constructors set ReadHeaderTimeout (gosec G112), ReadTimeout,
// WriteTimeout, IdleTimeout, and MaxHeaderBytes. [Options.Timeouts] tunes them
// -- a fan-out read that legitimately exceeds 30s otherwise gets a TRUNCATED
// body rather than an error -- but zero means the documented default and a
// negative value is rejected, so a timeout-less server stays UNCONSTRUCTABLE
// through this package's public API. See [Timeouts] for the full tension.
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
	"slices"
	"strconv"
	"strings"
	"sync"

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

	// Timeouts tunes the server budgets. The zero value yields the documented
	// defaults; see [Timeouts] before changing any of them, and in particular
	// before raising WriteTimeout for a slow read.
	Timeouts Timeouts

	// InstrumentationFilter selects which requests the otelhttp middleware
	// observes. It is called with the request BEFORE the mux routes it;
	// returning false means the request is neither traced nor counted in the
	// RED metrics. Nil observes everything, which is the right default.
	//
	// The reason it exists is the transitional single-port mode: admin routes
	// mounted on this listener would otherwise put every Prometheus scrape and
	// every kubelet probe into the RED series, which is a permanent baseline of
	// self-referential fake traffic on every dashboard -- exactly what the
	// listener split avoids. Pass [ExceptAdminPaths] while they are here.
	InstrumentationFilter func(*http.Request) bool

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
	// so prefer huma, and prefer [API.RawRoute] over touching this directly:
	// it records the pattern so [API.RawRoutes] can report it.
	Mux *http.ServeMux

	mu        sync.Mutex
	rawRoutes []string
}

// RawRoute registers a handler that cannot be a huma operation -- an MCP
// endpoint, a webhook receiver with a non-JSON body, a legacy JSON shape a
// consumer parses field by field. See "Non-REST surfaces on the public
// listener" in the package doc for the trade, which includes an explicit
// exception to the one-error-format-per-listener rule.
//
// pattern is a [http.ServeMux] pattern and MUST be method-qualified
// ("POST /mcp", not "/mcp"): an unqualified pattern answers every method, so a
// GET from a browser or a crawler reaches a handler written for one verb. A
// route that legitimately serves several methods registers each one. RawRoute
// panics on an unqualified pattern, on an empty pattern, and on a nil handler,
// because all three are wiring bugs that are silent at runtime.
//
// The handler is otherwise exactly as if registered on API.Mux: instrumented,
// traced, and absent from the OpenAPI document.
func (a *API) RawRoute(pattern string, h http.Handler) {
	if h == nil {
		panic("httpapi: RawRoute " + pattern + " has a nil handler")
	}
	method, rest, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || strings.TrimSpace(rest) == "" || strings.HasPrefix(pattern, "/") {
		panic("httpapi: RawRoute pattern " + strconv.Quote(pattern) +
			` must be method-qualified, e.g. "POST /mcp"`)
	}

	a.mu.Lock()
	a.rawRoutes = append(a.rawRoutes, pattern)
	slices.Sort(a.rawRoutes)
	a.mu.Unlock()

	a.Mux.Handle(pattern, h)
}

// RawRouteFunc is [API.RawRoute] for a plain handler function.
func (a *API) RawRouteFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	if h == nil {
		panic("httpapi: RawRouteFunc " + pattern + " has a nil handler")
	}
	a.RawRoute(pattern, http.HandlerFunc(h))
}

// RawRoutes returns the patterns registered through [API.RawRoute], sorted.
//
// This is the listener's opt-out list: everything here is absent from the
// OpenAPI document and may answer with an error format other than problem+json.
// Assert on it in a test, so that set only ever grows deliberately.
func (a *API) RawRoutes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rawRoutes)
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
	otelOpts := []otelhttp.Option{
		otelhttp.WithTracerProvider(opts.TracerProvider),
		otelhttp.WithMeterProvider(opts.MeterProvider),
	}
	if opts.InstrumentationFilter != nil {
		otelOpts = append(otelOpts, otelhttp.WithFilter(otelhttp.Filter(opts.InstrumentationFilter)))
	}
	instrumented := otelhttp.NewHandler(mux, "http.server", otelOpts...)

	handler := instrumented
	for i := len(opts.Middleware) - 1; i >= 0; i-- {
		if opts.Middleware[i] == nil {
			continue
		}
		handler = opts.Middleware[i](handler)
	}

	return &API{
		Server: newServer(addr, handler, opts.Logger, opts.Timeouts),
		Huma:   humaAPI,
		Mux:    mux,
	}
}

// Handler returns the fully wrapped handler the server serves. Useful in tests
// that drive the listener through httptest without binding a port.
func (a *API) Handler() http.Handler { return a.Server.Handler }
