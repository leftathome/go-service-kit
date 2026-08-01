package httpapi

import (
	"embed"
	"net/http"
	"strings"
)

// Documentation UI, self-hosted.
//
// # Why not Stoplight Elements
//
// huma's built-in docs renderers -- Stoplight Elements, Scalar, Swagger UI --
// all load their bundle from unpkg.com. In an air-gapped cluster, or behind a
// default-src 'none' CSP, or simply on a node with no egress, that is a blank
// page plus an outbound connection attempt. So the built-in route is disabled
// (huma.Config.DocsPath = "") and this package serves its own.
//
// Vendoring Elements instead would mean committing several megabytes of
// third-party minified JavaScript into a LIBRARY module, embedding it in every
// service binary built on the kit, and inheriting its CVE feed. The renderer
// here is a few kilobytes of dependency-free JavaScript that reads the same
// /openapi.json. It is deliberately less pretty. If a service wants a richer
// UI, it can serve one from its own assets on API.Mux, self-hosted, on the
// same terms.
//
// The page renders the document; it does not send requests to the API. A "try
// it" console served by the API itself, same origin, is a CSRF primitive.

//go:embed assets/docs.html assets/docs.css assets/docs.js
var docsFS embed.FS

// Documentation paths on the PUBLIC listener. They exist only when
// Options.DocsEnabled is true.
const (
	DocsPath        = "/docs"
	OpenAPIJSONPath = "/openapi.json"
	OpenAPIYAMLPath = "/openapi.yaml"
)

// DocsAssetPaths are the extra files the docs page loads. Every one is served
// by this process; the page references nothing else. The test that walks this
// list is what keeps a CDN reference from creeping back in.
var DocsAssetPaths = []string{
	"/docs/assets/docs.css",
	"/docs/assets/docs.js",
}

// docsCSP is as tight as a page that fetches one same-origin JSON document can
// be. No inline script, no inline style, no external origin of any kind.
const docsCSP = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"connect-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

func registerDocs(mux *http.ServeMux, _ string) {
	page := mustAsset("assets/docs.html")

	mux.Handle("GET "+DocsPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", docsCSP)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(page)
	}))

	for _, p := range DocsAssetPaths {
		name := "assets/" + p[strings.LastIndexByte(p, '/')+1:]
		body := mustAsset(name)
		ct := "text/plain; charset=utf-8"
		switch {
		case strings.HasSuffix(name, ".css"):
			ct = "text/css; charset=utf-8"
		case strings.HasSuffix(name, ".js"):
			ct = "text/javascript; charset=utf-8"
		}
		mux.Handle("GET "+p, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_, _ = w.Write(body)
		}))
	}
}

// mustAsset panics on a missing embedded file. The only way to reach it is a
// build that lost a go:embed target, which must fail loudly at startup rather
// than serve an empty page.
func mustAsset(name string) []byte {
	b, err := docsFS.ReadFile(name)
	if err != nil {
		panic("httpapi: missing embedded docs asset " + name + ": " + err.Error())
	}
	return b
}
