# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.2] - 2026-08-02

### Security

- **Bumped `google.golang.org/grpc` v1.81.1 -> v1.82.1** for GO-2026-6061.
  The dependency arrives via the OTLP trace exporter, so every service built on
  the kit inherited it. `govulncheck` now reports no vulnerabilities.

### Fixed

- **`make lint` assumed `golangci-lint` was on PATH.** It is not on a stock CI
  runner, so this repository's GitHub Actions workflow was red from its first
  commit -- `make: golangci-lint: No such file or directory` -- while every
  local run looked green, because this machine has the binary installed. The
  target now prefers a binary on PATH and falls back to a pinned
  `go run` invocation, matching go-service-template. Both CIs call `make lint`,
  so the resolution belongs in one place.
- `govulncheck` was invoked as `@latest`, which makes the gate
  non-reproducible: a build could start failing because a tool moved rather
  than because the code changed. Pinned to v1.6.0.

## [0.1.1] - 2026-08-02

### Fixed

- **`go.mod` declared `go 1.26.5`, which the standard `golang:1.26` CI image
  cannot satisfy.** That image ships Go 1.26.4, and with `GOTOOLCHAIN=local` the
  build fails outright rather than upgrading:
  `go: go.mod requires go >= 1.26.5 (running go 1.26.4)`. v0.1.0 was therefore
  unbuildable in the homelab pipeline. The directive is now `go 1.26.0` -- the
  language version, not whichever patch release happened to be on the machine
  that ran `go mod init`.

  Raising `GOTOOLCHAIN` instead would have been the wrong fix: it makes every
  build download a Go toolchain, and this build path is behind a residential
  uplink where that is exactly the kind of pull that hangs a pipeline.

  Found by the first real pipeline run of a service generated from
  go-service-template. No API change; v0.1.0 and v0.1.1 are source-identical
  apart from the directive.

## [0.1.0] - 2026-08-01

First release. Six packages, all tested; `go vet`, `go test -race`, and
`golangci-lint` are clean across the module.

### Added

- **`config`** -- env-driven configuration via `Load[T]()`. Validation errors are
  aggregated with `errors.Join`, so N bad fields produce N reported errors rather
  than only the first. Walks nested structs, so a service config can embed
  `obs.Config` and load everything in one call. Env vars whose names contain
  SECRET/PASSWORD/TOKEN/KEY/CREDENTIAL never echo their value into error text.
  Supported field types: `string`, `int`, `int64`, `bool`, `time.Duration`,
  `[]string`.

- **`obs`** -- OpenTelemetry wiring for all three signals behind one `Setup` and
  one `Shutdown`. Traces export over OTLP/gRPC and are a no-op unless
  `OTEL_EXPORTER_OTLP_ENDPOINT` is set. Metrics go through the Prometheus
  exporter and include Go runtime instrumentation plus a build-info series.
  Logs are `slog` JSON on stdout with `trace_id`/`span_id` injected from context.
  `Redact` and `DefaultRedactKeys` support a logging-hygiene policy. The
  `semconv` import is pinned in exactly one file so an OTel bump is a one-line
  change.

- **`httpapi`** -- huma v2 over stdlib `net/http`, with OpenAPI 3.1 derived from
  Go types so the spec cannot drift from the code. Two listeners: a public API
  port, and an admin port carrying health, metrics, and pprof. Server timeouts
  are mandatory -- a timeout-less server is unconstructable. Includes
  `Readiness` (registered dependency checks, shared with `lifecycle`), RFC 9457
  problem+json helpers, and a cursor-pagination envelope.

- **`lifecycle`** -- graceful shutdown in the order that actually works on
  Kubernetes: readiness flips to 503 immediately, the servers keep serving
  through a propagation delay while endpoints are withdrawn, then in-flight
  requests drain, and only then is telemetry flushed. Solved in-process because
  distroless images have no shell and `preStop: exec` is unavailable.
  `TerminationGracePeriodSeconds()` derives the value a chart must set.

- **`outbound`** -- the egress trust boundary. Rate limited by default,
  identifying User-Agent, honors `Retry-After`, exponential backoff with jitter,
  and mandatory timeouts. An SSRF guard resolves the host, refuses if any answer
  is private/loopback/link-local/CGNAT/ULA, and dials the checked IP literal so
  there is no check-to-connect window. Auth-bearing headers are stripped when a
  redirect changes origin, which Go's stdlib does not do for custom headers.

- **`storekit`** -- a generic contract-test harness that any store adapter must
  satisfy, parameterized over the service's own types. Pins the semantics
  adapters would otherwise diverge on: a missing key is not an error, `Put` is an
  upsert, `Delete` is idempotent, and `List` order must be deterministic. `Ping`
  is part of the contract so `/readyz` can be a real dependency check.

### Notes

- The docs UI is a dependency-free embedded renderer, not Stoplight Elements.
  Every huma built-in renderer hardcodes a `unpkg.com` script tag, and vendoring
  megabytes of third-party minified JS into a library -- and therefore into every
  binary built on it -- was the worse trade. The page works air-gapped under
  `default-src 'none'`.
- Verified behavior of huma v2.39.0 that differs from common assumption:
  validation failures are **422**, not 400; the problem+json `type` member is
  **absent** by default despite the OpenAPI schema advertising `about:blank`; and
  an unrouted 404 is stdlib plain text rather than problem+json.
- `otelhttp` emits **no request-count metric**. Rate and error rate come from
  `http_server_request_duration_seconds_count`.
- Go runtime metrics are OTel-namespaced and are **not** the classic collector
  names: `go_goroutine_count`, not `go_goroutines`; no `go_memstats_*`; and no
  GC-pause metric at all in the current instrumentation.

### Not included

Database adapters (per-service; a SQLite adapter must use a pure-Go driver such
as `modernc.org/sqlite` to stay CGO-free), authentication middleware beyond a
reserved slot, and an MCP surface.

[0.1.2]: https://github.com/leftathome/go-service-kit/releases/tag/v0.1.2
[0.1.1]: https://github.com/leftathome/go-service-kit/releases/tag/v0.1.1
[0.1.0]: https://github.com/leftathome/go-service-kit/releases/tag/v0.1.0
