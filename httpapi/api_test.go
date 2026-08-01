package httpapi_test

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/leftathome/go-service-kit/httpapi"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// --- fixture API -------------------------------------------------------
//
// The kit ships no operations of its own, so the golden OpenAPI test needs a
// representative surface: a paginated list, a path parameter, and an error
// response. These types deliberately look like a service's real handlers.

type fixtureWidget struct {
	ID   string `json:"id" doc:"Opaque widget identifier"`
	Name string `json:"name" doc:"Human-readable label"`
}

type listWidgetsInput struct {
	httpapi.PageParams
}

type getWidgetInput struct {
	ID string `path:"id" doc:"Widget identifier" maxLength:"64"`
}

type getWidgetOutput struct {
	Body fixtureWidget
}

func registerFixtureRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-widgets",
		Method:      http.MethodGet,
		Path:        "/widgets",
		Summary:     "List widgets",
	}, func(_ context.Context, in *listWidgetsInput) (*httpapi.PageResponse[fixtureWidget], error) {
		items := []fixtureWidget{{ID: "w-1", Name: "first"}}
		return &httpapi.PageResponse[fixtureWidget]{
			Body: httpapi.NewPage(items[:min(len(items), in.EffectiveLimit())], "next-token"),
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-widget",
		Method:      http.MethodGet,
		Path:        "/widgets/{id}",
		Summary:     "Get one widget",
	}, func(_ context.Context, in *getWidgetInput) (*getWidgetOutput, error) {
		if in.ID != "w-1" {
			return nil, httpapi.Problem(http.StatusNotFound, "no widget with that id")
		}
		return &getWidgetOutput{Body: fixtureWidget{ID: "w-1", Name: "first"}}, nil
	})
}

func newFixtureAPI(t *testing.T, docsEnabled bool) *httpapi.API {
	t.Helper()
	api := httpapi.New(httpapi.Options{
		Title:       "Fixture API",
		Version:     "1.2.3",
		Description: "Fixture surface used by the kit's own golden test.",
		DocsEnabled: docsEnabled,
	})
	registerFixtureRoutes(api.Huma)
	return api
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	return rec
}

// --- server hardening --------------------------------------------------

// gosec G112 and its neighbours. Every timeout must be non-zero on both
// listeners, and there must be no public path that yields a server without
// them: Options carries no timeout fields at all.
func TestServersAlwaysHaveTimeouts(t *testing.T) {
	t.Parallel()

	servers := map[string]*http.Server{
		"public": httpapi.New(httpapi.Options{}).Server,
		"admin":  httpapi.NewAdmin(httpapi.AdminOptions{}).Server,
	}
	for name, srv := range servers {
		if srv.ReadHeaderTimeout <= 0 {
			t.Errorf("%s: ReadHeaderTimeout = %v, want > 0 (gosec G112)", name, srv.ReadHeaderTimeout)
		}
		if srv.ReadTimeout <= 0 {
			t.Errorf("%s: ReadTimeout = %v, want > 0", name, srv.ReadTimeout)
		}
		if srv.WriteTimeout <= 0 {
			t.Errorf("%s: WriteTimeout = %v, want > 0", name, srv.WriteTimeout)
		}
		if srv.IdleTimeout <= 0 {
			t.Errorf("%s: IdleTimeout = %v, want > 0", name, srv.IdleTimeout)
		}
		if srv.MaxHeaderBytes <= 0 {
			t.Errorf("%s: MaxHeaderBytes = %d, want > 0", name, srv.MaxHeaderBytes)
		}
	}
}

// The hardening above is only durable if a caller cannot opt out of it. Assert
// the shape of the Options structs, not just the constructed value.
func TestOptionsExposeNoTimeoutKnobs(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeOf(httpapi.Options{}),
		reflect.TypeOf(httpapi.AdminOptions{}),
	} {
		for i := range typ.NumField() {
			name := typ.Field(i).Name
			if strings.Contains(strings.ToLower(name), "timeout") ||
				strings.Contains(strings.ToLower(name), "maxheader") {
				t.Errorf("%s.%s: server timeouts are not configurable by design", typ.Name(), name)
			}
		}
	}
}

func TestDefaultAddresses(t *testing.T) {
	t.Parallel()

	if got := httpapi.New(httpapi.Options{}).Server.Addr; got != httpapi.DefaultAddr {
		t.Errorf("public addr = %q, want %q", got, httpapi.DefaultAddr)
	}
	if got := httpapi.NewAdmin(httpapi.AdminOptions{}).Server.Addr; got != httpapi.DefaultAdminAddr {
		t.Errorf("admin addr = %q, want %q", got, httpapi.DefaultAdminAddr)
	}
	if got := httpapi.New(httpapi.Options{Addr: "127.0.0.1:1"}).Server.Addr; got != "127.0.0.1:1" {
		t.Errorf("public addr = %q, want override", got)
	}
}

// --- the listener split ------------------------------------------------

// The public listener carries the API and, when enabled, the documentation.
// It must NOT carry health, metrics, or pprof: those describe the process,
// and the public listener is the one that may one day sit behind an Ingress.
func TestPublicListenerDoesNotServeAdminPaths(t *testing.T) {
	t.Parallel()

	api := newFixtureAPI(t, true)
	for _, path := range []string{
		"/healthz",
		"/readyz",
		"/metrics",
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/cmdline",
	} {
		if rec := do(t, api.Handler(), http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("public GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestPublicListenerServesTheAPI(t *testing.T) {
	t.Parallel()

	api := newFixtureAPI(t, true)
	rec := do(t, api.Handler(), http.MethodGet, "/widgets")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /widgets = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var page httpapi.Page[fixtureWidget]
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(page.Items) != 1 || page.Items[0].ID != "w-1" {
		t.Errorf("items = %+v", page.Items)
	}
}

// --- documentation surface ---------------------------------------------

func TestDocsEnabledServesSpecAndUI(t *testing.T) {
	t.Parallel()

	h := newFixtureAPI(t, true).Handler()

	for path, wantCT := range map[string]string{
		"/openapi.json": "application/openapi+json",
		"/openapi.yaml": "application/openapi+yaml",
		"/docs":         "text/html",
	} {
		rec := do(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, wantCT) {
			t.Errorf("GET %s content-type = %q, want prefix %q", path, ct, wantCT)
		}
	}
}

func TestDocsDisabledRemovesSpecAndUI(t *testing.T) {
	t.Parallel()

	h := newFixtureAPI(t, false).Handler()
	for _, path := range []string{"/openapi.json", "/openapi.yaml", "/openapi-3.0.json", "/docs", "/schemas/Widget.json"} {
		if rec := do(t, h, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("with docs disabled, GET %s = %d, want 404", path, rec.Code)
		}
	}
	// The API itself is unaffected.
	if rec := do(t, h, http.MethodGet, "/widgets"); rec.Code != http.StatusOK {
		t.Errorf("GET /widgets = %d, want 200 with docs disabled", rec.Code)
	}
}

// Air-gapped clusters and strict CSP: every byte the docs page needs must come
// from this process. A single CDN href turns /docs into a blank screen and an
// egress attempt.
func TestDocsAssetsAreSelfHostedWithNoExternalReferences(t *testing.T) {
	t.Parallel()

	h := newFixtureAPI(t, true).Handler()

	page := do(t, h, http.MethodGet, "/docs")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /docs = %d", page.Code)
	}
	body := page.Body.String()

	for _, forbidden := range []string{
		"http://", "https://", "//unpkg.com", "cdn.jsdelivr.net", "cdnjs", "integrity=",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/docs references %q; it must be fully self-hosted:\n%s", forbidden, body)
		}
	}

	// Everything it does reference must resolve on this same listener.
	for _, asset := range httpapi.DocsAssetPaths {
		rec := do(t, h, http.MethodGet, asset)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", asset, rec.Code)
		}
		if !strings.Contains(body, asset) {
			t.Errorf("/docs does not reference its own asset %s", asset)
		}
		if strings.Contains(rec.Body.String(), "https://") {
			t.Errorf("asset %s contains an external URL", asset)
		}
	}

	csp := page.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("/docs has no Content-Security-Policy header")
	}
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q missing %q", csp, want)
		}
	}
}

// --- golden OpenAPI ----------------------------------------------------

// The served spec is the API's contract. Committing it means a change to a
// handler signature shows up as a diff in review instead of as a surprise in a
// consumer. Regenerate with:
//
//	go test ./httpapi/ -run TestOpenAPIGolden -update
func TestOpenAPIGolden(t *testing.T) {
	t.Parallel()

	rec := do(t, newFixtureAPI(t, true).Handler(), http.MethodGet, "/openapi.yaml")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml = %d", rec.Code)
	}
	got := rec.Body.Bytes()

	golden := filepath.Join("testdata", "openapi.golden.yaml")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", golden)
		return
	}

	want, err := os.ReadFile(filepath.Clean(golden))
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("served OpenAPI differs from %s.\nRegenerate with: go test ./httpapi/ -run TestOpenAPIGolden -update\n--- got ---\n%s", golden, got)
	}
}

// The spec is generated from Go types, so it cannot drift from the code. Pin
// the pieces the house style depends on so a huma bump that changes them is
// caught here rather than by a client.
func TestOpenAPIReflectsHouseConventions(t *testing.T) {
	t.Parallel()

	rec := do(t, newFixtureAPI(t, true).Handler(), http.MethodGet, "/openapi.json")
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", v)
	}
	paths, _ := doc["paths"].(map[string]any)
	for _, want := range []string{"/widgets", "/widgets/{id}"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("spec is missing path %s (have %v)", want, keysOf(paths))
		}
	}
	// The spec endpoints themselves are not operations in the document.
	for _, unwanted := range []string{"/openapi.json", "/docs", "/healthz"} {
		if _, ok := paths[unwanted]; ok {
			t.Errorf("spec should not document %s", unwanted)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- middleware slot ---------------------------------------------------

// The reserved auth slot: adding a bearer check later must be a wiring change,
// not a redesign. Middleware wraps the whole public listener, including the
// docs and spec endpoints.
func TestMiddlewareWrapsThePublicListener(t *testing.T) {
	t.Parallel()

	api := httpapi.New(httpapi.Options{
		DocsEnabled: true,
		Middleware: []func(http.Handler) http.Handler{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Token") != "let-me-in" {
						_ = httpapi.WriteProblem(w, http.StatusUnauthorized, "missing token", httpapi.ProblemOptions{})
						return
					}
					next.ServeHTTP(w, r)
				})
			},
		},
	})
	registerFixtureRoutes(api.Huma)

	rec := do(t, api.Handler(), http.MethodGet, "/widgets")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /widgets = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != httpapi.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, httpapi.ProblemContentType)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/widgets", nil)
	req.Header.Set("X-Token", "let-me-in")
	authed := httptest.NewRecorder()
	api.Handler().ServeHTTP(authed, req)
	if authed.Code != http.StatusOK {
		t.Errorf("authenticated GET /widgets = %d, want 200", authed.Code)
	}
}
