package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Admin listener paths. All four are on the ADMIN port and none of them is on
// the public one.
const (
	HealthzPath = "/healthz"
	ReadyzPath  = "/readyz"
	MetricsPath = "/metrics"
	PprofPrefix = "/debug/pprof/"
)

// AdminOptions configures the ADMIN listener. The zero value is valid and
// yields /healthz, an always-ready /readyz, and an empty /metrics, which is
// what an incompletely wired service should look like: reachable and honest.
type AdminOptions struct {
	// Addr is the listen address. Default DefaultAdminAddr (":9090").
	Addr string

	// Readiness backs /readyz. Nil means a readiness handle with no checks,
	// which always reports ready -- fine for a service with no dependencies,
	// wrong for one that has them.
	Readiness *Readiness

	// Registry backs /metrics. Pass obs.Providers.PromRegistry. Nil means an
	// empty registry: /metrics answers 200 with no families rather than 404,
	// so a ServiceMonitor scraping a half-wired service reports "up" with no
	// data instead of a scrape error that looks like a network problem.
	Registry *prometheus.Registry

	// PprofEnabled mounts net/http/pprof under /debug/pprof/.
	//
	// Off by default. Even on the admin port it is a real capability: /heap
	// and /goroutine dump memory contents and stack traces, and /profile burns
	// 30 seconds of CPU per request. Turn it on to debug, turn it back off.
	PprofEnabled bool

	// Logger is used for the listener's own diagnostics and for reporting
	// errors during a /metrics scrape. Nil means slog.Default().
	Logger *slog.Logger

	// Timeouts tunes the server budgets. The zero value yields the documented
	// defaults. A registry with a very large number of families can make a
	// scrape slow enough to matter; that is the one reason to touch this here.
	// See [Timeouts].
	Timeouts Timeouts
}

// AdminHandlers builds the admin routes WITHOUT a listener, keyed by
// [http.ServeMux] pattern.
//
// The keys are method-qualified ("GET /healthz"), so mounting them on any mux
// keeps the strictness the admin listener has: a POST to /healthz is a 405 from
// the mux rather than a silent 200. That is the property a service loses when
// it hand-rolls the three routes during a migration.
//
// Use it when the routes must go somewhere this package does not own -- an
// existing mux built elsewhere, a router that is not *http.ServeMux, a test.
// For the common case of an *http.ServeMux, [Admin.RegisterOn] is the same
// thing with the loop written for you.
//
// SINGLE-PORT MODE IS A MIGRATION AFFORDANCE. Read "Single-port mode is a
// MIGRATION AFFORDANCE, not an equal option" in the package doc before mounting
// these on an API listener; two listeners remain the destination, and the
// reason is that only one of the two ports is ever a candidate for an Ingress.
func AdminHandlers(opts AdminOptions) map[string]http.Handler {
	ready := opts.Readiness
	if ready == nil {
		ready = NewReadiness()
	}
	reg := opts.Registry
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	h := map[string]http.Handler{
		// LIVENESS. Static 200 for as long as the process can accept a
		// connection and run a handler. It deliberately consults nothing: see
		// the Readiness doc for why a dependency check here turns an outage
		// into a CrashLoopBackOff.
		"GET " + HealthzPath: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		}),

		// READINESS. Dependency checks plus the shutting-down gate that
		// lifecycle.Run flips on SIGTERM.
		"GET " + ReadyzPath: ready.Handler(),

		"GET " + MetricsPath: promhttp.HandlerFor(reg, promhttp.HandlerOpts{
			ErrorLog:            slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
			ErrorHandling:       promhttp.HTTPErrorOnError,
			MaxRequestsInFlight: 4,
			EnableOpenMetrics:   true,
		}),
	}

	if opts.PprofEnabled {
		// Index handles /debug/pprof/ and dispatches the named profiles;
		// the four below are not reachable through it.
		h["GET "+PprofPrefix] = http.HandlerFunc(pprof.Index)
		h["GET "+PprofPrefix+"cmdline"] = http.HandlerFunc(pprof.Cmdline)
		h["GET "+PprofPrefix+"profile"] = http.HandlerFunc(pprof.Profile)
		h["GET "+PprofPrefix+"symbol"] = http.HandlerFunc(pprof.Symbol)
		h["GET "+PprofPrefix+"trace"] = http.HandlerFunc(pprof.Trace)
	}
	return h
}

// AdminPaths lists the paths the admin listener owns, without their methods.
// [ExceptAdminPaths] matches against it.
var AdminPaths = []string{HealthzPath, ReadyzPath, MetricsPath, PprofPrefix}

// ExceptAdminPaths is an [Options.InstrumentationFilter] that observes every
// request EXCEPT the admin paths: /healthz, /readyz, /metrics, and anything
// under /debug/pprof/.
//
// Pass it on the API listener for as long as the admin routes are mounted there
// during a migration. Without it, every kubelet probe and every Prometheus
// scrape lands in http_server_request_duration_seconds -- at a probe every 10s
// and a scrape every 30s that is a permanent floor of traffic that is not
// traffic, and it moves the moment the routes migrate to the admin port, which
// makes the before/after comparison worthless exactly when you need it.
func ExceptAdminPaths(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return true
	}
	p := r.URL.Path
	for _, admin := range AdminPaths {
		if p == admin || (strings.HasSuffix(admin, "/") && strings.HasPrefix(p, admin)) {
			return false
		}
	}
	return true
}

// Admin is the admin listener.
type Admin struct {
	// Server is the configured http.Server. lifecycle.Run starts and drains
	// it alongside the public one.
	Server *http.Server

	// Mux is the underlying router, for a service that must add an operational
	// endpoint of its own. Anything added here is admin-port-only, which is
	// the right default for anything operational.
	Mux *http.ServeMux

	handlers map[string]http.Handler
}

// Handlers returns the routes this admin listener serves, keyed by
// [http.ServeMux] pattern. It is the same map [AdminHandlers] built, so a
// service that already has an *Admin does not have to rebuild it (and does not
// get a second *Readiness or a second registry by accident).
//
// Routes added directly to Admin.Mux afterwards are NOT included: the map is
// what this package registered.
func (a *Admin) Handlers() map[string]http.Handler {
	out := make(map[string]http.Handler, len(a.handlers))
	for pattern, h := range a.handlers {
		out[pattern] = h
	}
	return out
}

// RegisterOn mounts this listener's routes on another mux -- in practice the
// API listener's, during a migration off a single port.
//
// The admin listener keeps serving them too. Both copies are backed by the same
// *Readiness and the same registry, so the two ports cannot disagree, which is
// the property that makes moving a probe from one to the other a no-op.
//
//	api := httpapi.New(httpapi.Options{
//	    Addr: ":8080",
//	    // Keep probes and scrapes out of the API listener's RED metrics
//	    // while they are answered here.
//	    InstrumentationFilter: httpapi.ExceptAdminPaths,
//	})
//	admin := httpapi.NewAdmin(httpapi.AdminOptions{Addr: ":9090", Readiness: ready, Registry: reg})
//	admin.RegisterOn(api.Mux)   // TRANSITIONAL: delete once the chart probes :9090
//
// It panics if a pattern is already registered on mux, which is [http.ServeMux]
// behaviour and is what you want: a service that already hand-rolled /healthz
// finds out at startup instead of serving two different answers depending on
// registration order.
//
// ONE HAZARD, worth knowing before the migration outlives its deadline:
// [Options.Middleware] is the reserved auth slot and it wraps EVERYTHING on
// the API listener, including anything mounted here. Add a bearer-token check
// there while the admin routes are still on that port and the kubelet's own
// probes start getting 401s -- the liveness probe fails, and the container is
// killed. The admin listener has no such slot, which is part of why the routes
// belong there. Either finish the migration before adding auth, or exempt
// [AdminPaths] in the middleware itself.
//
// READ THE PACKAGE DOC FIRST. Single-port mode is a migration affordance with a
// deadline, not a supported end state: /metrics, pprof and the OpenAPI document
// belong off any listener that could end up behind an Ingress.
func (a *Admin) RegisterOn(mux *http.ServeMux) {
	for pattern, h := range a.handlers {
		mux.Handle(pattern, h)
	}
}

// NewAdmin builds the admin listener.
//
// Route patterns use Go 1.22 method syntax, so a POST to /healthz is a 405
// from the mux rather than a silent 200.
func NewAdmin(opts AdminOptions) *Admin {
	addr := opts.Addr
	if addr == "" {
		addr = DefaultAdminAddr
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	handlers := AdminHandlers(opts)
	mux := http.NewServeMux()
	for pattern, h := range handlers {
		mux.Handle(pattern, h)
	}

	// No otelhttp here, deliberately. Instrumenting the admin listener would
	// make every Prometheus scrape emit RED series describing the scrape,
	// which is both self-referential and a permanent baseline of fake traffic
	// on every dashboard.
	return &Admin{
		Server:   newServer(addr, mux, logger, opts.Timeouts),
		Mux:      mux,
		handlers: handlers,
	}
}

// Handler returns the handler the admin server serves.
func (a *Admin) Handler() http.Handler { return a.Server.Handler }
