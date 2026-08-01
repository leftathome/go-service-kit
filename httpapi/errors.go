package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// ProblemContentType is the media type every error on the public listener
// carries. VERIFIED against huma v2.39.0 rather than assumed -- see
// TestHumaErrorsAreRFC9457ProblemJSON, which is the regression gate if a huma
// bump changes it.
const ProblemContentType = "application/problem+json"

// What huma v2.39.0 ACTUALLY emits, recorded here because docs/API-STYLE.md is
// written against it:
//
//   - Content-Type is exactly "application/problem+json" (no charset
//     parameter). huma's *huma.ErrorModel implements ContentType(string)
//     string, which rewrites "application/json" to the problem type after the
//     JSON marshaller has run.
//
//   - The body is RFC 9457 Problem Details, every member omitempty:
//
//     {"title":"Not Found","status":404,"detail":"no widget with that id"}
//
//     Members: type, title, status, detail, instance, errors.
//
//   - `type` is ABSENT by default. The struct tag declares
//     default:"about:blank" so the OPENAPI SCHEMA advertises that default, but
//     the Go zero value is "" and the field is omitempty, so nothing is
//     written. RFC 9457 says a consumer must treat an absent type as
//     "about:blank", which is the same meaning -- but a client asserting the
//     member is present will break. Use [ProblemWithOptions] to set it.
//
//   - `title` defaults to http.StatusText(status) and `status` to the response
//     status code. `instance` is absent unless set.
//
//   - `errors` is a huma EXTENSION, not an RFC 9457 member: an array of
//     {message, location, value} objects. RFC 9457 section 3.2 explicitly
//     permits extension members, so this is conformant. Request validation
//     failures populate it, and `location` is a path-like string beginning
//     with path, query, header, or body ("query.limit", "body.items[3].tags").
//     Validation failures are 422 Unprocessable Entity, not 400.
//
//   - A 404 from the ServeMux itself -- a path with no route at all -- is
//     stdlib plain text, NOT problem+json. Only huma-routed requests and code
//     paths that go through [WriteProblem] produce the problem shape. The
//     alternative, a catch-all route on the public mux, would swallow genuine
//     routing mistakes.
//
// Nothing here mutates huma's package-level NewError hook. A library that
// rewrites another library's globals at construction time is a landmine for
// any process that builds two APIs.

// ProblemOptions carries the RFC 9457 members huma leaves empty by default.
// The zero value is fine; every field is optional.
type ProblemOptions struct {
	// Type is a URI reference identifying the problem TYPE, and it should
	// resolve to human-readable documentation. Absent means "about:blank",
	// i.e. "the problem is exactly the HTTP status code".
	//
	// Give a stable URI to any error a client is expected to branch on. It is
	// the only member a client may switch behaviour against: title and detail
	// are prose and may be reworded at any time.
	Type string

	// Title overrides the default, which is http.StatusText(status). It must
	// NOT change between occurrences of the same problem type.
	Title string

	// Instance is a URI reference identifying this specific occurrence -- a
	// log URL or a correlation id. Never put a user-supplied value here
	// unescaped.
	Instance string
}

// Problem returns an RFC 9457 error to return from a huma handler. Return it,
// do not write it: huma sets the status code, negotiates the content type, and
// marshals it.
//
//	return nil, httpapi.Problem(http.StatusNotFound, "no widget with that id")
//
// huma's own constructors (huma.Error404NotFound and friends) produce the same
// shape and are equally fine; this exists so a handler does not have to import
// huma just to fail, and so [Detail] has a natural companion.
func Problem(status int, detail string, errs ...error) huma.StatusError {
	return huma.NewError(status, detail, errs...)
}

// ProblemWithOptions is [Problem] plus the members huma omits: a stable type
// URI, an overridden title, and an occurrence instance.
func ProblemWithOptions(status int, detail string, opts ProblemOptions, errs ...error) huma.StatusError {
	err := huma.NewError(status, detail, errs...)
	model, ok := err.(*huma.ErrorModel)
	if !ok {
		// Only reachable if a service has replaced huma.NewError globally, in
		// which case its own shape wins and there is nothing to enrich.
		return err
	}
	if opts.Type != "" {
		model.Type = opts.Type
	}
	if opts.Title != "" {
		model.Title = opts.Title
	}
	if opts.Instance != "" {
		model.Instance = opts.Instance
	}
	return model
}

// Detail builds one entry for a problem's `errors` array: what was wrong,
// where, and the offending value.
//
// location is a path-like string beginning with path, query, header, or body,
// matching what huma's own validator emits ("body.items[3].tags").
//
// value is echoed back to the client, so pass the value the client SENT. Never
// pass a value read from the store, a secret, or anything the caller did not
// already have.
func Detail(message, location string, value any) error {
	return &huma.ErrorDetail{Message: message, Location: location, Value: value}
}

// WriteProblem writes an RFC 9457 problem to a plain http.ResponseWriter, for
// the code paths that never reach huma: middleware that rejects a request
// before routing, and any hand-written handler on the public listener.
//
// It produces byte-for-byte the same wire shape as a huma-returned error, so
// clients need exactly one error parser. Two error formats on one listener is
// how a fleet ends up with two.
//
// It must be called before anything else writes to w.
func WriteProblem(w http.ResponseWriter, status int, detail string, opts ProblemOptions, errs ...error) error {
	model, ok := ProblemWithOptions(status, detail, opts, errs...).(*huma.ErrorModel)
	if !ok {
		model = &huma.ErrorModel{Status: status, Title: http.StatusText(status), Detail: detail}
	}
	if model.Title == "" {
		model.Title = http.StatusText(status)
	}
	model.Status = status

	body, err := json.Marshal(model)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}
