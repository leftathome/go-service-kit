package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/leftathome/go-service-kit/httpapi"
)

// What huma v2.39.0 ACTUALLY emits, asserted rather than assumed. The package
// documentation is written against these assertions; if a huma bump changes
// the content type or the member names, this test is what tells us before a
// client does.
func TestHumaErrorsAreRFC9457ProblemJSON(t *testing.T) {
	t.Parallel()

	h := newFixtureAPI(t, false).Handler()
	rec := do(t, h, http.MethodGet, "/widgets/does-not-exist")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != httpapi.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, httpapi.ProblemContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	for k, want := range map[string]any{
		"title":  "Not Found",
		"status": float64(404),
		"detail": "no widget with that id",
	} {
		if body[k] != want {
			t.Errorf("body[%q] = %#v, want %#v", k, body[k], want)
		}
	}
	// huma omits `type` when it is empty rather than emitting the RFC's
	// "about:blank" default. Recorded here because API-STYLE.md says so.
	if _, present := body["type"]; present {
		t.Errorf("huma now emits `type` by default; update the package doc: %v", body)
	}
}

// Validation failures are the common case and they carry per-field detail in
// an `errors` array. That array is a huma extension to RFC 9457, which the
// RFC explicitly permits.
func TestValidationErrorsCarryPerFieldDetail(t *testing.T) {
	t.Parallel()

	h := newFixtureAPI(t, false).Handler()
	rec := do(t, h, http.MethodGet, "/widgets?limit=99999")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != httpapi.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, httpapi.ProblemContentType)
	}

	var body struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Errors []struct {
			Message  string `json:"message"`
			Location string `json:"location"`
			Value    any    `json:"value"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("no per-field errors: %s", rec.Body.String())
	}
	if body.Errors[0].Location != "query.limit" {
		t.Errorf("errors[0].location = %q, want %q", body.Errors[0].Location, "query.limit")
	}
}

func TestProblemSetsStatusTitleAndDetail(t *testing.T) {
	t.Parallel()

	err := httpapi.Problem(http.StatusConflict, "widget already exists")
	if err.GetStatus() != http.StatusConflict {
		t.Errorf("status = %d, want 409", err.GetStatus())
	}
	if err.Error() != "widget already exists" {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestProblemWithOptionsCarriesTypeTitleAndInstance(t *testing.T) {
	t.Parallel()

	api := httpapi.New(httpapi.Options{})
	huma.Register(api.Huma, huma.Operation{
		OperationID: "boom",
		Method:      http.MethodGet,
		Path:        "/boom",
	}, func(context.Context, *struct{}) (*struct{}, error) {
		return nil, httpapi.ProblemWithOptions(http.StatusTeapot, "short and stout", httpapi.ProblemOptions{
			Type:     "https://example.test/errors/teapot",
			Title:    "I Am A Teapot",
			Instance: "/boom#42",
		}, httpapi.Detail("wrong vessel", "body.vessel", "kettle"))
	})

	rec := do(t, api.Handler(), http.MethodGet, "/boom")
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != httpapi.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, httpapi.ProblemContentType)
	}

	var body struct {
		Type     string `json:"type"`
		Title    string `json:"title"`
		Status   int    `json:"status"`
		Detail   string `json:"detail"`
		Instance string `json:"instance"`
		Errors   []struct {
			Message  string `json:"message"`
			Location string `json:"location"`
			Value    any    `json:"value"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if body.Type != "https://example.test/errors/teapot" {
		t.Errorf("type = %q", body.Type)
	}
	if body.Title != "I Am A Teapot" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Instance != "/boom#42" {
		t.Errorf("instance = %q", body.Instance)
	}
	if len(body.Errors) != 1 || body.Errors[0].Location != "body.vessel" || body.Errors[0].Value != "kettle" {
		t.Errorf("errors = %+v", body.Errors)
	}
}

// Middleware, and anything else that rejects a request before huma sees it,
// must produce the same wire shape. Two error formats on one listener is how
// clients end up with two error parsers.
func TestWriteProblemMatchesTheHumaWireShape(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	if err := httpapi.WriteProblem(rec, http.StatusTooManyRequests, "slow down", httpapi.ProblemOptions{
		Type: "https://example.test/errors/rate-limit",
	}); err != nil {
		t.Fatalf("WriteProblem: %v", err)
	}

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != httpapi.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, httpapi.ProblemContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	for k, want := range map[string]any{
		"type":   "https://example.test/errors/rate-limit",
		"title":  "Too Many Requests",
		"status": float64(429),
		"detail": "slow down",
	} {
		if body[k] != want {
			t.Errorf("body[%q] = %#v, want %#v", k, body[k], want)
		}
	}
}

// A 404 from the mux itself (no route) is stdlib plain text, not problem+json.
// Recorded deliberately: the alternative is a catch-all route on the public
// mux, which would swallow genuine routing mistakes.
func TestUnroutedPathsUseTheStdlibNotFound(t *testing.T) {
	t.Parallel()

	rec := do(t, newFixtureAPI(t, false).Handler(), http.MethodGet, "/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == httpapi.ProblemContentType {
		t.Errorf("unrouted 404 unexpectedly uses problem+json; update the package doc")
	}
}
