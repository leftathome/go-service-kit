package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/pprof"

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

	mux := http.NewServeMux()

	// LIVENESS. Static 200 for as long as the process can accept a connection
	// and run a handler. It deliberately consults nothing: see the Readiness
	// doc for why a dependency check here turns an outage into a
	// CrashLoopBackOff.
	mux.Handle("GET "+HealthzPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}))

	// READINESS. Dependency checks plus the shutting-down gate that
	// lifecycle.Run flips on SIGTERM.
	mux.Handle("GET "+ReadyzPath, ready.Handler())

	mux.Handle("GET "+MetricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:            slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		ErrorHandling:       promhttp.HTTPErrorOnError,
		MaxRequestsInFlight: 4,
		EnableOpenMetrics:   true,
	}))

	if opts.PprofEnabled {
		// Index handles /debug/pprof/ and dispatches the named profiles;
		// the four below are not reachable through it.
		mux.HandleFunc("GET "+PprofPrefix, pprof.Index)
		mux.HandleFunc("GET "+PprofPrefix+"cmdline", pprof.Cmdline)
		mux.HandleFunc("GET "+PprofPrefix+"profile", pprof.Profile)
		mux.HandleFunc("GET "+PprofPrefix+"symbol", pprof.Symbol)
		mux.HandleFunc("GET "+PprofPrefix+"trace", pprof.Trace)
	}

	// No otelhttp here, deliberately. Instrumenting the admin listener would
	// make every Prometheus scrape emit RED series describing the scrape,
	// which is both self-referential and a permanent baseline of fake traffic
	// on every dashboard.
	return &Admin{
		Server: newServer(addr, mux, logger),
		Mux:    mux,
	}
}

// Handler returns the handler the admin server serves.
func (a *Admin) Handler() http.Handler { return a.Server.Handler }
