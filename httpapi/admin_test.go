package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/leftathome/go-service-kit/httpapi"
	"github.com/prometheus/client_golang/prometheus"
)

func newFixtureAdmin(t *testing.T, pprofEnabled bool) (*httpapi.Admin, *httpapi.Readiness) {
	t.Helper()

	ready := httpapi.NewReadiness()
	ready.Register("store", func(context.Context) error { return nil })

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "fixture_probe",
		Help: "Fixture metric so /metrics has something to serve.",
	}, func() float64 { return 1 }))

	admin := httpapi.NewAdmin(httpapi.AdminOptions{
		Readiness:    ready,
		Registry:     reg,
		PprofEnabled: pprofEnabled,
	})
	return admin, ready
}

// /healthz is LIVENESS and /readyz is READINESS. They are different questions
// and they must not be aliases: a failing dependency must take the pod out of
// the Service endpoints (readiness) without the kubelet killing and restarting
// it (liveness). Restarting a process because Postgres is down turns one
// outage into a CrashLoopBackOff.
func TestHealthzIsStaticAndIndependentOfReadiness(t *testing.T) {
	t.Parallel()

	admin, ready := newFixtureAdmin(t, false)

	if rec := do(t, admin.Handler(), http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}

	// Break every dependency and start shutting down: liveness stays 200.
	ready.Register("broken", func(context.Context) error { return context.DeadlineExceeded })
	ready.SetShuttingDown()

	if rec := do(t, admin.Handler(), http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz with failing deps = %d, want 200 (liveness is not readiness)", rec.Code)
	}
	if rec := do(t, admin.Handler(), http.MethodGet, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with failing deps = %d, want 503", rec.Code)
	}
}

func TestAdminListenerServesItsFourSurfaces(t *testing.T) {
	t.Parallel()

	admin, _ := newFixtureAdmin(t, true)
	h := admin.Handler()

	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/debug/pprof/", "/debug/pprof/cmdline"} {
		if rec := do(t, h, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("admin GET %s = %d, want 200", path, rec.Code)
		}
	}

	if rec := do(t, h, http.MethodGet, "/metrics"); !strings.Contains(rec.Body.String(), "fixture_probe") {
		t.Errorf("/metrics does not serve the supplied registry: %s", rec.Body.String())
	}
}

// The admin listener is where the process internals live. It must never carry
// the API or the machine-readable spec, because the whole point of the split
// is that only one of the two ports is ever a candidate for an Ingress.
func TestAdminListenerDoesNotServeApiOrDocs(t *testing.T) {
	t.Parallel()

	admin, _ := newFixtureAdmin(t, true)
	for _, path := range []string{"/widgets", "/widgets/w-1", "/openapi.json", "/openapi.yaml", "/docs", "/"} {
		if rec := do(t, admin.Handler(), http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("admin GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestPprofDisabledByOption(t *testing.T) {
	t.Parallel()

	admin, _ := newFixtureAdmin(t, false)
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/profile"} {
		if rec := do(t, admin.Handler(), http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("with pprof disabled, GET %s = %d, want 404", path, rec.Code)
		}
	}
	// The rest of the admin surface is unaffected.
	if rec := do(t, admin.Handler(), http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

// An admin listener built with no registry and no readiness handle must still
// come up: the template wires those in, but a partial wiring must degrade to a
// clear answer rather than a nil dereference on the first scrape.
func TestAdminZeroOptionsIsUsable(t *testing.T) {
	t.Parallel()

	h := httpapi.NewAdmin(httpapi.AdminOptions{}).Handler()

	if rec := do(t, h, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("GET /readyz with no checks = %d, want 200", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/metrics"); rec.Code != http.StatusOK {
		t.Errorf("GET /metrics with no registry = %d, want 200", rec.Code)
	}
}

// The admin listener is not instrumented: scraping it must not create RED
// series, or Prometheus scraping itself becomes a permanent source of traffic
// metrics and self-referential dashboards.
func TestAdminListenerHasNoRequestInstrumentation(t *testing.T) {
	t.Parallel()

	admin, _ := newFixtureAdmin(t, false)
	h := admin.Handler()
	for range 3 {
		do(t, h, http.MethodGet, "/healthz")
	}
	rec := do(t, h, http.MethodGet, "/metrics")
	if strings.Contains(rec.Body.String(), "http_server_request_duration") {
		t.Errorf("admin listener recorded RED metrics for its own traffic:\n%s", rec.Body.String())
	}
}
