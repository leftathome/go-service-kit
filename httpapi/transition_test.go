package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/leftathome/go-service-kit/httpapi"
	"github.com/prometheus/client_golang/prometheus"
)

// --- single-port transition mode ---------------------------------------
//
// The destination is still two listeners. These tests pin the MIGRATION
// AFFORDANCE that lets a live service get there without a hard cutover: nagus
// runs replicas: 1 with strategy: Recreate and serves everything on :8080, so
// a release that moves the probes and the binary at once has a rollback path
// that is itself an outage.

func newTransitionFixture(t *testing.T) (*httpapi.API, *httpapi.Admin, *httpapi.Readiness) {
	t.Helper()

	ready := httpapi.NewReadiness()
	ready.Register("store", func(context.Context) error { return nil })

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "fixture_probe",
		Help: "Fixture metric so /metrics has something to serve.",
	}, func() float64 { return 1 }))

	api := httpapi.New(httpapi.Options{InstrumentationFilter: httpapi.ExceptAdminPaths})
	admin := httpapi.NewAdmin(httpapi.AdminOptions{Readiness: ready, Registry: reg})
	return api, admin, ready
}

func TestAdminHandlersAreMethodQualified(t *testing.T) {
	t.Parallel()

	handlers := httpapi.AdminHandlers(httpapi.AdminOptions{PprofEnabled: true})

	for _, want := range []string{
		"GET " + httpapi.HealthzPath,
		"GET " + httpapi.ReadyzPath,
		"GET " + httpapi.MetricsPath,
		"GET " + httpapi.PprofPrefix,
	} {
		if handlers[want] == nil {
			t.Errorf("AdminHandlers has no entry for %q; keys: %v", want, slices.Sorted(mapKeys(handlers)))
		}
	}

	// The keys are ServeMux patterns, and the method qualification is the
	// point: hand-rolling these during a migration is what loses it.
	mux := http.NewServeMux()
	for pattern, h := range handlers {
		mux.Handle(pattern, h)
	}
	if rec := do(t, mux, http.MethodGet, httpapi.HealthzPath); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz on a hand-built mux = %d, want 200", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, httpapi.HealthzPath); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405 (method-qualified patterns)", rec.Code)
	}
}

func TestAdminHandlersWithoutPprofOmitsIt(t *testing.T) {
	t.Parallel()

	handlers := httpapi.AdminHandlers(httpapi.AdminOptions{})
	for pattern := range handlers {
		if strings.Contains(pattern, httpapi.PprofPrefix) {
			t.Errorf("pprof is off but AdminHandlers returned %q", pattern)
		}
	}
	if len(handlers) != 3 {
		t.Errorf("AdminHandlers returned %d routes, want 3 (healthz, readyz, metrics)", len(handlers))
	}
}

// The whole point of the affordance: during the transition BOTH ports answer
// the admin paths, so moving a probe from one to the other is a no-op and each
// release is independently revertible.
func TestRegisterOnServesAdminPathsOnBothListeners(t *testing.T) {
	t.Parallel()

	api, admin, _ := newTransitionFixture(t)
	admin.RegisterOn(api.Mux)

	for _, path := range []string{httpapi.HealthzPath, httpapi.ReadyzPath, httpapi.MetricsPath} {
		if rec := do(t, api.Handler(), http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("API listener GET %s = %d, want 200 during the transition", path, rec.Code)
		}
		if rec := do(t, admin.Handler(), http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("admin listener GET %s = %d, want 200", path, rec.Code)
		}
	}

	if rec := do(t, api.Handler(), http.MethodGet, httpapi.MetricsPath); !strings.Contains(rec.Body.String(), "fixture_probe") {
		t.Errorf("API-port /metrics does not serve the supplied registry: %s", rec.Body.String())
	}
	if rec := do(t, api.Handler(), http.MethodPost, httpapi.HealthzPath); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz on the API listener = %d, want 405", rec.Code)
	}
}

// Both copies must be backed by the SAME readiness handle. If they could
// disagree, the release that moves the probe would change the answer, which is
// exactly the risk the transition is supposed to remove.
func TestRegisterOnSharesReadinessState(t *testing.T) {
	t.Parallel()

	api, admin, ready := newTransitionFixture(t)
	admin.RegisterOn(api.Mux)

	ready.SetShuttingDown()

	for name, h := range map[string]http.Handler{"api": api.Handler(), "admin": admin.Handler()} {
		rec := do(t, h, http.MethodGet, httpapi.ReadyzPath)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s GET /readyz while shutting down = %d, want 503", name, rec.Code)
		}
		var report httpapi.ReadinessReport
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("%s /readyz body: %v", name, err)
		}
		if report.Status != httpapi.StatusShuttingDown {
			t.Errorf("%s /readyz status = %q, want %q", name, report.Status, httpapi.StatusShuttingDown)
		}
	}
}

// Handlers() must not hand back a second Readiness or a second registry: a
// service that mounts Handlers() rather than RegisterOn must get the same
// state, not a parallel universe that always reports ready.
func TestAdminHandlersAccessorSharesState(t *testing.T) {
	t.Parallel()

	_, admin, ready := newTransitionFixture(t)
	ready.SetShuttingDown()

	h := admin.Handlers()["GET "+httpapi.ReadyzPath]
	if h == nil {
		t.Fatal("Admin.Handlers() has no /readyz entry")
	}
	if rec := do(t, h, http.MethodGet, httpapi.ReadyzPath); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Handlers()[/readyz] while shutting down = %d, want 503 (shared state)", rec.Code)
	}

	// The returned map is a copy: mutating it must not reconfigure the running
	// admin listener.
	m := admin.Handlers()
	delete(m, "GET "+httpapi.HealthzPath)
	if rec := do(t, admin.Handler(), http.MethodGet, httpapi.HealthzPath); rec.Code != http.StatusOK {
		t.Errorf("mutating the Handlers() copy changed the listener: GET /healthz = %d", rec.Code)
	}
}

// A pattern already on the target mux is a wiring bug -- typically a service
// that hand-rolled /healthz and forgot. ServeMux panics; that is the right
// answer, because the alternative is two different bodies depending on
// registration order.
func TestRegisterOnPanicsOnADuplicatePattern(t *testing.T) {
	t.Parallel()

	api, admin, _ := newTransitionFixture(t)
	api.Mux.HandleFunc("GET "+httpapi.HealthzPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	defer func() {
		if recover() == nil {
			t.Error("RegisterOn over an existing /healthz did not panic")
		}
	}()
	admin.RegisterOn(api.Mux)
}

func TestExceptAdminPathsClassifies(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{ // path -> should be instrumented
		"/healthz":                 false,
		"/readyz":                  false,
		"/metrics":                 false,
		"/debug/pprof/":            false,
		"/debug/pprof/heap":        false,
		"/widgets":                 true,
		"/mcp":                     true,
		"/":                        true,
		"/healthzzz":               true,
		"/api/healthz":             true,
		"/debug/pprofile":          true,
		"/metrics/custom/thing":    true,
		"/readyz/../secret":        true,
		"/watches?since=yesterday": true,
	}
	for target, want := range cases {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if got := httpapi.ExceptAdminPaths(req); got != want {
			t.Errorf("ExceptAdminPaths(%q) = %v, want %v", target, got, want)
		}
	}
}

// With the admin routes on the API listener, a kubelet probe every 10s and a
// scrape every 30s would otherwise become a permanent floor in the RED series
// -- and it would vanish the moment the routes move, ruining the comparison.
func TestAdminPathsOnTheApiListenerAreNotCountedAsTraffic(t *testing.T) {
	t.Parallel()

	api, reg := newMetricsAPIWithOptions(t, httpapi.Options{
		InstrumentationFilter: httpapi.ExceptAdminPaths,
	})
	admin := httpapi.NewAdmin(httpapi.AdminOptions{})
	admin.RegisterOn(api.Mux)

	h := api.Handler()
	for range 3 {
		do(t, h, http.MethodGet, "/healthz")
		do(t, h, http.MethodGet, "/readyz")
	}
	do(t, h, http.MethodGet, "/things/abc")

	fam := gather(t, reg, httpapi.RequestDurationMetricName)
	for _, m := range fam.GetMetric() {
		route := labelsOf(m)["http_route"]
		if route == "/healthz" || route == "/readyz" || route == "/metrics" {
			t.Errorf("admin path %s produced RED series despite ExceptAdminPaths", route)
		}
	}

	var sawThings bool
	for _, m := range fam.GetMetric() {
		if labelsOf(m)["http_route"] == "/things/{id}" {
			sawThings = true
		}
	}
	if !sawThings {
		t.Error("the filter suppressed real API traffic too; /things/{id} has no RED series")
	}
}

// --- raw routes ---------------------------------------------------------

func TestRawRouteIsServedAndListed(t *testing.T) {
	t.Parallel()

	api := httpapi.New(httpapi.Options{})
	api.RawRouteFunc("POST /mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A JSON-RPC error object, NOT problem+json. This is the documented
		// exception to the one-error-format-per-listener rule.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	})

	rec := do(t, api.Handler(), http.MethodPost, "/mcp")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /mcp = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"jsonrpc"`) {
		t.Errorf("POST /mcp body = %q, want the raw handler's JSON-RPC body", rec.Body.String())
	}

	// Method qualification is enforced, so a GET from a crawler is a 405 from
	// the mux rather than a JSON-RPC handler reading an empty body.
	if rec := do(t, api.Handler(), http.MethodGet, "/mcp"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405", rec.Code)
	}

	if got := api.RawRoutes(); !slices.Equal(got, []string{"POST /mcp"}) {
		t.Errorf("RawRoutes() = %v, want [POST /mcp]", got)
	}
}

func TestRawRoutesIsSortedAndCopied(t *testing.T) {
	t.Parallel()

	api := httpapi.New(httpapi.Options{})
	nop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	api.RawRoute("POST /mcp", nop)
	api.RawRoute("GET /legacy", nop)

	want := []string{"GET /legacy", "POST /mcp"}
	got := api.RawRoutes()
	if !slices.Equal(got, want) {
		t.Fatalf("RawRoutes() = %v, want %v", got, want)
	}

	got[0] = "mutated"
	if again := api.RawRoutes(); !slices.Equal(again, want) {
		t.Errorf("RawRoutes() returned an aliased slice: %v", again)
	}
}

func TestRawRouteRejectsAmbientPatterns(t *testing.T) {
	t.Parallel()

	nop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, pattern := range []string{"/mcp", "", "/", "   "} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RawRoute(%q) did not panic; an unqualified pattern answers every method", pattern)
				}
			}()
			httpapi.New(httpapi.Options{}).RawRoute(pattern, nop)
		}()
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("RawRoute with a nil handler did not panic")
			}
		}()
		httpapi.New(httpapi.Options{}).RawRoute("POST /mcp", nil)
	}()
}

// A raw route is invisible to the OpenAPI document. That is the cost of the
// pattern and it is asserted rather than merely documented, so nobody expects
// /mcp to show up in the spec.
func TestRawRouteIsAbsentFromTheOpenAPIDocument(t *testing.T) {
	t.Parallel()

	api := httpapi.New(httpapi.Options{DocsEnabled: true, Title: "raw", Version: "1.0.0"})
	api.RawRouteFunc("POST /mcp", func(http.ResponseWriter, *http.Request) {})

	rec := do(t, api.Handler(), http.MethodGet, "/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/mcp") {
		t.Error("the OpenAPI document mentions a raw route; RawRoute must not register with huma")
	}
}

// A raw route still gets RED metrics with a BOUNDED http_route label, which is
// most of why it belongs on API.Mux rather than on a third listener.
func TestRawRouteIsInstrumented(t *testing.T) {
	t.Parallel()

	api, reg := newMetricsAPI(t)
	api.RawRouteFunc("POST /mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader("{}")))

	fam := gather(t, reg, httpapi.RequestDurationMetricName)
	for _, m := range fam.GetMetric() {
		if labelsOf(m)["http_route"] == "/mcp" {
			return
		}
	}
	t.Error("no RED series with http_route=/mcp; a raw route must still be instrumented")
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
