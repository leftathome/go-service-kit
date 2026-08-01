package httpapi_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/leftathome/go-service-kit/httpapi"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// newMetricsAPI builds a public listener whose RED metrics land in a private
// registry. The translation strategy MUST match the one obs.Setup pins
// (UnderscoreEscapingWithSuffixes), or the names asserted here are not the
// names a deployed service exports. This is deliberately reconstructed rather
// than imported from obs: the kit's packages have no edges between siblings.
func newMetricsAPI(t *testing.T) (*httpapi.API, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	)
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	api := httpapi.New(httpapi.Options{MeterProvider: mp})
	huma.Register(api.Huma, huma.Operation{
		OperationID: "get-thing",
		Method:      http.MethodGet,
		Path:        "/things/{id}",
	}, func(context.Context, *getWidgetInput) (*getWidgetOutput, error) {
		return &getWidgetOutput{Body: fixtureWidget{ID: "t"}}, nil
	})
	return api, reg
}

func gather(t *testing.T, reg *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	got := make([]string, 0, len(families))
	for _, f := range families {
		got = append(got, f.GetName())
	}
	sort.Strings(got)
	t.Fatalf("metric family %q not exported; have %v", name, got)
	return nil
}

func labelsOf(m *dto.Metric) map[string]string {
	out := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

// THE cardinality assertion. otelhttp derives http.route from the ServeMux
// pattern, so the label must be the TEMPLATE. If it were the raw URL, one
// crawler walking /things/<uuid> would mint a new time series per request and
// take Prometheus down with the service still nominally healthy.
func TestREDMetricsCarryTheRouteTemplateNotTheRawURL(t *testing.T) {
	t.Parallel()

	api, reg := newMetricsAPI(t)
	h := api.Handler()

	rawIDs := []string{"0f2a1c", "deadbeef", "b6b6b6", "another-crawler-path"}
	for _, id := range rawIDs {
		if rec := do(t, h, http.MethodGet, "/things/"+id); rec.Code != http.StatusOK {
			t.Fatalf("GET /things/%s = %d", id, rec.Code)
		}
	}

	fam := gather(t, reg, httpapi.RequestDurationMetricName)

	if n := len(fam.GetMetric()); n != 1 {
		var seen []string
		for _, m := range fam.GetMetric() {
			seen = append(seen, labelsOf(m)["http_route"])
		}
		t.Fatalf("%d series for %d requests to one route (routes seen: %v); the route label is not a template",
			n, len(rawIDs), seen)
	}

	m := fam.GetMetric()[0]
	labels := labelsOf(m)

	if got := labels["http_route"]; got != "/things/{id}" {
		t.Errorf("http_route = %q, want %q", got, "/things/{id}")
	}
	if got := m.GetHistogram().GetSampleCount(); got != uint64(len(rawIDs)) {
		t.Errorf("sample count = %d, want %d", got, len(rawIDs))
	}
	for name, value := range labels {
		for _, id := range rawIDs {
			if strings.Contains(value, id) {
				t.Errorf("label %s=%q contains the raw path segment %q", name, value, id)
			}
		}
	}
}

// The route label survives request-cloning middleware. otelhttp reads
// r.Pattern off the pointer it passed downstream, so it must sit adjacent to
// the mux; New keeps user middleware outside it for exactly this reason. If
// someone reorders the chain, this is the test that notices, because the
// symptom in production -- http_route quietly going absent -- looks like
// nothing at all until the series count is already a problem.
func TestRouteLabelSurvivesCloningMiddleware(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	)
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	type ctxKey struct{}
	api := httpapi.New(httpapi.Options{
		MeterProvider: mp,
		Middleware: []func(http.Handler) http.Handler{
			// The single most common middleware shape, and the one that
			// breaks route labels if otelhttp is nested inside it.
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, "v")))
				})
			},
		},
	})
	huma.Register(api.Huma, huma.Operation{
		OperationID: "get-thing",
		Method:      http.MethodGet,
		Path:        "/things/{id}",
	}, func(context.Context, *getWidgetInput) (*getWidgetOutput, error) {
		return &getWidgetOutput{Body: fixtureWidget{ID: "t"}}, nil
	})

	for _, id := range []string{"aaa", "bbb", "ccc"} {
		if rec := do(t, api.Handler(), http.MethodGet, "/things/"+id); rec.Code != http.StatusOK {
			t.Fatalf("GET /things/%s = %d", id, rec.Code)
		}
	}

	fam := gather(t, reg, httpapi.RequestDurationMetricName)
	if n := len(fam.GetMetric()); n != 1 {
		t.Fatalf("%d series behind cloning middleware, want 1", n)
	}
	if got := labelsOf(fam.GetMetric()[0])["http_route"]; got != "/things/{id}" {
		t.Errorf("http_route = %q behind cloning middleware, want %q", got, "/things/{id}")
	}
}

// The label SET is as load-bearing as the route value: every extra label
// multiplies the series count. Pin it so a dependency bump that starts
// emitting, say, user_agent_original shows up here.
func TestREDMetricLabelSetIsBounded(t *testing.T) {
	t.Parallel()

	api, reg := newMetricsAPI(t)
	req := "/things/abc"
	if rec := do(t, api.Handler(), http.MethodGet, req); rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", req, rec.Code)
	}

	fam := gather(t, reg, httpapi.RequestDurationMetricName)
	got := make([]string, 0, 8)
	for name := range labelsOf(fam.GetMetric()[0]) {
		got = append(got, name)
	}
	sort.Strings(got)

	want := []string{
		"http_request_method",
		"http_response_status_code",
		"http_route",
		"network_protocol_name",
		"network_protocol_version",
		"otel_scope_name",
		"otel_scope_schema_url",
		"otel_scope_version",
		"server_address",
		"url_scheme",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("RED label set changed.\n got: %v\nwant: %v", got, want)
	}

	// Unbounded-cardinality labels that must never appear.
	for _, forbidden := range []string{"url_path", "url_full", "user_agent_original", "client_address", "network_peer_address", "network_peer_port"} {
		if _, ok := labelsOf(fam.GetMetric()[0])[forbidden]; ok {
			t.Errorf("high-cardinality label %q is on a RED metric", forbidden)
		}
	}
}

// Record the exact family names so the Grafana dashboard is written against
// reality. These are OTel-namespaced, translated by the Prometheus exporter --
// they are NOT promhttp's http_requests_total / http_request_duration_seconds.
func TestREDMetricFamilyNames(t *testing.T) {
	t.Parallel()

	api, reg := newMetricsAPI(t)
	if rec := do(t, api.Handler(), http.MethodGet, "/things/abc"); rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	for _, name := range httpapi.REDMetricNames {
		gather(t, reg, name)
	}
}

// A request that matches no route still gets recorded, and it must NOT carry a
// route label -- an unrouted path is exactly where a crawler's raw URL would
// leak in if otelhttp ever changed its fallback.
func TestUnroutedRequestsHaveNoRouteLabel(t *testing.T) {
	t.Parallel()

	api, reg := newMetricsAPI(t)
	h := api.Handler()
	for _, p := range []string{"/wp-login.php", "/.env", "/admin/config.php"} {
		if rec := do(t, h, http.MethodGet, p); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, rec.Code)
		}
	}

	fam := gather(t, reg, httpapi.RequestDurationMetricName)
	if n := len(fam.GetMetric()); n != 1 {
		t.Fatalf("%d series for 3 unrouted requests, want 1", n)
	}
	labels := labelsOf(fam.GetMetric()[0])
	if v, ok := labels["http_route"]; ok {
		t.Errorf("unrouted request carries http_route = %q", v)
	}
	if labels["http_response_status_code"] != "404" {
		t.Errorf("status label = %q, want 404", labels["http_response_status_code"])
	}
}
